package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var terminalStates = map[string]bool{
	"completed": true,
	"failed":    true,
	"canceled":  true,
}

// Task mirrors the Python task_from_row output. History, artifacts and
// metadata are kept as raw JSON so stored content round-trips byte-for-byte.
type Task struct {
	ID        string          `json:"id"`
	ContextID string          `json:"contextId"`
	Status    TaskStatus      `json:"status"`
	Artifacts json.RawMessage `json:"artifacts"`
	History   json.RawMessage `json:"history"`
	Metadata  json.RawMessage `json:"metadata"`
}

type TaskStatus struct {
	State     string          `json:"state"`
	Message   json.RawMessage `json:"message"`
	Timestamp string          `json:"timestamp"`
}

// Message is a cleaned inbound user message (contextId stripped on store).
type Message struct {
	Role      string      `json:"role"`
	Parts     []Part      `json:"parts"`
	MessageID interface{} `json:"messageId"`
	ContextID string      `json:"-"`
}

type Part struct {
	Kind string      `json:"kind"`
	Text string      `json:"text,omitempty"`
	Data interface{} `json:"data,omitempty"`
}

func taskFromRow(id, contextID, state, history, artifacts, metadata, updatedAt string) (*Task, error) {
	var histItems []json.RawMessage
	if err := json.Unmarshal([]byte(history), &histItems); err != nil {
		return nil, err
	}
	var statusMsg json.RawMessage // nil marshals as null
	if state == "input-required" || state == "failed" || state == "completed" {
		for i := len(histItems) - 1; i >= 0; i-- {
			var m struct {
				Role string `json:"role"`
			}
			if err := json.Unmarshal(histItems[i], &m); err == nil && m.Role == "agent" {
				statusMsg = histItems[i]
				break
			}
		}
	}
	return &Task{
		ID:        id,
		ContextID: contextID,
		Status:    TaskStatus{State: state, Message: statusMsg, Timestamp: updatedAt},
		Artifacts: json.RawMessage(artifacts),
		History:   json.RawMessage(history),
		Metadata:  json.RawMessage(metadata),
	}, nil
}

