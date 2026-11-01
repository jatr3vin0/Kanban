# Phase 0 — Recon

**Status:** working document produced by the architect. Verified against the code and by running it.
**Repo:** `/home/team/shared/ledgerboard` — fork of `rgracey/kanban-mcp`, pinned at `c6938a2` + one local commit `dc65c77` (`docs: add phase-0 build/test baseline`).
**Date:** 2026-10-30. **Toolchain:** Go 1.27.3 linux/amd64, `CGO_ENABLED=1`, SQLite via `modernc.org/sqlite v1.46.1` (embedded SQLite **3.51.2**).

Everything below was produced in this session. Raw evidence is in `docs/test-results/phase-0/`.
Nothing in this document is owner-ratified; it is the team's reading of the code as it stands.

---

## 0. Headline findings (read this if you read nothing else)

| # | Finding | Consequence for Ledgerboard |
|---|---|---|
| **H1** | **There is no LICENSE file in the repository and no license section in the README.** | Default copyright applies: "all rights reserved". The fork-and-extend plan, and any redistribution of a modified binary, is **not permitted** by the code as it stands. Owner decision required (§9). This is the single biggest risk to the project, and it is not a technical one. |
| **H2** | `foreign_keys=ON` is executed **once on one pooled connection**, so `ON DELETE CASCADE` / `ON DELETE SET NULL` are enforced on **1 of 8** connections under concurrency (measured: `[1 0 0 0 0 0 0 0]`; dangling-FK inserts succeeded on 7 of 8). | Every design that leans on FK cascade (board delete, ticket delete, handoff rows, `lease_events`) must set pragmas in the **DSN**, not with `Exec`. Verified fix: `?_pragma=foreign_keys(ON)` applies to all 8/8. |
| **H3** | Concurrent writes fail with `SQLITE_BUSY` and surface as an unlogged HTTP 500. Measured: **1/20** concurrent PUTs on one ticket → 500; **2/20** across distinct tickets → 500. In a raw driver probe: **17/40** writes lost without `busy_timeout`, **0/40** with `busy_timeout(5000)`. | Atomic claim *is* a concurrent conditional write. This must be fixed before or with the claim work. |
| **H4** | The status wire value is **`in_progress`** (underscore). The README and the owner's spec both say "in progress". `{"status":"in progress"}` → **HTTP 400**. | Preserved-feature compatibility means keeping `in_progress` on the wire; a display-only label change belongs in the UI. Do not "fix" the wire format. |
| **H5** | The vendored driver is **already pure Go** (`modernc.org/sqlite`), builds with `CGO_ENABLED=0`, embeds SQLite 3.51.2, and supports `RETURNING`. | The spec's "pure-Go driver" requirement is **already satisfied**. No driver migration is needed. This corrects a `[VERIFY]` assumption in the earlier design docs. |

---

## 1. Real code vs README — where the README overstates, omits or is wrong

The README is good marketing copy and a decent quick start. As an API reference it is unreliable in
ways that would bite an agent integrator. Bluntly:

### 1.1 Wrong

| # | README claim | Reality (verified) |
|---|---|---|
| R1 | Features: status **`todo` / `in progress` / `blocked` / `done`** (L223) and the example prompt tells the agent to set status `"in progress"`. | The only accepted values are `todo`, `in_progress`, `done`, `blocked`. `PUT /api/v1/tickets/{id}` with `{"status":"in progress"}` returns **400** `{"error": "status must be one of: todo, in_progress, done, blocked"}`. The README's own suggested agent prompt produces a 400. |
| R2 | `GET /api/v1/boards/:id/events` — SSE stream (L375). | **This route does not exist.** `GET /api/v1/boards/{id}/events` → **404 `404 page not found`** (text/plain). The real stream is the **global** `GET /api/v1/events`; clients filter by `board_id` themselves (the bundled UI does exactly that). |
| R3 | "Emits an event whenever a ticket on the board is created, updated, or deleted." (L375) | The stream is not per-board and does not emit one event per mutation kind. It emits SSE event name **`board_change`** with payload `{"type": "...", "board_id": "..."}` — **no ticket id, no status, no diff**. Observed types: `ticket.created`, `ticket.updated`, `ticket.deleted`, `task.created`, `note.created`. Not emitted: task updated/deleted, note updated/deleted, relations, boards, epics, leases. |
| R4 | "Audit history — **every** create, edit, move, note, and task change is recorded" (L230). | Recording is best-effort and partial: the `actor` column is **always `''`** (every writer passes `""`); ticket **deletes** and note **deletes** record nothing; board and epic changes record nothing; relation add/remove does record (two rows). The UI has no actor to display. |
| R5 | "All IDs are UUID v4" (L285). | IDs are 32 hex characters with **no hyphens** (`hex.EncodeToString` of a v4 UUID), e.g. `83f681a8a8864a1cbe1deff69e6570e9`. Not the canonical `8-4-4-4-12` form; anything that validates a UUID string will reject them. |
| R6 | "Timestamps are RFC3339" (L285). | Stored as RFC3339 **text with second precision** (`time.RFC3339` truncates sub-second). Worse: the **create response** echoes `time.Now()` with nanoseconds (`2026-10-30T23:08:22.180080756Z`) while every subsequent read returns `2026-10-30T23:08:22Z`. The same object changes its timestamp format between the create response and a GET. Measured: 5 tickets created within 3 s → **2 distinct `created_at` values**. |
| R7 | Prerequisites: "Go 1.22 or later" (L132). | `go.mod` declares `go 1.25.5`; CI pins `1.25.5`. Go 1.22 will not build it. |
| R8 | "Zero external dependencies" (L234). | True at *runtime* (no cgo, embedded assets), but the module graph has 30+ modules including `modernc.org/libc` and `golang-migrate`. Fine, but the phrase will be read as "no deps" by an auditor. |

### 1.2 Omits (documented surface is incomplete)

