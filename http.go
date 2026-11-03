package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// marshalNoEscape mirrors Python's json.dumps(ensure_ascii=False): UTF-8
// output without Go's default HTML escaping of <, > and &.
func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func sendJSON(w http.ResponseWriter, obj interface{}, status int) {
	body, err := marshalNoEscape(obj)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Server", serverVersion)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func healthCounts(db *sql.DB) map[string]int {
	rows, err := db.Query(`SELECT state, COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		return map[string]int{}
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var c int
		if err := rows.Scan(&state, &c); err == nil {
			counts[state] = c
		}
	}
	return counts
}

func handleGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/agent-card.json", "/.well-known/agent.json":
			sendJSON(w, agentCard(loadConfig()), http.StatusOK)
		case "/health":
			sendJSON(w, map[string]interface{}{
				"status": "ok", "tasks": healthCounts(db),
			}, http.StatusOK)
		default:
			sendJSON(w, map[string]interface{}{"error": "not found"}, http.StatusNotFound)
		}
	}
}

func handlePost(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			sendJSON(w, map[string]interface{}{"error": "not found"}, http.StatusNotFound)
			return
		}
		if r.ContentLength > maxBodyBytes {
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": rpcError(errInvalidParams, "request body too large"),
			}, http.StatusRequestEntityTooLarge)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": rpcError(errParse, "parse error"),
			}, http.StatusBadRequest)
			return
		}
		if len(raw) > maxBodyBytes {
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": rpcError(errInvalidParams, "request body too large"),
			}, http.StatusRequestEntityTooLarge)
			return
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var payload map[string]interface{}
		if err := dec.Decode(&payload); err != nil {
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": rpcError(errParse, "parse error"),
			}, http.StatusBadRequest)
			return
		}
		if payload["jsonrpc"] != "2.0" {
			var id interface{}
			id = payload["id"]
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": id,
				"error": rpcError(errInvalidRequest, "invalid request"),
			}, http.StatusBadRequest)
			return
		}
		token := loadConfig().Token
		if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
			sendJSON(w, map[string]interface{}{
				"jsonrpc": "2.0", "id": payload["id"],
				"error": rpcError(errTaskNotFound, "unauthorized"),
			}, http.StatusUnauthorized)
			return
		}
		out, err := handleRPC(db, payload, payload["id"])
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if out.sseTask != nil {
			streamTask(w, r, db, payload["id"], out.sseTask)
			return
		}
		sendJSON(w, out.resp, http.StatusOK)
	}
}

// streamTask mirrors the Python SSE stream: status updates on change,
// artifact updates as they appear, then the final task object. It polls
// every second and closes with the last known task after one hour.
func streamTask(w http.ResponseWriter, r *http.Request, db *sql.DB, reqID interface{}, task *Task) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Server", serverVersion)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emitFrame := func(obj interface{}) bool {
		data, err := marshalNoEscape(obj)
		if err != nil {
			return false
		}
		frame := append([]byte("data: "), data...)
		frame = append(frame, '\n', '\n')
		if _, err := w.Write(frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	taskID := task.ID
	lastState := ""
	sentArtifacts := 0
	deadline := time.Now().Add(3600 * time.Second)
	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
		cur, err := getTask(db, taskID)
		if err != nil || cur == nil {
			return
		}
		state := cur.Status.State
		if state != lastState {
			if !emitFrame(map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID,
				"result": map[string]interface{}{
					"taskId": taskID, "contextId": cur.ContextID,
					"kind": "status-update", "status": cur.Status,
					"final": terminalStates[state],
				},
			}) {
				return
			}
			lastState = state
		}
		var arts []json.RawMessage
		if err := json.Unmarshal(cur.Artifacts, &arts); err == nil {
			for sentArtifacts < len(arts) {
				if !emitFrame(map[string]interface{}{
					"jsonrpc": "2.0", "id": reqID,
					"result": map[string]interface{}{
						"taskId": taskID, "contextId": cur.ContextID,
						"kind": "artifact-update", "artifact": arts[sentArtifacts],
						"append": false, "lastChunk": true,
					},
				}) {
					return
				}
				sentArtifacts++
			}
		}
		if terminalStates[state] {
			emitFrame(map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID, "result": cur,
			})
			return
		}
		time.Sleep(time.Second)
	}
	if cur, err := getTask(db, taskID); err == nil && cur != nil {
		emitFrame(map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID, "result": cur,
		})
	}
}

func serve(db *sql.DB, host string, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleGet(db)(w, r)
		case http.MethodPost:
			handlePost(db)(w, r)
		default:
			sendJSON(w, map[string]interface{}{"error": "not found"}, http.StatusNotFound)
		}
	})
	addr := fmt.Sprintf("%s:%d", host, port)
	fmt.Fprintf(os.Stderr, "a2a server listening on http://%s/\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fatalErr(err)
	}
}
