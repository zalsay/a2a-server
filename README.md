# a2a-server (Go)

Go implementation of 竹子's A2A server: an agent-to-agent task queue over
JSON-RPC 2.0, ported from the Python implementation (`server.py`).

The server itself never executes tasks; it only queues them. A worker agent
claims tasks, executes them, and writes results back through the CLI
subcommands in this same binary.

## Build

```bash
go build -o a2a .
```

Requires CGO (gcc) for the SQLite driver.

## Run

```bash
# Server (defaults from config.json; flags override)
A2A_DIR=~/workspace/a2a ./a2a serve
A2A_DIR=~/workspace/a2a ./a2a serve --host 127.0.0.1 --port 9877

# Worker CLI (same subcommands and output shapes as the Python version)
A2A_DIR=~/workspace/a2a ./a2a list --state submitted
A2A_DIR=~/workspace/a2a ./a2a claim <task-id>
A2A_DIR=~/workspace/a2a ./a2a complete <task-id> --text "result..."
A2A_DIR=~/workspace/a2a ./a2a fail <task-id> --error "reason"
A2A_DIR=~/workspace/a2a ./a2a need-input <task-id> --message "question?"
A2A_DIR=~/workspace/a2a ./a2a cancel <task-id>
A2A_DIR=~/workspace/a2a ./a2a release <task-id>
```

## Configuration

`$A2A_DIR/config.json` (default `~/workspace/a2a/config.json`):

```json
{
  "host": "127.0.0.1",
  "port": 9877,
  "public_url": "http://127.0.0.1:9877/",
  "token": "Bearer token goes here, never commit the real one"
}
```

Copy `config.example.json` to `config.json` and fill in the token.
`config.json` is git-ignored on purpose: it holds the auth token.

## API

- `GET /.well-known/agent-card.json` (and `/.well-known/agent.json`), public
- `GET /health`, public, returns task counts by state
- `POST /` JSON-RPC 2.0, requires `Authorization: Bearer <token>`:
  `message/send`, `message/stream` (SSE), `tasks/get`, `tasks/cancel`,
  `tasks/list` (small documented extension, not core A2A)

Error codes follow the A2A spec range: task not found `-32001`,
not cancelable `-32002`; unauthorized requests return HTTP 401.

## Compatibility with the Python version

- Same SQLite schema (`tasks.db`), same task JSON shapes, same validation
  limits (10 MB body, 200,000 chars per text part, 50 parts), same state
  machine (`submitted → working → completed/failed/canceled`, plus
  `input-required` resumed by a follow-up with the same `contextId`).
- The Python worker CLI, the Python WS client and this binary can share one
  `tasks.db` (WAL mode, 30 s busy timeout).
- Timestamps use the same ISO-8601 UTC shape as Python's
  `datetime.now(timezone.utc).isoformat()`.
- Known minor differences: Go's `mime.TypeByExtension` returns
  `text/plain; charset=utf-8` for `.txt`, the charset suffix is stripped to
  match Python's `mimetypes` output; JSON object key order may differ
  (semantically irrelevant).

## systemd

```ini
[Unit]
Description=A2A server (Go)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/home/hatch/workspace/a2a
Environment=A2A_DIR=/home/hatch/workspace/a2a
ExecStart=/path/to/a2a serve
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
```