| # | Undocumented but real |
|---|---|
| O1 | `GET /api/v1/boards/{id}/context` — full board snapshot (board + epics + tickets with tasks and relations). |
| O2 | `GET /api/v1/boards/{id}/ready` — unblocked `todo` tickets, priority-descending. |
| O3 | `POST /api/v1/boards/{id}/tickets/bulk` — bulk ticket create in one transaction. |
| O4 | `POST /api/v1/tickets/{id}/tasks/bulk` — bulk task create (`{"titles": [...]}`). |
| O5 | `GET /api/v1/boards/{id}/tickets` accepts an **`assignee`** filter (README lists only `status`, `priority`, `epic_id`, `q`, `sort_by`, `sort_order`). |
| O6 | `POST /api/v1/tickets/{id}/relations` takes an optional **`kind`** (only `blocks` accepted); `DELETE .../relations/{toId}?kind=blocks`. |
| O7 | MCP `task` tool has a **`bulk_create`** action with a `titles_json` argument (README lists only `list, create, update, delete`). |
| O8 | MCP `ticket{action:"get"}` returns an envelope with `notes`, `history`, `tasks`, `relations` only when the corresponding `include_*` boolean is passed. README mentions the flags but not that the default payload omits them entirely (the JSON keys are absent, not empty). |
| O9 | There is **no authentication, no authorization and no rate limiting anywhere**. Nothing in the README says so. |
| O10 | The HTTP listener binds **`:port`** = all interfaces (see §4.9), not localhost. The README says "available at `http://localhost:8080`", which reads as a localhost-only service. |

### 1.3 Correct, for the record

Boards/epics/tickets/tasks/notes/relations all exist and work; bulk ticket create is one real transaction;
`board context` and `board ready` are real and useful; `resolution_json` is a real free-form JSON blob
column; there are exactly **6** MCP action-dispatch tools; both MCP transports work; the frontend is real,
embedded and served from the same binary; migrations run automatically on open.

---

## 2. Schema — exactly as it exists

Mechanism: **`golang-migrate/migrate/v4`** with migrations embedded via `go:embed` from
`internal/db/migrations/`, applied in `db.Open()` on every start. Bookkeeping is a **`schema_migrations`
table** (`version uint64, dirty bool`, unique index on `version`), **not** `PRAGMA user_version`
(which is `0` on a fully-migrated database — verified). There is exactly one migration pair:
`0001_init.up.sql` / `0001_init.down.sql`. Down migrations exist. No other migration mechanism.

```sql
CREATE TABLE schema_migrations (version uint64, dirty bool);          -- created by golang-migrate
CREATE UNIQUE INDEX version_unique ON schema_migrations (version);

CREATE TABLE boards (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE epics (
    id TEXT PRIMARY KEY,
    board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE tickets (
    id TEXT PRIMARY KEY,
    board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    epic_id  TEXT REFERENCES epics(id) ON DELETE SET NULL,
    title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    status   TEXT NOT NULL DEFAULT 'todo',      -- no CHECK constraint
    priority TEXT NOT NULL DEFAULT 'medium',    -- no CHECK constraint
    assignee TEXT NOT NULL DEFAULT '',          -- free text, NOT NULL, empty string = unassigned
    resolution TEXT,                            -- nullable JSON blob, free-form
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE ticket_events (
    id TEXT PRIMARY KEY,
    ticket_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    type TEXT NOT NULL, actor TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL);
CREATE INDEX ticket_events_ticket_id ON ticket_events(ticket_id);

CREATE TABLE notes (
    id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    body TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    title TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX tasks_ticket_id ON tasks(ticket_id);

CREATE TABLE ticket_relations (
    from_ticket_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    to_ticket_id   TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    kind TEXT NOT NULL DEFAULT 'blocks', created_at TEXT NOT NULL,
    PRIMARY KEY (from_ticket_id, to_ticket_id, kind));
CREATE INDEX ticket_relations_from ON ticket_relations(from_ticket_id);
CREATE INDEX ticket_relations_to   ON ticket_relations(to_ticket_id);
```

**Answers to the specific questions asked:**

| Question | Answer |
|---|---|
| Ticket `version` column? | **No.** No version/etag/revision column anywhere in the database. |
| What is `assignee` really? | `TEXT NOT NULL DEFAULT ''`, free text, no constraint, no identity, no expiry, no token. Empty string is the "unassigned" sentinel (not `NULL`). Written by the UI (editable field), REST and MCP. |
| Any claim / lock / lease field? | **None.** A scan of every column in every table for `lease`/`lock`/`claim`/`holder`/`version`/`expire`/`generation` returns only `schema_migrations.version`. |
| Indexes on `tickets`? | Only the implicit PK index (`sqlite_autoindex_tickets_1`). **No index on `status`, `priority`, `assignee`, `board_id` or `created_at`.** `ListTickets` and `ReadyTickets` are full scans with a sort. Fine at local scale, worth knowing before adding a claim query. |
| Triggers? | **None.** Zero triggers in the schema. |
| FKs pointing at `tickets`? | Four: `ticket_events.ticket_id`, `notes.ticket_id`, `tasks.ticket_id`, `ticket_relations.{from,to}_ticket_id` — all `ON DELETE CASCADE`. Any table rebuild of `tickets` must handle all four (§8, G9). |
| Timestamp type | `TEXT`, RFC3339, **second precision**. Comparisons are lexicographic string comparisons — which is correct for this format and must stay that way if we add lease times as TEXT; mixing TEXT and INTEGER times in one column would silently break ordering. |
| Enums | Not enforced by the database. Validated in Go, and **only in the REST/MCP handlers** — the store layer will happily write any status/priority string if called directly (the tests do exactly this). |

---

## 3. MCP — the real surface

Library: `github.com/mark3labs/mcp-go v0.44.0`. Server name `kanban-mcp`, version `1.0.0`,
protocol `2025-06-18`. **Exactly 6 tools**, all action-dispatch, all with a single **required** argument
`action` and everything else optional and flat (no nested objects except JSON-in-a-string arguments).

