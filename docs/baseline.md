# Ledgerboard — Build & Test Baseline

**Status:** Phase 0 (setup only). No product features added.
**Author:** engineer
**Date:** 2026-10-30
**Repo:** `/home/team/shared/ledgerboard` — clone of `github.com/rgracey/kanban-mcp`

---

## 1. Pinned commit

```
commit c6938a2e6e0f3b2aca6cebf4afd93babd94d8317
Date:   2026-02-25 00:38:27 +1100
Subject: chore: delete unused docs
branch:  main (origin/main)
```

Everything in this document was produced from **c6938a2e**. No fork or PR was created (per brief).

## 2. Toolchain

Go was **not** present on the machine. Installed:

```sh
curl -sSL -o /tmp/go.tgz https://go.dev/dl/go1.27.3.linux-amd64.tar.gz
tar -C /usr/local -xzf /tmp/go.tgz
# persisted in /etc/profile.d/go.sh
```

```
$ go version
go version go1.27.3 linux/amd64
```

`git version 2.43.0`. Git identity was unset; set to `Ledgerboard Engineer <engineer@ledgerboard.local>`.

### Disk gotcha (important for everyone)

`/home` (where the shared tree lives) is a **300 MB virtiofs with ~149 MB free**.
`/` (overlay) has ~3.1 GB free. Therefore the Go toolchain, module cache and build
cache all live **outside** `/home`, configured in `/etc/profile.d/go.sh`:

```sh
export PATH=/usr/local/go/bin:$PATH
export GOPATH=/var/gopath
export GOMODCACHE=/var/gopath/pkg/mod   # 418 MB after first build
export GOCACHE=/var/gocache             # ~200 MB
```

Only the repo itself (source + one 19 MB binary) sits in `/home`. Anyone adding
frontend `node_modules` or a second vendor tree into `/home` will fill the disk.

## 3. Build

```
$ cd /home/team/shared/ledgerboard && go build ./...
real    0m38.981s        # includes first-run module download
BUILD_EXIT=0
```

Clean build, no warnings. Binary:

```
$ go build -o kanban-mcp .
-rwxr-xr-x 1 root root 19M kanban-mcp
```

Dependencies are modest and all pure Go (no cgo): `modernc.org/sqlite`,
`golang-migrate/migrate/v4`, `mark3labs/mcp-go v0.44.0`, `go-chi/chi/v5`,
`stretchr/testify`. `go.mod` declares `go 1.25.5`; it builds fine on 1.27.3.

## 4. Tests

```
$ go test ./...
?       github.com/rgracey/kanban-mcp          [no test files]
?       github.com/rgracey/kanban-mcp/frontend [no test files]
ok      github.com/rgracey/kanban-mcp/internal/api      0.064s
ok      github.com/rgracey/kanban-mcp/internal/config   0.003s
ok      github.com/rgracey/kanban-mcp/internal/db       0.027s
ok      github.com/rgracey/kanban-mcp/internal/mcp      0.054s
?       github.com/rgracey/kanban-mcp/internal/models   [no test files]
ok      github.com/rgracey/kanban-mcp/internal/store    0.223s
TEST_EXIT=0
```

Verbose run: **31 test functions, 31 PASS, 0 FAIL, 0 SKIP.** Nothing was skipped
or papered over — the suite is genuinely green.

## 5. Running the server

```sh
cd /home/team/shared/ledgerboard
go build -o kanban-mcp .
./kanban-mcp -port 8099 -db /tmp/scratch/ledgerboard.db -mcp-transport http -log-level debug
```

| Flag | Env | Default |
|---|---|---|
| `--port` | `KANBAN_PORT` | `8080` |
| `--db` | `KANBAN_DB` | `kanban.db` (in CWD) |
| `--mcp-transport` | `KANBAN_MCP_TRANSPORT` | `stdio` (`stdio`\|`http`\|`both`) |
| `--log-level` | `KANBAN_LOG_LEVEL` | `info` |

**Where the DB lives:** it is simply the `--db` path, default `./kanban.db`
relative to the working directory. `.gitignore` already excludes `*.db`,
`*.db*` and the `kanban-mcp` binary, so a default run does not dirty the tree.
Migrations are embedded and run automatically on open (creates
`schema_migrations` + 7 tables).

One process serves everything on one port: **UI + REST + MCP**:
- UI: `/`
- REST: `/api/v1/...`
- MCP: `/mcp` (Streamable HTTP), when transport is `http` or `both`

Startup log (real):

```json
{"level":"INFO","msg":"starting kanban-mcp","version":"dev"}
{"level":"INFO","msg":"database opened","path":"/tmp/scratch/ledgerboard.db"}
{"level":"INFO","msg":"MCP HTTP handler mounted","path":"/mcp","port":8099}
{"level":"INFO","msg":"HTTP server listening","port":8099}
```

`/tmp/scratch/ledgerboard.db` = 86016 bytes after migrations.

## 6. What upstream already supports

### Data model (migration `0001_init.up.sql`, verified against the live DB)

