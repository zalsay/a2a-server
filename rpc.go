package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// A2A JSON-RPC error codes (spec-reserved range), mirroring the Python server.
const (
	errTaskNotFound       = -32001
	errTaskNotCancelable  = -32002
	errParse              = -32700
	errInvalidRequest     = -32600
	errMethodNotFound     = -32601
	errInvalidParams      = -32602
)

func rpcError(code int, message string) map[string]interface{} {
	return map[string]interface{}{"code": code, "message": message}
}

func agentCard(cfg config) map[string]interface{} {
	return map[string]interface{}{
		"name": "竹子",
		"description": "竹子 — 个人 AI 助手，可通过 A2A 协议接收自然语言任务并在自己的环境中执行" +
			"（文件读写、命令执行、联网调研、文档处理等），执行结果通过任务接口返回。",
		"version":         "1.0.0",
		"protocolVersion": "0.3.0",
		"url":             cfg.PublicURL,
		"capabilities": map[string]interface{}{
			"streaming": true, "pushNotifications": false, "stateTransitionHistory": true,
		},
		"authentication":  map[string]interface{}{"schemes": []string{"Bearer"}},
		"securitySchemes": map[string]interface{}{"bearerAuth": map[string]interface{}{"type": "http", "scheme": "bearer"}},
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"skills": []map[string]interface{}{
			{
				"id": "task-execution", "name": "通用任务执行",
				"description": "执行自然语言描述的任务：文件读写、命令执行、文档生成、数据处理，" +
					"并在任务记录中返回文字结果与文件制品。",
				"tags": []string{"general", "shell", "files", "documents"},
			},
			{
				"id": "research", "name": "联网调研",
				"description": "联网搜索、查阅公开资料并整理成结论，附来源说明。",
				"tags": []string{"research", "web"},
			},
		},
	}
}

type rpcOutcome struct {
	resp    map[string]interface{} // full JSON-RPC response; nil when streaming
	sseTask *Task                  // set for message/stream
}

// handleRPC mirrors the Python handle_rpc. reqID is echoed verbatim.
func handleRPC(db *sql.DB, payload map[string]interface{}, reqID interface{}) (*rpcOutcome, error) {
	paramsRaw, ok := payload["params"]
	var params map[string]interface{}
	if paramsRaw == nil {
		params = map[string]interface{}{}
	} else if params, ok = paramsRaw.(map[string]interface{}); !ok {
		return &rpcOutcome{resp: map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID,
			"error": rpcError(errInvalidParams, "params must be an object"),
		}}, nil
	}
	method, _ := payload["method"].(string)

	switch method {
	case "message/send", "message/stream":
		msg, errStr := validateMessage(params["message"])
		if errStr != "" {
			return &rpcOutcome{resp: map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID,
				"error": rpcError(errInvalidParams, errStr),
			}}, nil
		}
		var meta json.RawMessage
		if v, ok := params["metadata"]; ok && v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			meta = b
		}
		task, err := createOrResumeTask(db, msg, meta)
		if err != nil {
			return nil, err
		}
		if method == "message/stream" {
			return &rpcOutcome{sseTask: task}, nil
		}
		return &rpcOutcome{resp: map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID, "result": task,
		}}, nil

	case "tasks/get":
		id, _ := params["id"].(string)
		task, err := getTask(db, id)
		if err != nil {
			return nil, err
		}
		if task == nil {
			return &rpcOutcome{resp: map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID,
				"error": rpcError(errTaskNotFound, "task not found"),
			}}, nil
		}
		return &rpcOutcome{resp: map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID, "result": task,
		}}, nil

	case "tasks/cancel":
		id, _ := params["id"].(string)
		task, err := getTask(db, id)
		if err != nil {
			return nil, err
		}
		if task == nil {
			return &rpcOutcome{resp: map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID,
				"error": rpcError(errTaskNotFound, "task not found"),
			}}, nil
		}
		if terminalStates[task.Status.State] {
			return &rpcOutcome{resp: map[string]interface{}{
				"jsonrpc": "2.0", "id": reqID,
				"error": rpcError(errTaskNotCancelable, "task already in a terminal state"),
			}}, nil
		}
		if err := cancelTask(db, id); err != nil {
			return nil, err
		}
		updated, err := getTask(db, id)
		if err != nil {
			return nil, err
		}
		return &rpcOutcome{resp: map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID, "result": updated,
		}}, nil

	case "tasks/list":
		state, _ := params["state"].(string)
		limit := 50
		if v, ok := params["limit"]; ok {
			switch n := v.(type) {
			case json.Number:
				if i, err := n.Int64(); err == nil {
					limit = int(i)
				}
			case float64:
				limit = int(n)
			case string:
				fmt.Sscanf(n, "%d", &limit)
			}
		}
		tasks, err := listTasks(db, state, limit)
		if err != nil {
			return nil, err
		}
		return &rpcOutcome{resp: map[string]interface{}{
			"jsonrpc": "2.0", "id": reqID,
			"result": map[string]interface{}{"tasks": tasks},
		}}, nil
	}

	return &rpcOutcome{resp: map[string]interface{}{
		"jsonrpc": "2.0", "id": reqID,
		"error": rpcError(errMethodNotFound, fmt.Sprintf("method not found: %s", method)),
	}}, nil
}