| Tool | Actions actually implemented | Arguments (all optional except `action`) |
|---|---|---|
| `board` | `list`, `get`, `create`, `update`, `delete`, `summary`, `context`, `ready` | `id`, `name`, `description`, `filter_status`, `omit_descriptions` |
| `epic` | `list`, `get`, `create`, `update`, `delete` | `id`, `board_id`, `title`, `description` |
| `ticket` | `list`, `get`, `create`, `bulk_create`, `update`, `delete`, `history` | `id`, `board_id`, `title`, `description`, `status`, `priority`, `epic_id`, `assignee`, `filter_status`, `filter_priority`, `filter_epic_id`, `filter_assignee`, `q`, `sort_by`, `sort_order`, `include_notes`, `include_history`, `include_tasks`, `include_relations`, `tickets_json`, `resolution_json` |
| `task` | `list`, `create`, `bulk_create`, `update`, `delete` | `id`, `ticket_id`, `title`, `titles_json`, `done` |
| `note` | `list`, `add`, `update`, `delete` | `id`, `ticket_id`, `body` |
| `relation` | `list`, `add`, `delete` | `ticket_id`, `to_ticket_id` |

Full `tools/list` JSON (including `inputSchema` and the auto-generated `annotations`) is captured in
`docs/test-results/phase-0/mcp-tools-list.json`. Note the annotations: every tool is advertised as
`destructiveHint: true, readOnlyHint: false` — a hint, not a constraint, but it tells clients that
"list" is as dangerous as "delete".

**Transports.** Both exist and both were exercised in this session.

- **Streamable HTTP** at `/mcp`, mounted on the same router as REST and the UI, only when
  `-mcp-transport http|both`. `initialize` returns `Mcp-Session-Id` in a response header and
  `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"kanban-mcp","version":"1.0.0"}}`.
- **stdio** via `server.NewStdioServer(...).Listen(ctx, os.Stdin, os.Stdout)` when
  `-mcp-transport stdio|both` (default is **`stdio`**).

**How errors are returned.** Two distinct shapes, and this matters for parity:

1. *Tool-level* failures (unknown action, missing argument, store error) are **not** JSON-RPC errors.
   They are a normal `result` with `isError: true` and a single text block:
   `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"unknown action \"nope\""}],"isError":true}}`.
   There is **no error code, no machine-readable discriminator, no `structuredContent`** — just a
   human sentence. An agent cannot reliably tell "not found" from "wrong args" from "someone else holds it"
   without parsing prose.
2. Successful list actions wrap arrays as `{"items": [...]}` (a JSON object is required for
   `structuredContent`); object results are returned bare. `delete` actions return the text `"deleted"`.

**stdio defects found (both real, both worth fixing before Phase 3):**

- **stdout is shared with the logger.** `main.go` installs `slog` with `os.Stdout`, and stdio MCP writes
  JSON-RPC to `os.Stdout`. A captured stdio session begins with four JSON *log* lines before the first
  JSON-RPC response. They happen to be valid JSON, so a client that ignores messages without `jsonrpc`
  survives; a strict client does not. Stdout must be reserved for protocol traffic.
- **The process does not exit when stdin closes.** After EOF, `StartStdio` returns, `main` keeps waiting
  on its signal channel, and the HTTP server stays up (see next point).

**Transport mode does not gate the HTTP server.** `main.go` always starts `ListenAndServe` on `:port`
regardless of `-mcp-transport`. With `-mcp-transport stdio`, the REST API and the web UI are still served
and were reachable (`GET /api/v1/boards` → 200, `GET /` → 200 on the port). A user who configures
stdio-only, expecting no network surface, gets a full unauthenticated HTTP service.

---

## 4. REST — every `/api/v1` route

chi router; all handlers under `internal/api/`. IDs are 32-hex strings; bodies are JSON; success
payloads are bare objects/arrays (no envelope); timestamps are RFC3339 strings.

| Method | Path | Request body | Success | Notes |
|---|---|---|---|---|
| GET | `/api/v1/events` | — | 200 `text/event-stream` | global SSE, see §5 |
| GET | `/api/v1/boards` | — | 200 `[Board]` | |
| POST | `/api/v1/boards` | `{name, description}` | **201** `Board` | `name` required (trimmed, non-empty) |
| GET | `/api/v1/boards/{id}` | — | 200 `Board` | 404 if missing |
| PUT | `/api/v1/boards/{id}` | `{name?, description?}` | 200 `Board` | empty string = "not supplied"; cannot clear a description |
| DELETE | `/api/v1/boards/{id}` | — | **204** | cascade (subject to H2) |
| GET | `/api/v1/boards/{id}/summary` | — | 200 `{board_id, ticket_counts{}, epics[]}` | |
| GET | `/api/v1/boards/{id}/context` | — | 200 `{board, epics[], tickets[]}` | each ticket carries `tasks[]` and `relations[]` |
| GET | `/api/v1/boards/{id}/ready` | — | 200 `[Ticket]` | `status='todo'` and no incoming `blocks`; priority desc, created asc |
| GET | `/api/v1/boards/{id}/epics` | — | 200 `[Epic]` | |
| POST | `/api/v1/boards/{id}/epics` | `{title, description}` | **201** `Epic` | |
| GET | `/api/v1/boards/{id}/tickets` | — | 200 `[Ticket]` | filters: `status`, `priority`, `epic_id`, `assignee`, `q`, `sort_by`, `sort_order` |
| POST | `/api/v1/boards/{id}/tickets` | `{title, description, status, priority, epic_id, assignee, resolution}` | **201** `Ticket` | defaults `todo`/`medium`/`""` |
| POST | `/api/v1/boards/{id}/tickets/bulk` | `[TicketRequest]` | **201** `[Ticket]` | one transaction; all-or-nothing |
| GET | `/api/v1/epics/{id}` | — | 200 `Epic` | |
| PUT | `/api/v1/epics/{id}` | `{title?, description?}` | 200 `Epic` | |
| DELETE | `/api/v1/epics/{id}` | — | 204 | tickets keep existing, `epic_id` → NULL |
| GET | `/api/v1/tickets/{id}` | — | 200 `Ticket` | |
| PUT | `/api/v1/tickets/{id}` | any subset of `title, description, status, priority, epic_id, assignee, resolution` | 200 `Ticket` | `epic_id: null` clears; `resolution: null` clears; see §4.3 |
| DELETE | `/api/v1/tickets/{id}` | — | 204 | |
| GET | `/api/v1/tickets/{id}/notes` | — | 200 `[Note]` | |
| POST | `/api/v1/tickets/{id}/notes` | `{body}` | **201** `Note` | |
| GET | `/api/v1/tickets/{id}/events` | — | 200 `[TicketEvent]` | audit trail, oldest first |
| GET | `/api/v1/tickets/{id}/tasks` | — | 200 `[Task]` | 404 if the ticket is missing |
| POST | `/api/v1/tickets/{id}/tasks` | `{title}` | **201** `Task` | |
| POST | `/api/v1/tickets/{id}/tasks/bulk` | `{titles: [string]}` | **201** `[Task]` | blank titles filtered out |
| GET | `/api/v1/tickets/{id}/relations` | — | 200 `[TicketRelation]` | both directions, with denormalised titles |
| POST | `/api/v1/tickets/{id}/relations` | `{to_ticket_id, kind?}` | **201** `TicketRelation` | `kind` must be `blocks`; self-relation → 400; duplicate → **409** |
| DELETE | `/api/v1/tickets/{id}/relations/{toId}` | — | 204 | `?kind=` defaults to `blocks` |
| PUT | `/api/v1/tasks/{id}` | `{title?, done?}` | 200 `Task` | |
| DELETE | `/api/v1/tasks/{id}` | — | 204 | |
| GET | `/api/v1/notes/{id}` | — | 200 `Note` | |
| PUT | `/api/v1/notes/{id}` | `{body}` | 200 `Note` | |
| DELETE | `/api/v1/notes/{id}` | — | 204 | |

