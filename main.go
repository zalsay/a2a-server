// Command a2a is a Go port of the Python A2A server (~/workspace/a2a/server.py).
//
// It implements the same Google A2A protocol surface over JSON-RPC 2.0, the
// same SQLite task store (tasks.db, same schema, so the Python worker CLI,
// the WS client and this binary can share one task database), and the same
// worker CLI subcommands. The server itself never executes tasks; it only
// queues them. A worker agent claims tasks and writes results back through
// the CLI subcommands in this same binary.
//
// Usage:
//
//	a2a serve [--host H] [--port P]
//	a2a list [--state S] [--limit N]
//	a2a show <id>
//	a2a claim <id>
//	a2a complete <id> (--text T | --result-file F) [--artifact-file P ...]
//	a2a fail <id> --error E
//	a2a need-input <id> --message M
//	a2a cancel <id>
//	a2a release <id>
//
// Configuration comes from $A2A_DIR/config.json (default ~/workspace/a2a),
// with host/port/public_url defaults matching the Python implementation.
// The Bearer token lives in config.json and is never printed.
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultHost   = "127.0.0.1"
	defaultPort   = 9877
	maxBodyBytes  = 10 * 1024 * 1024
	maxTextChars  = 200000
	maxParts      = 50
	serverVersion = "bamboo-a2a/1.0"
)

// nowISO mimics Python's datetime.now(timezone.utc).isoformat():
// "2026-11-03T06:26:19.330522+00:00" (microseconds omitted when zero).
func nowISO() string {
	t := time.Now().UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	// Clear version/variant bits handling is unnecessary for an opaque id;
	// keep the raw 32 hex chars like Python's uuid4().hex.
	return fmt.Sprintf("%x", b[:])
}

type config struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	PublicURL string `json:"public_url"`
	Token     string `json:"token"`
}

func a2aDir() string {
	if d := os.Getenv("A2A_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(home, "workspace", "a2a")
}

func loadConfig() config {
	cfg := config{Host: defaultHost, Port: defaultPort}
	path := filepath.Join(a2aDir(), "config.json")
	data, err := os.ReadFile(path)
	if err == nil {
		// Keep explicitly set zero values out: decode into a map first so
		// a missing key does not wipe the default.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err == nil {
			if v, ok := raw["host"]; ok {
				json.Unmarshal(v, &cfg.Host)
			}
			if v, ok := raw["port"]; ok {
				json.Unmarshal(v, &cfg.Port)
			}
			if v, ok := raw["public_url"]; ok {
				json.Unmarshal(v, &cfg.PublicURL)
			}
			if v, ok := raw["token"]; ok {
				json.Unmarshal(v, &cfg.Token)
			}
		}
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = fmt.Sprintf("http://%s:%d/", cfg.Host, cfg.Port)
	}
	return cfg
}

func openDB() *sql.DB {
	dir := a2aDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalErr(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "tasks.db")+
		"?_journal_mode=WAL&_busy_timeout=30000")
	if err != nil {
		fatalErr(err)
	}
	// Serialize access: this service is low-traffic and the worker CLI
	// also opens the same database; one connection avoids lock churn.
	db.SetMaxOpenConns(1)
	if err := initDB(db); err != nil {
		fatalErr(err)
	}
	return db
}

func initDB(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		context_id TEXT NOT NULL,
		state TEXT NOT NULL,
		history TEXT NOT NULL DEFAULT '[]',
		artifacts TEXT NOT NULL DEFAULT '[]',
		metadata TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state)`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_context ON tasks(context_id)`)
	return err
}