```
boards(id, name, description, created_at, updated_at)
epics(id, board_id→boards, title, description, created_at, updated_at)
tickets(id, board_id→boards, epic_id→epics, title, description,
        status, priority, assignee, resolution, created_at, updated_at)
ticket_events(id, ticket_id→tickets, type, actor, payload, created_at)
notes(id, ticket_id→tickets, body, created_at, updated_at)
tasks(id, ticket_id→tickets, title, done, position, created_at, updated_at)
ticket_relations(from_ticket_id, to_ticket_id, kind, created_at)  -- "blocks"
```

`PRAGMA table_info(tickets)` confirms the full column list ends at
`updated_at` — **there are no lease, claim, holder, expiry or version columns.**

Enums: status `todo|in_progress|done|blocked`; priority
`low|medium|high|critical`. IDs are UUIDv4 hex. Timestamps are RFC3339 **TEXT,
second precision** (`time.RFC3339` round-trips through SQLite and truncates
sub-second precision — relevant later for lease tests).

### MCP tools — 6, action-dispatch style

Confirmed live via `tools/list`:

| Tool | Actions |
|---|---|
| `board` | list, get, create, update, delete, summary, context, ready |
| `epic` | list, get, create, update, delete |
| `ticket` | list, get, create, bulk_create, update, delete, history |
| `task` | list, create, bulk_create, update, delete |
| `note` | (agent scratchpad: list, create, update, delete) |
| `relation` | list, add, delete |

Both transports work: Streamable HTTP at `/mcp` **and** stdio
(`-mcp-transport stdio`, JSON-RPC over stdin/stdout). Both were exercised.

### REST — yes, it exists

`/api/v1`, chi router, full CRUD for boards/epics/tickets/tasks/notes/relations,
plus `/boards/{id}/summary`, `/boards/{id}/context`, `/boards/{id}/ready`, and an
**SSE stream at `/api/v1/events`** for live board updates. Documented in README.

### Locking / assignment semantics — the key finding

**There are none.** `assignee` is a free-text `TEXT NOT NULL DEFAULT ''` column
with no constraint, no holder identity, no token, no expiry.

- `UpdateTicket` (`internal/store/ticket.go:180`) does
  `GetTicket → UPDATE → GetTicket → event insert` with **no transaction and no
  optimistic concurrency** (no version column, no `WHERE assignee = ?` guard).
  Only `BulkCreateTickets` and `UpdateEpic` use `BeginTx`.
- There is no `SELECT ... FOR UPDATE` equivalent, no `BEGIN IMMEDIATE`, no mutex.
- `CREATE` is safe (single statement). Read-modify-write is not.

### Frontend — a real, working Kanban UI already exists

Svelte app in `frontend/`, built assets committed to `frontend/dist/` and
`go:embed`-ed into the binary (no Node build needed to run). Verified in a
browser: 4 columns (TO DO / IN PROGRESS / BLOCKED / DONE), drag-and-drop,
board switcher, epic filter, search, priority sort, assignee filter, dark mode,
live SSE updates, and a ticket detail panel exposing status / priority / epic /
**assignee (free text)** / resolution (commit SHA, PR URL, notes) / description /
tasks / notes.

**A human can already reassign a ticket from the UI.** Verified: typed
`human-engineer` into the ASSIGNEE field, saved, and confirmed via REST that it
persisted. So "override" exists as plain field editing — what's missing is
anything to override.

## 7. Evidence — real interactions

### REST create + list (works)

```
$ curl -X POST .../api/v1/boards -d '{"name":"Ledgerboard Baseline",...}'
{"id":"76e86f355b914ef9943fcd2cf61aa615","name":"Ledgerboard Baseline",...}

$ curl -X POST .../api/v1/boards/$BID/tickets -d '{"title":"Baseline ticket via REST",...}'
{"id":"ce291ce46f3c4f3d94108cd4101cb0d1","status":"todo","priority":"high","assignee":"",...}
```

### MCP initialize against `/mcp`

```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18",
 "capabilities":{"tools":{}},"serverInfo":{"name":"kanban-mcp","version":"1.0.0"}}}
```
Session id returned in `Mcp-Session-Id`; `notifications/initialized` then
`tools/list` → 6 tools.

### MCP create + list (works — the required proof)

```
tools/call ticket{action:create, board_id:…, title:"Created purely over MCP"}
→ {"id":"503f84c73d5f491c84eb696246719a91","status":"todo","priority":"critical","assignee":""}

tools/call ticket{action:list, board_id:…}
→ count: 2
  503f84c7  status=todo  assignee=''  Created purely over MCP
  ce291ce4  status=todo  assignee=''  Baseline ticket via REST
MCP-created ticket present in list: True
```

### Race probe — two agents, same ticket (the gap, demonstrated)

Two concurrent MCP `ticket{action:update, assignee:…}` calls on one ticket:

```
results: {"agent-B": "agent-B", "agent-A": "agent-A"}
final assignee in DB: 'agent-A'
=> both writers "succeeded": True
```

**Both agents were told they won.** One silently lost its work. This is exactly
the collision Ledgerboard exists to prevent.

### Concurrency under load