Anything else under `/api/v1` → **404 `404 page not found`** (`text/plain`). Non-API paths fall through
to the embedded SPA (`index.html`).

### 4.1 Error shape

```json
{"error": "<sentence>"}      // with Content-Type: text/plain; charset=utf-8  (via http.Error)
```
Only **two** status codes carry meaning: `400` (validation, message names the field) and `404`
(`{"error": "not found"}`). Everything else that goes wrong — including every `SQLITE_BUSY` — is
**`500` `{"error": "internal server error"}`**. `409` appears in exactly one place (duplicate relation).
There is no error code, no `details`, no request id in the body. Not-found detection is done by
**substring-matching the error string** (`strings.Contains(err, "no rows in result set")` in
`internal/api/utils.go`) — brittle, and it means a store error whose text happens to contain "not found"
becomes a 404.

### 4.2 Unknown JSON fields are silently dropped

Verified: `PUT /api/v1/tickets/{id}` with `{"lease_expires_at":"2030-01-01T00:00:00Z","title":"t1-renamed"}`
→ **200**, title changed, `lease_expires_at` ignored with no warning. The handler decodes into
`map[string]json.RawMessage` and copies only the seven known keys; the store's own
`invalid field key` guard is therefore unreachable from HTTP. Any future `generation`/`actor` field sent by
an old client, or any typo by a new one, is accepted and ignored. **A client cannot tell it was ignored.**

### 4.3 Two update paths behave differently

`PUT /tickets/{id}` refuses `"title": ""` (empty title is skipped, not rejected — you get 200 and the old
title), while `description` and `assignee` accept empty strings. MCP `ticket{action:update}` passes
whatever was supplied straight into the store. So "clear a field" is expressible for some fields and not
others, and the two transports are not identical here.

### 4.4 `UpdateTicket` is an untransacted read-modify-write

`internal/store/ticket.go:180` — `GetTicket` → `UPDATE` → `GetTicket` → insert audit event, with **no
transaction, no version guard, no conditional WHERE**. Two concurrent updates both return 200 and the
last writer wins silently; the audit log gets two `edited` rows, one of which describes a write that no
longer exists. This is the exact collision Ledgerboard exists to prevent, and it is also the template
that a naive claim implementation would copy.

### 4.5 Concurrency: measured

| Experiment | Result |
|---|---|
| 20 concurrent `PUT /tickets/{id}` on **one** ticket | **19 × 200, 1 × 500** |
| 20 concurrent `PUT /tickets/{id}` on **20 distinct** tickets | **18 × 200, 2 × 500** |
| 40 concurrent raw `UPDATE` via the driver, upstream config | **23 ok, 17 `SQLITE_BUSY`** |
| 40 concurrent raw `UPDATE`, with `_pragma=busy_timeout(5000)` | **40 ok, 0 busy** |

The 500s are `SQLITE_BUSY` converted to `{"error":"internal server error"}`. chi's request logger records
`- 500 35B` but **nothing about the cause**; there is no error log at all. In production this is
indistinguishable from a bug in the handler.

### 4.6 Timestamps

Second precision in the database (see R6). Ordering by `created_at` is therefore not a total order:
5 tickets created within 3 s produced 2 distinct `created_at` values, so `ORDER BY created_at ASC` among
ties falls back to storage order. Any lease design that sorts or compares times must not inherit this.

### 4.7 Frontend

Svelte 5 app in `frontend/`, built assets committed under `frontend/dist/`, `go:embed all:dist` in
`frontend/fs.go`, served by the same router with SPA fallback. Columns are driven by a hard-coded
`[{status:'todo',label:'To Do'},{status:'in_progress',label:'In Progress'},{status:'blocked',label:'Blocked'},{status:'done',label:'Done'}]`
— the label is display-only, the wire value is `in_progress`. The detail panel edits status, priority,
epic, **assignee (free text)**, resolution (commit SHA / PR URL / notes), description, tasks, notes and
shows an event timeline. Drag-and-drop changes `status` via `PUT`.

### 4.8 UI realtime

`KanbanBoard.svelte` opens `new EventSource('/api/v1/events')`, listens for the `board_change` event, and
calls `load()` (a full refetch) when `board_id` matches the selected board. It does not use the payload
beyond `board_id`. So the SSE stream is a *cache invalidator*, not a data channel.

### 4.9 Bind address

`main.go` uses `Addr: fmt.Sprintf(":%d", cfg.Port)` — an empty host, so the listener is **all
interfaces (0.0.0.0 / [::])**, not localhost. Confirmed reachable on `127.0.0.1` in both transport modes.
There is no auth, no TLS, and no `--host` flag. This directly contradicts the owner's "localhost binding
by default" constraint and is the smallest gap to close in the whole spec (one flag + a default).