func fatalErr(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func fatalMsg(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// printJSON mirrors the Python CLI: list/show use 1-space indent,
// claim/complete/fail/need-input/cancel/release use compact JSON.
func printJSON(v interface{}, indent bool) {
	var b []byte
	var err error
	if indent {
		b, err = json.MarshalIndent(v, "", " ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		fatalErr(err)
	}
	fmt.Println(string(b))
}

func main() {
	if len(os.Args) < 2 {
		fatalMsg("usage: a2a <serve|list|show|claim|complete|fail|need-input|cancel|release> ...")
	}
	db := openDB()
	defer db.Close()
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "serve":
		host, port := serveFlags(args)
		cfg := loadConfig()
		if host == "" {
			host = cfg.Host
		}
		if port == 0 {
			port = cfg.Port
		}
		serve(db, host, port)
	case "list":
		state, limit := listFlags(args)
		tasks, err := listTasks(db, state, limit)
		if err != nil {
			fatalErr(err)
		}
		out := make([]map[string]interface{}, 0, len(tasks))
		for _, t := range tasks {
			out = append(out, map[string]interface{}{
				"id": t.ID, "contextId": t.ContextID,
				"state": t.Status.State, "created": t.Status.Timestamp,
			})
		}
		printJSON(out, true)
	case "show":
		id := needArg(args, "show <id>")
		t, err := getTask(db, id)
		if err != nil {
			fatalErr(err)
		}
		if t == nil {
			fatalMsg("task not found: %s", id)
		}
		printJSON(t, true)
	case "claim":
		id := needArg(args, "claim <id>")
		if err := claimTask(db, id); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	case "complete":
		id, text, resultFile, artifacts := completeFlags(args)
		if resultFile != "" {
			data, err := os.ReadFile(resultFile)
			if err != nil {
				fatalErr(err)
			}
			text = string(data)
		} else if text == "" {
			fatalMsg("complete needs --text or --result-file")
		}
		if err := finishTask(db, id, "completed", text, artifacts); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	case "fail":
		id, errText := failFlags(args)
		if err := finishTask(db, id, "failed", errText, nil); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	case "need-input":
		id, msg := needInputFlags(args)
		if err := finishTask(db, id, "input-required", msg, nil); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	case "cancel":
		id := needArg(args, "cancel <id>")
		if err := cancelTask(db, id); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	case "release":
		id := needArg(args, "release <id>")
		if err := releaseTask(db, id); err != nil {
			fatalMsg("%s", err)
		}
		t, _ := getTask(db, id)
		printJSON(t, false)
	default:
		fatalMsg("unknown command: %s", cmd)
	}
}

func needArg(args []string, usage string) string {
	if len(args) < 1 {
		fatalMsg("usage: a2a %s", usage)
	}
	return args[0]
}

// --- minimal flag parsing (flag package would reorder/rename semantics) ---

func serveFlags(args []string) (host string, port int) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--host" && i+1 < len(args):
			host = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--host="):
			host = strings.TrimPrefix(args[i], "--host=")
		case args[i] == "--port" && i+1 < len(args):
			fmt.Sscanf(args[i+1], "%d", &port)
			i++
		case strings.HasPrefix(args[i], "--port="):
			fmt.Sscanf(strings.TrimPrefix(args[i], "--port="), "%d", &port)
		}
	}
	return host, port
}

func listFlags(args []string) (state string, limit int) {
	limit = 50
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--state" && i+1 < len(args):
			state = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--state="):
			state = strings.TrimPrefix(args[i], "--state=")
		case args[i] == "--limit" && i+1 < len(args):
			fmt.Sscanf(args[i+1], "%d", &limit)
			i++
		case strings.HasPrefix(args[i], "--limit="):
			fmt.Sscanf(strings.TrimPrefix(args[i], "--limit="), "%d", &limit)
		}
	}
	return state, limit
}

func completeFlags(args []string) (id, text, resultFile string, artifacts []string) {
	if len(args) < 1 {
		fatalMsg("usage: a2a complete <id> (--text T | --result-file F) [--artifact-file P ...]")
	}
	id = args[0]
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--text" && i+1 < len(args):
			text = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--text="):
			text = strings.TrimPrefix(args[i], "--text=")
		case args[i] == "--result-file" && i+1 < len(args):
			resultFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--result-file="):
			resultFile = strings.TrimPrefix(args[i], "--result-file=")
		case args[i] == "--artifact-file" && i+1 < len(args):
			artifacts = append(artifacts, args[i+1])
			i++
		case strings.HasPrefix(args[i], "--artifact-file="):
			artifacts = append(artifacts, strings.TrimPrefix(args[i], "--artifact-file="))
		}
	}
	return id, text, resultFile, artifacts
}

func failFlags(args []string) (id, errText string) {
	if len(args) < 1 {
		fatalMsg("usage: a2a fail <id> --error E")
	}
	id = args[0]
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--error" && i+1 < len(args):
			errText = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--error="):
			errText = strings.TrimPrefix(args[i], "--error=")
		}
	}
	if errText == "" {
		fatalMsg("fail requires --error")
	}
	return id, errText
}

func needInputFlags(args []string) (id, msg string) {
	if len(args) < 1 {
		fatalMsg("usage: a2a need-input <id> --message M")
	}
	id = args[0]
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--message" && i+1 < len(args):
			msg = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message="):
			msg = strings.TrimPrefix(args[i], "--message=")
		}
	}
	if msg == "" {
		fatalMsg("need-input requires --message")
	}
	return id, msg
}