func getTask(db *sql.DB, taskID string) (*Task, error) {
	var id, contextID, state, history, artifacts, metadata, updatedAt string
	err := db.QueryRow(`SELECT id, context_id, state, history, artifacts, metadata,
		updated_at FROM tasks WHERE id = ?`, taskID).Scan(
		&id, &contextID, &state, &history, &artifacts, &metadata, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return taskFromRow(id, contextID, state, history, artifacts, metadata, updatedAt)
}

func listTasks(db *sql.DB, state string, limit int) ([]*Task, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if state != "" {
		rows, err = db.Query(`SELECT id, context_id, state, history, artifacts, metadata,
			updated_at FROM tasks WHERE state = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
			state, limit)
	} else {
		rows, err = db.Query(`SELECT id, context_id, state, history, artifacts, metadata,
			updated_at FROM tasks ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		var id, contextID, st, history, artifacts, metadata, updatedAt string
		if err := rows.Scan(&id, &contextID, &st, &history, &artifacts, &metadata, &updatedAt); err != nil {
			return nil, err
		}
		t, err := taskFromRow(id, contextID, st, history, artifacts, metadata, updatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if out == nil {
		out = []*Task{}
	}
	return out, rows.Err()
}

// validateMessage mirrors the Python validator. It returns the cleaned
// message (contextId kept aside, not part of the stored message).
func validateMessage(raw interface{}) (*Message, string) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, "message must be an object"
	}
	role := "user"
	if v, ok := m["role"]; ok {
		s, ok := v.(string)
		if !ok || s != "user" {
			return nil, "message.role must be 'user'"
		}
		role = s
	}
	partsRaw, ok := m["parts"].([]interface{})
	if !ok || len(partsRaw) == 0 {
		return nil, "message.parts must be a non-empty array"
	}
	if len(partsRaw) > maxParts {
		return nil, fmt.Sprintf("message.parts exceeds %d", maxParts)
	}
	parts := make([]Part, 0, len(partsRaw))
	for _, p := range partsRaw {
		pm, ok := p.(map[string]interface{})
		if !ok {
			return nil, "each part must be an object"
		}
		kind, _ := pm["kind"].(string)
		switch kind {
		case "text":
			text, _ := pm["text"].(string)
			if _, hasText := pm["text"]; hasText {
				if _, ok := pm["text"].(string); !ok {
					return nil, "text part text must be a string"
				}
			}
			if utf8.RuneCountInString(text) > maxTextChars {
				return nil, fmt.Sprintf("text part exceeds %d chars", maxTextChars)
			}
			parts = append(parts, Part{Kind: "text", Text: text})
		case "data":
			data, ok := pm["data"].(map[string]interface{})
			if !ok {
				return nil, "data part data must be an object"
			}
			parts = append(parts, Part{Kind: "data", Data: data})
		default:
			return nil, fmt.Sprintf("unsupported part kind: %q (v1 accepts text, data)", kind)
		}
	}
	msg := &Message{Role: role, Parts: parts}
	if v, ok := m["messageId"]; ok && v != nil {
		msg.MessageID = v
	} else {
		msg.MessageID = newID()
	}
	if v, ok := m["contextId"]; ok {
		if s, ok := v.(string); ok && s != "" {
			msg.ContextID = s
		}
	}
	return msg, ""
}

func (m *Message) storedJSON() ([]byte, error) {
	// The stored message excludes contextId, like the Python version.
	type stored struct {
		Role      string      `json:"role"`
		Parts     []Part      `json:"parts"`
		MessageID interface{} `json:"messageId"`
	}
	return json.Marshal(stored{Role: m.Role, Parts: m.Parts, MessageID: m.MessageID})
}

// createOrResumeTask mirrors the Python logic: a follow-up to an
// input-required task (same contextId) resumes it, otherwise a new task.
func createOrResumeTask(db *sql.DB, msg *Message, metadata json.RawMessage) (*Task, error) {
	if msg.ContextID != "" {
		var id, contextID, state, history, artifacts, meta, updatedAt string
		err := db.QueryRow(`SELECT id, context_id, state, history, artifacts, metadata,
			updated_at FROM tasks WHERE context_id = ? AND state = 'input-required'
			ORDER BY updated_at DESC LIMIT 1`, msg.ContextID).Scan(
			&id, &contextID, &state, &history, &artifacts, &meta, &updatedAt)
		if err == nil {
			var histItems []json.RawMessage
			if err := json.Unmarshal([]byte(history), &histItems); err != nil {
				return nil, err
			}
			stored, err := msg.storedJSON()
			if err != nil {
				return nil, err
			}
			histItems = append(histItems, stored)
			newHist, err := json.Marshal(histItems)
			if err != nil {
				return nil, err
			}
			if _, err := db.Exec(`UPDATE tasks SET state='submitted', history=?, updated_at=?
				WHERE id=?`, string(newHist), nowISO(), id); err != nil {
				return nil, err
			}
			return getTask(db, id)
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	taskID := newID()
	contextID := msg.ContextID
	if contextID == "" {
		contextID = newID()
	}
	stored, err := msg.storedJSON()
	if err != nil {
		return nil, err
	}
	histJSON, err := json.Marshal([]json.RawMessage{stored})
	if err != nil {
		return nil, err
	}
	meta := metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	ts := nowISO()
	if _, err := db.Exec(`INSERT INTO tasks (id, context_id, state, history, artifacts,
		metadata, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, contextID, "submitted", string(histJSON), "[]", string(meta), ts, ts); err != nil {
		return nil, err
	}
	return getTask(db, taskID)
}

func claimTask(db *sql.DB, id string) error {
	res, err := db.Exec(`UPDATE tasks SET state='working', updated_at=?
		WHERE id=? AND state='submitted'`, nowISO(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		t, err := getTask(db, id)
		if err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("cannot claim %s: not found", id)
		}
		return fmt.Errorf("cannot claim %s: state=%s", id, t.Status.State)
	}
	return nil
}

func releaseTask(db *sql.DB, id string) error {
	res, err := db.Exec(`UPDATE tasks SET state='submitted', updated_at=?
		WHERE id=? AND state='working'`, nowISO(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		t, err := getTask(db, id)
		if err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("cannot release %s: not found", id)
		}
		return fmt.Errorf("cannot release %s: state=%s", id, t.Status.State)
	}
	return nil
}

func cancelTask(db *sql.DB, id string) error {
	t, err := getTask(db, id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("task not found: %s", id)
	}
	if terminalStates[t.Status.State] {
		return fmt.Errorf("cannot cancel %s: already %s", id, t.Status.State)
	}
	_, err = db.Exec(`UPDATE tasks SET state='canceled', updated_at=? WHERE id=?`,
		nowISO(), id)
	return err
}

// finishTask implements complete/fail/need-input: allowed only from working
// or submitted, appends a result artifact and an agent history message.
func finishTask(db *sql.DB, id, state, resultText string, artifactFiles []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID, contextID, curState, history, artifacts, meta, updatedAt string
	err = tx.QueryRow(`SELECT id, context_id, state, history, artifacts, metadata,
		updated_at FROM tasks WHERE id = ?`, id).Scan(
		&taskID, &contextID, &curState, &history, &artifacts, &meta, &updatedAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("task not found: %s", id)
	}
	if err != nil {
		return err
	}
	if curState != "working" && curState != "submitted" {
		return fmt.Errorf("cannot set %s to %s: state=%s", id, state, curState)
	}
	type filePart struct {
		Kind string `json:"kind"`
		File struct {
			Name     string `json:"name"`
			MimeType string `json:"mimeType"`
			Bytes    string `json:"bytes"`
		} `json:"file"`
	}
	parts := []interface{}{map[string]interface{}{"kind": "text", "text": resultText}}
	for _, path := range artifactFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if i := strings.Index(mt, ";"); i >= 0 {
			mt = mt[:i]
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		fp := filePart{Kind: "file"}
		fp.File.Name = filepath.Base(path)
		fp.File.MimeType = mt
		fp.File.Bytes = base64.StdEncoding.EncodeToString(data)
		parts = append(parts, fp)
	}
	var artItems []json.RawMessage
	if err := json.Unmarshal([]byte(artifacts), &artItems); err != nil {
		return err
	}
	newArt, err := json.Marshal(map[string]interface{}{
		"artifactId": newID(), "name": "result", "parts": parts,
	})
	if err != nil {
		return err
	}
	artItems = append(artItems, newArt)
	newArts, err := json.Marshal(artItems)
	if err != nil {
		return err
	}
	var histItems []json.RawMessage
	if err := json.Unmarshal([]byte(history), &histItems); err != nil {
		return err
	}
	agentMsg, err := json.Marshal(map[string]interface{}{
		"role": "agent",
		"parts": []map[string]interface{}{
			{"kind": "text", "text": resultText},
		},
		"messageId": newID(),
	})
	if err != nil {
		return err
	}
	histItems = append(histItems, agentMsg)
	newHist, err := json.Marshal(histItems)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET state=?, artifacts=?, history=?, updated_at=?
		WHERE id=?`, state, string(newArts), string(newHist), nowISO(), id); err != nil {
		return err
	}
	return tx.Commit()
}