---

## 5. Audit and SSE

### 5.1 What is recorded today

One table, `ticket_events`, one row per recorded mutation: `id` (32-hex), `ticket_id`, `type`, `actor`
(**always the empty string** — no caller passes anything else), `payload` (JSON text), `created_at`
(second precision). Read via `GET /api/v1/tickets/{id}/events`, MCP `ticket{action:"history"}` or
`ticket{action:"get", include_history:true}`; the UI renders it as a timeline.

| Event `type` | Written by | Payload |
|---|---|---|
| `created` | `CreateTicket`, `BulkCreateTickets` | `{"title","status","priority"}` |
| `moved` | `UpdateTicket` when `status` actually changed | `{"from","to"}` |
| `edited` | `UpdateTicket` for any other field set | the raw changed fields, e.g. `{"assignee":"agent-2"}` |
| `commented` | `CreateNote` | `{}` (payload `nil`) — note body is **not** recorded |
| `comment_edited` | `UpdateNote` | `{}` |
| `task_added` | `CreateTask` | `{"task_title"}` |
| `task_updated` | `UpdateTask` | `{"task_title","done"?,"title"?}` |
| `task_deleted` | `DeleteTask` | `{"task_title"}` |
| `relation_added` | `AddRelation` | one row **on each** ticket: `{"kind","related_ticket","related_title","direction"}` |
| `relation_removed` | `DeleteRelation` | same, two rows |

`EventCommentEdited`/`EventCommented` are legacy names from a removed comments feature; notes reuse them.

**Not recorded at all:** ticket delete, note delete, board create/update/delete, epic
create/update/delete, bulk task create (it loops over `CreateTask`, so one `task_added` per title — fine),
and — obviously — nothing about assignment, ownership, or leases. There is no table for non-ticket events,
no global audit log, and no way to query "what did agent X do". `actor` being universally `''` means the
audit trail cannot answer "who".

### 5.2 SSE — exactly what the stream emits

Route: `GET /api/v1/events` (global, no board path, no auth). Implementation: an in-process `Hub`
holding `chan SSEEvent` subscribers (buffer 16, **slow clients silently drop events**), `Publish` is
called from HTTP handlers only — never from the store — so anything written through MCP emits **no SSE
event at all**.

Frame format:

```
event: ping
data: {}

event: board_change
data: {"type":"ticket.created","board_id":"d3251bb6a7604239a8361c9c90ae3296"}
```

- `ping` once on connect.
- Every mutation is the **same SSE event name `board_change`**; the kind is in the JSON `type` field.
- Payload is exactly `{type, board_id}` — no ticket id, no ids of anything, no diff, no actor, no
  timestamp, no sequence number.
- Published types (all from REST handlers): `ticket.created`, `ticket.updated`, `ticket.deleted`,
  `task.created` (single and bulk), `note.created`. **Not** published: `task.updated`, `task.deleted`,
  `note.updated`, `note.deleted`, relations, boards, epics, and anything done over MCP.

---

## 6. Upstream tests actually run — this session

Run from `/home/team/shared/ledgerboard` at `dc65c77` with Go 1.27.3. Raw logs in
`docs/test-results/phase-0/`.

```sh
go build ./...                       # exit 0, no output
go vet ./...                         # exit 0, no findings
go test ./... -count=1 -v            # 31 top-level tests, 74 RUN lines, 31 PASS, 0 FAIL, 0 SKIP
go test ./... -count=1 -race         # exit 0, all packages ok
CGO_ENABLED=0 go build ./...         # exit 0  (no cgo required)
CGO_ENABLED=0 go test -race ./...    # "go: -race requires cgo" (toolchain, not the driver)
```

Package results (`go test ./... -count=1`):

```
?       github.com/rgracey/kanban-mcp            [no test files]
?       github.com/rgracey/kanban-mcp/frontend   [no test files]
ok      github.com/rgracey/kanban-mcp/internal/api      0.045s
ok      github.com/rgracey/kanban-mcp/internal/config   0.005s
ok      github.com/rgracey/kanban-mcp/internal/db       0.020s
ok      github.com/rgracey/kanban-mcp/internal/mcp      0.044s
ok      github.com/rgracey/kanban-mcp/internal/models   [no test files]
ok      github.com/rgracey/kanban-mcp/internal/store    0.226s
```

31 test functions: `TestAPIBoards`, `TestAPIEpics`, `TestAPINotes`, `TestAPITickets`,
`TestAllToolsRegistered`, `TestBoardCRUD`, `TestBoardGetByName`, `TestBulkCreate`, `TestEpicCRUD`,
`TestGetBoardSummary`, `TestLoad_*` (8), `TestNoteCRUD`, `TestOpen`, `TestOpenInvalidPath`,
`TestOpenReRun`, `TestStore*` (6), `TestTaskCRUD`, `TestTicketGetIncludeTasksAndRelations`,
`TestTicketHistory`, `TestTicketWorkflow`.

**What the suite does not cover — this matters more than the count.** There is no test that uses
`t.Parallel`, no goroutine, no second connection, no second process. So the suite is green *and* says
nothing about concurrency: `go test -race` passing is evidence of nothing here. Specifically absent:
concurrent claims, expiry boundaries, fencing, crash boundaries, migration-from-old-schema, and any
assertion about `SQLITE_BUSY`. `TestOpen` asserts `PRAGMA foreign_keys` = 1 and `journal_mode` = `wal`
— on the **first** pooled connection only, which is exactly why H2 goes unnoticed.

---

## 7. Preserved-feature inventory

Everything the spec says must be preserved, where it lives, and whether it works (all verified by reading
the code and exercising it):