| Experiment | Result |
|---|---|
| 30 concurrent ticket **creates** | 30/30 `201 OK`, all persisted |
| 20 concurrent **updates to one ticket** via REST | **14 × 200 OK, 6 × HTTP 500** |
| 40 concurrent raw `UPDATE tickets` (same DSN/pragmas) | **21 ok, 19 × `database is locked (5) (SQLITE_BUSY)`** |

The 500s are `SQLITE_BUSY` surfacing through `internal/api` as a generic
`{"error": "internal server error"}` — **and nothing is written to the server
log**, which makes this invisible in operation. Reproduced independently:

```
ERR: database is locked (5) (SQLITE_BUSY)
```

Cause: `internal/db/db.go` enables `journal_mode=WAL` and `foreign_keys=ON` but
sets **no `_busy_timeout`** and never calls `db.SetMaxOpenConns(...)`. Concurrent
writers therefore fail immediately instead of waiting.

Also confirmed: unknown JSON fields are **silently dropped**, not rejected.
`PUT /tickets/:id {"lease_expires_at":"…"}` returns `200 OK` with the ticket
unchanged. The store's own `invalid field key` validation is unreachable because
the HTTP handler only copies known keys out of the raw map.

## 8. Gap list vs. what Ledgerboard needs

| # | Need | Upstream reality | Gap |
|---|---|---|---|
| 1 | **Atomic claim** | `assignee` is free text; read-modify-write, no tx, no version guard | **None of it.** Needs a conditional `UPDATE … WHERE` (or `BEGIN IMMEDIATE`) that returns a definitive win/lose, identical over REST and MCP. |
| 2 | **Expiring lease** | No columns, no concept. No scheduler/janitor. | **None of it.** Needs `lease_holder`, `lease_expires_at`, `lease_token` (or a `leases` table), plus a reaper that returns expired tickets to the board. Second-precision timestamps mean sub-second leases will be flaky in tests. |
| 3 | **Renewal / release** | Nothing. | **None of it.** A holder must be able to extend and to release early; a human must be able to force-release. |
| 4 | **Structured handoff** | Partial building blocks only: `resolution{commit_sha, pr_url, notes, resolved_at}` and free-text `notes` on done. No handoff-on-release, no summary contract, no "next agent" state. | Needs a defined handoff payload written atomically with release/expiry. |
| 5 | **Live UI: who holds it + when it expires + override** | UI exists and is good: columns, DnD, SSE live updates, detail panel with editable assignee + resolution. But it shows `assignee` as a plain string and there is nothing to show about leases. | Needs holder + countdown + release/reassign controls; the SSE plumbing and detail panel are already there to extend. This is the **cheapest** gap. |
| 6 | **REST + MCP parity** | REST is complete; MCP exposes the same operations. | Adding claim/renew/release needs to land in both, with the same failure semantics. |
| 7 | **Concurrency correctness at the SQLite level** | WAL on, but no busy timeout, no pool cap; concurrent writes 500. | **Blocking prerequisite.** Any atomic-claim design will intermittently 500 until this is fixed. |

### What works today (so we don't rebuild it)

Boards, epics, tickets, tasks (checklists), notes (agent scratchpad), blocking
relations with a `ready` query, audit events, bulk create, board context
snapshot for LLMs, full REST, two MCP transports, embedded Svelte UI with SSE
live updates and drag-and-drop, automatic migrations, clean 31-test suite.
A lot of Ledgerboard is already standing.

## 9. The single biggest obstacle to the next phase

**SQLite is opened without a busy timeout and without a capped connection pool,
so concurrent writers fail immediately with `SQLITE_BUSY` (19/40 in a plain
test, 6/20 through the real REST path), and the API turns that into an opaque
HTTP 500 that is never logged.**

Atomic claim *is* a concurrent conditional write, so this is the first thing any
lease implementation will hit — and it will present as a flaky, unexplained 500
rather than a claim conflict. It must be fixed **before or with** the claim work:
set `_busy_timeout` (e.g. 5000 ms) and/or `db.SetMaxOpenConns(1)` for writes in
`internal/db/db.go`, and make the API surface/ log real errors instead of
`internal server error`.

Secondary obstacle: `UpdateTicket`'s read-modify-write is not transactional, so
even with busy handling, "check then set" is not atomic. The claim must be a
single conditional `UPDATE … WHERE assignee = '' OR lease_expired` (or
`BEGIN IMMEDIATE`) whose `RowsAffected` decides the winner — not a
read-then-write in Go.

## 10. Reproducing this baseline

```sh
. /etc/profile.d/go.sh
cd /home/team/shared/ledgerboard
go build ./... && go test ./...
go build -o kanban-mcp .
rm -f /tmp/scratch/*.db*
./kanban-mcp -port 8099 -db /tmp/scratch/ledgerboard.db -mcp-transport http &
# then drive REST at http://127.0.0.1:8099/api/v1 or MCP at /mcp, UI at /
```

Scratch DBs, logs and the probe scripts used above live in `/tmp/scratch/`
outside the repo; nothing scratch was committed.