| Feature | Where | Works? |
|---|---|---|
| Boards | `boards` table, `internal/store/board.go`, `internal/api/boards.go`, `board` MCP tool | ✅ |
| Epics | `epics` table, `internal/store/epic.go`, `internal/api/epics.go`, `epic` tool | ✅ |
| Tickets | `tickets` table, `internal/store/ticket.go`, `internal/api/tickets.go`, `ticket` tool | ✅ |
| Statuses `todo`/`in progress`/`blocked`/`done` | `models.Status*`; **wire value is `in_progress`** | ✅ with `in_progress`; the literal `"in progress"` is rejected (400) |
| Priorities low→critical | `models.Priority*`, CASE-weighted ordering in SQL | ✅ |
| Checklists (tasks) | `tasks` table, `internal/store/task.go`, `internal/api/tasks.go`, `task` tool | ✅ (no reorder endpoint; `position` is append-only count) |
| Notes | `notes` table, `internal/store/note.go`, `internal/api/notes.go`, `note` tool | ✅ |
| Blocking relations | `ticket_relations`, `internal/store/relation.go`, `internal/api/relations.go`, `relation` tool | ✅ only `kind='blocks'`; no cycle detection; self-block rejected |
| Assignees | `tickets.assignee` TEXT free text, editable in UI, filterable in list | ✅ (no identity, no expiry — it is a label) |
| Bulk create (tickets) | `BulkCreateTickets`, `POST /boards/{id}/tickets/bulk`, `ticket{bulk_create}` | ✅ single transaction |
| `board context` | `BoardContext()`, `GET /boards/{id}/context`, `board{context}` with `filter_status`/`omit_descriptions` | ✅ |
| `board ready` | `ReadyTickets()`, `GET /boards/{id}/ready`, `board{ready}` | ✅ |
| `resolution_json` | `tickets.resolution` JSON text; `resolution_json` MCP arg; REST `resolution` object | ✅ (free-form; no schema, no validation) |
| Audit | `ticket_events`, `GET /tickets/{id}/events`, `ticket{history}` | ✅ but partial and actorless (§5.1) |
| REST `/api/v1` | `internal/api/router.go` | ✅ 30 routes |
| MCP action-dispatch | `internal/mcp/tools.go` | ✅ 6 tools |
| stdio + HTTP MCP | `internal/mcp/server.go`, `main.go` | ✅ both, with the defects in §3 |
| SQLite + migrations | `modernc.org/sqlite`, `golang-migrate`, `internal/db/migrations/` | ✅ |
| Embedded frontend | `frontend/fs.go` `//go:embed all:dist`, SPA fallback in `router.go` | ✅ |
| SSE | `internal/api/sse.go`, `GET /api/v1/events` | ✅ but coarse and REST-only (§5.2) |

Nothing in this list needs rebuilding. The substance to add is ownership.

---

## 8. Gaps vs the spec

| # | Spec requires | Upstream reality | Smallest additive change |
|---|---|---|---|
| G1 | Ownership with expiry, one holder | No lease/claim/holder/expiry/generation column anywhere; `assignee` is a label | Add nullable `lease_holder TEXT`, `lease_holder_type TEXT`, `lease_acquired_at INTEGER` (unix ms, G8), `lease_expires_at INTEGER` (unix ms, G8), `lease_generation INTEGER NOT NULL DEFAULT 0`, `lease_renewals INTEGER NOT NULL DEFAULT 0` to `tickets` via `ALTER TABLE ADD COLUMN`. No rebuild needed — nullable columns with defaults are legal `ADD COLUMN` and need no `CHECK`. |
| G2 | Atomic claim | `UpdateTicket` is untransacted read-modify-write; no version column | One conditional `UPDATE ... WHERE id=? AND (lease_holder IS NULL OR lease_expires_at <= ?)`, decide by `RowsAffected()`, inside `BEGIN IMMEDIATE`. Never read-then-write. |
| G3 | Fencing / stale writes rejected | No generation, no compare-and-set | Reuse `lease_generation` as the fencing token; require it in agent writes and put it in the `WHERE`. |
| G4 | Concurrency safety at the DB | No `busy_timeout`, no pool cap → measured `SQLITE_BUSY` 500s (§4.5) | **DSN pragmas** `?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)` + `SetMaxOpenConns(1)` for writes. Verified: 40/40 writes succeed. |
| G5 | FK integrity / cascade reliability | `Exec("PRAGMA foreign_keys=ON")` reaches 1 of 8 connections (H2) | Move all pragmas into the DSN (same fix as G4). Do not rely on `Exec`. |
| G6 | Localhost binding by default | Binds `:port` = all interfaces, no auth (§4.9) | Add `--host`/`KANBAN_HOST` defaulting to `127.0.0.1`; keep `0.0.0.0` an explicit opt-in. |
| G7 | Unknown/extra JSON fields must not be silently dropped | Silently ignored (§4.2) | Decode into a typed struct with `DisallowUnknownFields`, or return an `unknown_fields` warning list. Additive: reject only when a strictness flag is set, to keep old clients working. |
| G8 | Timestamps precise enough for leases | Second precision; create-response and read-response disagree (§4.6) | New lease columns store **unix milliseconds as INTEGER** (new columns only — do not convert `created_at`), and lease decisions use server-computed `now` passed as a bound parameter. Leave existing TEXT timestamps alone for compatibility. |
| G9 | Additive schema, existing DBs keep working | One migration `0001`; no version column; 4 FK children of `tickets`; no triggers | Add `0002_lease.up.sql` with `ALTER TABLE ADD COLUMN` only. **No table rebuild** — which also sidesteps re-creating indexes, the 4 cascading FKs, and the `DROP TABLE` trap flagged in `schema-v1.md`. |
| G10 | Audit must attribute actions | `actor` always `''`; deletes unrecorded; MCP writes emit no events | Populate `actor` from the authenticated identity (or trusted local config) in a single choke point in the store; extend `ticket_events` with new types rather than a new table. |
| G11 | One business layer behind MCP/REST/CLI/UI | Two entry points (`api` handlers, `mcp` handlers) each doing their own validation; they already disagree (§4.3); SSE published from HTTP handlers only | Move validation + SSE emission into the store/service layer so both transports inherit it. Additive: keep handler signatures, delete the duplicated validation. |
| G12 | Structured handoff | Only free-form `resolution` JSON and `notes` | New `handoffs` table + `handoff_id` column on `tickets` (both additive). |
| G13 | Outbox to the operator's memory | Nothing | New `outbox` table, config-gated `off`/`worker`. Additive. |
| G14 | Idempotency | Nothing | New `idempotency_keys` table. Additive. |
| G15 | Error codes, machine-readable | Sentence strings; `isError:true` text; 500 for everything | Introduce a shared `{code, message, details}` error type used by both transports. **Additive**: add `structuredContent.error.code` on MCP and an `error_code` field in the REST body while keeping `{"error": "..."}` intact for old clients. |
| G16 | MCP stdio is a clean protocol channel | Logs share stdout; process does not exit on EOF (§3) | Log to **stderr** (or a file) always; exit when the stdio stream closes. |
| G17 | Enums enforced | No `CHECK` constraints; store accepts any string | Validate in the service layer (Go), not with a `CHECK` — adding `CHECK` needs a table rebuild (G9) and would reject existing rows. |

---

## 9. Upstream license finding

**There is no license file, and no license statement in the README.**

```
$ ls -a
.  ..  .git  .github  .gitignore  README.md  docs  frontend  go.mod  go.sum  internal  main.go
$ find . -iname 'LICEN*' -o -iname 'COPYING*'      # (excluding node_modules)
(no results)
$ grep -in 'licen' README.md
(no license section)
```

The only `licen` matches in the README are the word "release" in unrelated sentences.

**What that means, stated plainly:** under the Berne Convention and the GitHub Terms of Service, a public
repository with no license is **not** open source. The author retains all rights; the default is
"all rights reserved". The GitHub ToS grants only the narrow right to view and fork *within GitHub* — it
does **not** grant the right to modify and redistribute, nor to distribute a built binary, nor to use the
code commercially. There is nothing to quote, because upstream provides no grant at all; the operative
fact is the absence:

> No `LICENSE`, `COPYING` or `NOTICE` file exists in the repository at `c6938a2`, and the README contains
> no license section or SPDX identifier. Absent an explicit grant, no permission to copy, modify or
> redistribute is given.

**Consequences the owner must decide on (this is not mine to decide):**

1. **Fork-and-extend is a licensing question before it is an engineering one.** Publishing `jatr3vin0/Kanban`
   as a modified redistribution, or shipping a binary of it, needs upstream's permission (an issue asking
   for a license, or a written grant).
2. The team fork on GitHub is fine as a *view/fork* under the ToS; building a product on it is not.
3. Options: (a) ask `rgracey` to add a license — an MIT/Apache-2.0 ask is normal and cheap; (b) treat
   Ledgerboard as a clean-room implementation of the *concepts* with no upstream code copied (large cost,
   contradicts "fork and extend, never rebuild"); (c) proceed privately without redistribution and accept
   the risk.
4. **My recommendation: (a) first, and do not merge further phases into a publicly redistributable
   artifact until it is resolved.** Engineering can continue on the fork meanwhile — the code we write is
   ours — but the combined work cannot be distributed.

This is flagged to the lead as an owner decision, alongside the money question.

---

## 10. Pure-Go SQLite driver trade-off

**The driver is already pure Go: `modernc.org/sqlite v1.46.1`, a transpilation of SQLite to Go, no cgo.**
The spec's requirement is therefore already met and **no migration is needed**. Recording the trade-off
anyway, because the design docs assumed otherwise and someone will ask:

| Question | Answer (measured) |
|---|---|
| Driver vendored | `modernc.org/sqlite v1.46.1` (plus `modernc.org/libc v1.67.6`, `mathutil`, `memory`). Not `mattn/go-sqlite3`. |
| CGO required? | **No.** `CGO_ENABLED=0 go build ./...` → exit 0. CI already builds releases with `CGO_ENABLED: 0`. `main` imports the driver blank-imported in `internal/db/db.go`. |
| SQLite version | **3.51.2** (`SELECT sqlite_version()` through the driver). Far past 3.35, so **`RETURNING` works** — verified with `INSERT ... RETURNING id`. |
| Does `go test -race` work? | **Yes**, with the default `CGO_ENABLED=1` (a C toolchain is present): all packages ok under `-race`. It is *not* a property of the driver — `-race` requires cgo, so with `CGO_ENABLED=0` the toolchain refuses (`go: -race requires cgo`). Since we ship `CGO_ENABLED=0` binaries but test with cgo enabled, that is fine; just don't put `-race` in a `CGO_ENABLED=0` CI job. |
| DSN pragma syntax | `file:path?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)`. Verified: `PRAGMA busy_timeout` → 5000 and `PRAGMA foreign_keys` → 1 on **8/8** pooled connections, unlike the `Exec`-once approach (1/8). |
| Behavioural differences vs `mattn/go-sqlite3` | (a) Slower to compile the first time (large generated Go; a cold `CGO_ENABLED=0` build took ~2 min here). (b) Some rarely-used SQLite extensions/`sqlite3_*` C APIs are unavailable. (c) Error strings come from the transpiled library and carry SQLite result codes in parentheses (`database is locked (5) (SQLITE_BUSY)`, `constraint failed: FOREIGN KEY constraint failed (787)`) — do **not** string-match on them; the driver's typed `*sqlite.Error` should be used once we start distinguishing busy from other failures. (d) `busy_timeout` behaves identically. |
| Recommendation | **Keep `modernc.org/sqlite`.** Switching to `mattn/go-sqlite3` would add cgo, break the `CGO_ENABLED=0` release matrix, and buy nothing we need. Fix the *configuration* (DSN pragmas + pool cap), not the driver. Record this as a decision in `/docs/decisions/`. |

---

## 11. Where the earlier design docs are wrong

`/home/team/shared/design/lease-state-machine.md` and `schema-v1.md` were written before the repo was
read. These assumptions are contradicted by the code and **must not** carry into Phase 1 contracts:

| # | Design doc said | Reality |
|---|---|---|
| D1 | Status vocabulary `todo/doing/review/done/archived` (§0 A3, schema §3.2 `CHECK`) | Actual vocabulary is `todo`, `in_progress`, `done`, `blocked`. There is **no** `doing`, `review` or `archived`. `blocked` is a first-class status, which the state machine does not model at all. |
| D2 | Table `tickets` with columns `title, description, status, priority INTEGER, assignee, created_at INTEGER, updated_at INTEGER` | `priority` is **TEXT** (`low|medium|high|critical`), `created_at`/`updated_at` are **TEXT RFC3339 second precision**, and there are additional columns `board_id`, `epic_id`, `resolution`. Any `INTEGER` time/priority assumption breaks the schema. |
| D3 | Migrations via `PRAGMA user_version`, "adopt upstream's mechanism if it has one" | Upstream **does** have one: `golang-migrate` with `schema_migrations` and embedded up/down files. `user_version` is 0 and unused. Use `0002_lease.{up,down}.sql`. |
| D4 | `tickets` rebuild required because `ALTER TABLE ADD COLUMN` cannot add `CHECK` | True, but **unnecessary**: nullable columns with defaults add cleanly. A rebuild would have to recreate the 4 cascading FKs' children, the (only) indexes, and survive `DROP TABLE` — the doc's own §6 checklist. Skip the rebuild (G9). |
| D5 | Driver `[VERIFY]`: `mattn/go-sqlite3` DSN `?_busy_timeout=` vs modernc `?_pragma=` | modernc is vendored; `?_pragma=name(value)` confirmed. `RETURNING` is available (3.51.2), so the fallback path in schema §4.2 is unnecessary. |
| D6 | `[VERIFY]` "does upstream already have any claim/lock column?" | No. Nothing to migrate. |
| D7 | Lease times as unix ms **INTEGER** in a new column — fine — but also assumed `created_at`/`updated_at` were INTEGER | They are TEXT. Lease columns can be INTEGER ms; the two must never be compared to each other. |
| D8 | `lease_events` table with `CHECK (event IN (...))`, `actor_type`, `from/to_generation` | Still a reasonable design, but note the existing audit table is `ticket_events` with a **different shape** (`type, actor, payload`) and a **not-null FK to `tickets`**. Two audit tables with two vocabularies is a smell; decide in Phase 1 whether leases extend `ticket_events` or get `lease_events`, and write it down. |
| D9 | "Humans are never fenced", human identity assumed available | There is **no identity of any kind** — no auth, no user, no token, and `actor` is always `''`. "Human override" cannot be distinguished from "agent" today. Phase 1's authz matrix has to start from zero. |
| D10 | REST/MCP parity table assumes envelope `{ok, server_now, data}` and error `{code, message, details}` | Upstream has **no envelope**: success bodies are bare objects/arrays, errors are `{"error": "..."}` with `Content-Type: text/plain`, and MCP errors are `isError:true` text. Adopting an envelope is a **breaking change for existing clients** — contradicts D4 "additive superset". Phase 1 must decide: additive fields on existing shapes (recommended), or envelope behind a version/flag. |
| D11 | `claim_next` uses `RETURNING` (available — good) but assumed `status='todo'` ordering by `priority DESC` numeric | `priority` is TEXT; ordering needs the `CASE` weight already used in `ListTickets`/`ReadyTickets`. Reuse that expression, don't reinvent it. |
| D12 | Assumption A1 "one server process owns the SQLite file" | Still the right assumption, but note `main.go` has no lock file and nothing prevents two processes on one DB — and the team's own Phase 0 baseline plus this session both ran a server while other probes opened the same file. Add a guard (or document it) in Phase 2. |

---

## 12. Open questions for the owner / lead

1. **License** (§9) — blocking for redistribution. Needs a decision, not an engineering fix.
2. **Status vocabulary**: keep `in_progress` on the wire (my recommendation: yes, it is the compatibility
   promise; display "In Progress" in the UI only)? The owner's spec writes "in progress".
3. **Bind address**: may we change the default to `127.0.0.1`? It is a behaviour change for anyone using
   the released binary over a LAN, but it is what the spec requires.
4. **Unknown-field strictness** (G7): reject or warn? Rejecting is safer, warning is more compatible.
5. **Envelope vs additive fields** (D10): I recommend additive fields and keeping the existing shapes.
6. **Two audit tables or one** (D8): I lean toward extending `ticket_events` with a lease payload and new
   `type` values, to keep one timeline in the UI.
7. **How the product makes money** — unchanged from the business plan, and it still decides whether
   multi-tenant/auth/TLS are in scope.

---

## 13. Evidence index — `docs/test-results/phase-0/`

| File | What it is |
|---|---|
| `go-test-v.log` | full `go test ./... -count=1 -v` output (31 PASS / 0 FAIL / 0 SKIP, 74 RUN lines) |
| `go-test-summary.log`, `go-test-race.log`, `go-vet.log` | non-verbose run, `-race` run, `go vet` |
| `schema.sql` | `.schema` dump of a freshly migrated database |
| `schema-and-mcp-capture.txt` | table/index/FK lists, `PRAGMA user_version`=0, `schema_migrations` row, column scan for lease/lock/version, live MCP `initialize` + `tools/list` + error shapes |
| `mcp-tools-list.json` | raw `tools/list` JSON with full `inputSchema` and annotations |
| `rest-sse-probe.txt` | REST/SSE probe: unknown-field drop, `"in progress"` → 400, boards-events 404, raw SSE frames, timestamp collisions, cascade delete |
| `contention-probe.txt` | 20 concurrent PUTs (19×200 / 1×500), 20 spread PUTs (18×200 / 2×500) |
| `stdio-and-audit-probe.txt` | stdio session stdout (logs interleaved with JSON-RPC), audit event inventory, actor values |
| `sqlite-driver-probe.txt` | driver probe output: version 3.51.2, `RETURNING`, DSN pragmas on 8/8 conns, 17/40 vs 0/40 busy failures |
| `probe-driver.go.txt` | source of that probe |
| `sqlite-fk-per-connection-probe.txt` | `foreign_keys` = `[1 0 0 0 0 0 0 0]` across 8 simultaneous connections; dangling-FK inserts succeeding on 7 |
| `probe-fk-per-connection.go.txt` | source of that probe |
| `bind-address-probe.txt` | stdio mode still serving the API and UI on the HTTP port |
| `sse-stream-sample.txt` | raw 2-second SSE capture (`ping` + three `board_change` frames) |

Probes were run from `/tmp` against the repo binary; scratch databases live in `/tmp/recon/`.
Nothing scratch was committed. The probe programs are included as `.go.txt` because they are not part of
the module and must not be built by `go build ./...`.
