# Claim-protected versus open operations

The matrix spec §5.7 requires: for every operation the REST and MCP surfaces expose, whether it needs a
claim, whether it needs `expected_version`, whether it needs an idempotency key, whether it is audited,
which actor types may perform it, and what it does to the fencing generation.

This document is **normative** for both transports; `docs/contracts.md` §3.2 summarises it and defers to
it. The operation list comes from `docs/recon.md` §3 (MCP) and §4 (REST) — every route and every action
that exists today appears below, plus the new ownership operations contracted in Phase 1.

Rule IDs here are `C-OPEN-n` (open operations) and `C-GUARD-n` (the guard mechanics), so the test plan can
cite them the same way it cites `C-CLAIM-n`.

---

## 1. The one expression that decides claim-protection

Everything below reduces to a single ownership guard, and it is **always inline in the `UPDATE`'s
`WHERE`** — never a `SELECT` that then decides whether to write (`C-CONV-4`):

```sql
-- the "may I write this ticket's content?" guard
AND ( lease_holder IS NULL                       -- nobody owns it: open
      OR lease_expires_at <= :now_ms             -- lapsed: open (C-CLAIM-1)
      OR (lease_holder = :actor_id AND lease_generation = :generation) )  -- I own it, with a live token
```

| ID | Rule |
|---|---|
| `C-GUARD-1` | **Claim-protected** means exactly: *the caller must be the live lease holder when a live lease exists.* It does not mean "the ticket must be claimed". A ticket nobody owns is editable by anyone (with `expected_version`, below) — that is what keeps the board usable before an agent picks the work up. |
| `C-GUARD-2` | The guard is one boolean expression in the statement. Its three arms are: unclaimed, lapsed, and "holder with matching generation". There is no fourth arm and no grace window (`C-FENCE-10`). |
| `C-GUARD-3` | When the guard fails because someone else holds a live lease, the result is `409 already_claimed`; when it fails because the caller's generation is stale, `409 lease_generation_mismatch`; when it fails because the caller is not the holder and the ticket is live, `409 already_claimed` (with the holder's details). The diagnosis order is `C-CLAIM-22`. |
| `C-GUARD-4` | `expected_version` is **required for `agent` and `orchestrator`** on ticket content mutations, and **optional for `human`** (`C-FENCE-6`). Required-and-missing is `400 invalid_request`; stale is `409 version_conflict`. `[LEAD-DECIDED]` — an agent always reads a ticket before editing it (it had to, to know what to change), so requiring the version costs it nothing and closes the last lost-update hole: two agents editing the same *unclaimed* ticket. A human editing inline in the UI does not always have a version in hand. |
| `C-GUARD-5` | "Audited" below means a `ticket_events` row with a populated `actor` (`C-AUDIT-1`). Open does **not** mean invisible: every mutation in this document is audited except the reads. |

---

## 2. Ownership operations (new in Phase 2)

| Operation (REST · MCP) | Claim required? | `expected_version` | Idempotency key | Audited as | Actors | Generation effect |
|---|---|---|---|---|---|---|
| `POST /tickets/{id}/claim` · `claim{action:"claim"}` | no — this *is* the claim | no | **required** | `claimed` | agent, orchestrator | **+1**, old token dead |
| `POST /boards/{id}/claim-next` · `claim{action:"next"}` | no | no | **required** | `claimed` (source `claim_next`) | agent, orchestrator | **+1** |
| `POST /tickets/{id}/claim/renew` · `claim{action:"renew"}` | yes — holder, **live lease only** (`C-CLAIM-14`) | no | no | `claim_renewed` | agent, orchestrator | unchanged |
| `DELETE /tickets/{id}/claim` · `claim{action:"release"}` | yes — holder, **live or lapsed** (`C-CLAIM-18`) | no | no | `claim_released` (`reason:"release"`) | agent, orchestrator, human | **+1**, holder cleared |
| `GET /tickets/{id}/claim` · `claim{action:"get"}` | no | no | no | not audited (read) | all | none |
| `POST /tickets/{id}/reassign` · `claim{action:"reassign"}` | no — override | no | no | `claim_reassigned` (`override:true` when a live lease existed) | **human, orchestrator** | **+1**, old token dead |
| `POST /tickets/{id}/revoke` · `claim{action:"revoke"}` | no — override | no | no | `claim_revoked` | **human, orchestrator** | **+1**, holder cleared |
| `POST /tickets/{id}/unblock` · `ticket{action:"unblock"}` | no — override | optional | no | `unblocked` | **human, orchestrator** | **+1** |
| expiry sweep (internal) | n/a | n/a | n/a | `claim_released` (`reason:"expiry"`) | `system` | **+1**, holder cleared |
| `POST /tickets/{id}/handoffs` · `handoff{action:"create"}` | yes — holder (agent); no (human) | required for agent | **required** | `handoff_created` | agent, orchestrator, human | unchanged — the author keeps the lease (`C-HANDOFF-15`) |
| `POST /handoffs/{id}/accept` · `handoff{action:"accept"}` | **no** — the handoff is the authority | no | **required** | `handoff_accepted` | agent, orchestrator, human | **+1**, new holder |
| `POST /tickets/{id}/memory-writes` · `memory{action:"write"}` | yes — holder (agent); no (human) | required for agent | **required** | `outbox_enqueued` | agent, orchestrator, human | unchanged |

| ID | Rule |
|---|---|
| `C-GUARD-6` | `claim` and `claim_next` do **not** require `expected_version`: a claim is a transition on the ticket, not a write against a state the caller has read. Requiring it would also make the very first claim awkward for an agent that discovered the ticket by id. If supplied, it *is* enforced (`409 version_conflict`) — the field is never silently ignored. |
| `C-GUARD-7` | `reassign` over a live lease requires `override: true` in the body; without it, `409 already_claimed` with `details.override_required = true`. This is the only "are you sure" in the system, and it exists so that a bulk UI action cannot silently yank work from an agent mid-task. |
| `C-GUARD-8` | `revoke` and `reassign` are the two operations where a human takes work away. Both issue a new generation, so every token the old holder has is dead the instant they commit (`C-FENCE-9`). |
| `C-GUARD-13` | `release` is the one ownership operation whose guard contains **no clock**: the holder and the generation must match, and the lease may be live or lapsed (`C-CLAIM-18`). `renew` is its mirror image — the same two guards **plus** `lease_expires_at > :now_ms` (`C-CLAIM-14`). The asymmetry is deliberate and is the settled reading of `C-CLAIM-18`: renewing *extends* authority and must not survive a lapse, while releasing *surrenders* it, and a stale release can never end anyone else's lease because every new claim issues a higher generation (`C-INV-2`, `C-FENCE-9`). Neither operation may gain or lose its liveness guard without a contract revision. |

---

## 3. Ticket content and lifecycle

| Operation (REST · MCP) | Claim required? | `expected_version` | Idem key | Audited as | Actors | Generation effect |
|---|---|---|---|---|---|---|
| `GET /tickets/{id}` · `ticket{action:"get"}` | no | no | no | not audited | all | none |
| `GET /boards/{id}/tickets` · `ticket{action:"list"}` | no | no | no | not audited | all | none |
| `GET /boards/{id}/ready` · `board{action:"ready"}` | no | no | no | not audited | all | none |
| `GET /boards/{id}/context` · `board{action:"context"}` | no | no | no | not audited | all | none |
| `GET /boards/{id}/summary` · `board{action:"summary"}` | no | no | no | not audited | all | none |
| `GET /tickets/{id}/events` · `ticket{action:"history"}` | no | no | no | not audited | all | none |
| `POST /boards/{id}/tickets` · `ticket{action:"create"}` | no — nothing to claim yet | no | **optional** | `created` | agent, orchestrator, human | none (new row: `generation = 0`) |
| `POST /boards/{id}/tickets/bulk` · `ticket{action:"bulk_create"}` | no | no | **optional** | `created` per ticket | agent, orchestrator, human | none |
| `PUT /tickets/{id}` · `ticket{action:"update"}` (content, priority, assignee, resolution, epic) | **conditional** (`C-GUARD-1`) | **required** (agent/orchestrator) | no | `edited` | agent, orchestrator, human | none — a content edit does not invalidate a claim (`C-FENCE-11`) |
| `PUT /tickets/{id}` · `ticket{action:"update"}` (status change) | **conditional** | **required** (agent) | no | `moved`, plus `claim_released` (`reason:"status"`) when a claim ends | agent, orchestrator, human | **+1 and the holder is cleared** when the new status is `done` or `blocked`, or when the ticket leaves `blocked` while claimed; otherwise none |
| `DELETE /tickets/{id}` · `ticket{action:"delete"}` | **conditional** | **required** (agent) | no | *not audited* (`C-AUDIT-9`) | agent (own claim only), orchestrator, human | n/a — the row is gone, taking its audit trail with it |

| ID | Rule |
|---|---|
| `C-GUARD-9` | A status change to `done` or `blocked` (or out of `blocked`) **ends the claim in the same statement**: the holder is cleared and the generation is bumped. It ends the work the claim was for, so leaving the holder in place would break the invariant that a live lease implies `status = 'in_progress'` (`C-INV-4`). This is the "status change" entry of spec §5.5's override list, and it applies to **any** actor, not just humans — an agent marking its own ticket done releases its own lease, which is correct and intentional. |
| `C-GUARD-10` | Ticket delete by an `agent` requires it to hold the live claim (or the ticket to be unclaimed, with `expected_version`). Deletion cascades to tasks, notes, relations, handoffs and `ticket_events` (recon §2), so it is the most destructive single operation on the board. Humans and orchestrators may delete anything. |
| `C-GUARD-11` | Ticket create never requires a claim — there is nothing to claim. It is idempotency-keyed so that a retried create does not produce a duplicate ticket, which is the actual failure mode an agent fleet hits. |
| `C-GUARD-12` | Editing `assignee` is a content edit, not ownership: it never touches `lease_holder`, never requires a claim beyond `C-GUARD-1`, and never bumps the generation. The two fields stay separate (`C-SCHEMA-5`). |

---

## 4. Ticket children — tasks, notes, relations

These are the operations where forcing a claim would be needless friction, and the spec's guidance
("claim-protected mutations require the current fencing generation") lands on the narrow reading: protect
the ticket's own state, not every row that hangs off it.

| Operation (REST · MCP) | Claim required? | `expected_version` | Idem key | Audited as | Actors | Generation effect |
|---|---|---|---|---|---|---|
| `GET /tickets/{id}/tasks` · `task{action:"list"}` | no | no | no | not audited | all | none |
| `POST /tickets/{id}/tasks` · `task{action:"create"}` | **no** | no | no | `task_added` | all | none |
| `POST /tickets/{id}/tasks/bulk` · `task{action:"bulk_create"}` | **no** | no | no | `task_added` per title | all | none |
| `PUT /tasks/{id}` · `task{action:"update"}` (tick / rename) | **no** | no | no | `task_updated` | all | none |
| `DELETE /tasks/{id}` · `task{action:"delete"}` | **no** | no | no | `task_deleted` | all | none |
| `GET /tickets/{id}/notes` · `note{action:"list"}` | no | no | no | not audited | all | none |
| `POST /tickets/{id}/notes` · `note{action:"add"}` | **no** | no | no | `commented` | all | none |
| `GET /notes/{id}` · (via `note{action:"list"}`) | no | no | no | not audited | all | none |
| `PUT /notes/{id}` · `note{action:"update"}` | **no** | no | no | `comment_edited` | all | none |
| `DELETE /notes/{id}` · `note{action:"delete"}` | **no** | no | no | *not audited* (recon §5.1) | all | none |
| `GET /tickets/{id}/relations` · `relation{action:"list"}` | no | no | no | not audited | all | none |
| `POST /tickets/{id}/relations` · `relation{action:"add"}` | **no** | no | no | `relation_added` (both tickets) | all | none |
| `DELETE /tickets/{id}/relations/{toId}` · `relation{action:"delete"}` | **no** | no | no | `relation_removed` (both tickets) | all | none |

| ID | Rule |
|---|---|
| `C-OPEN-1` | **Ticking a checklist item is open.** `PUT /tasks/{id}` with `{"done":true}` on a claimed ticket by a non-holder is accepted. Rationale: a checklist row is a single boolean per row, so there is no lost update to prevent (two ticks of the same item are the same outcome); the write is scoped by task id, not by the ticket's mutable content; and the common real case — a reviewer or an orchestrator confirming one item — would otherwise require a claim transfer, which is exactly the friction spec §5.7 asks us to identify. The audit row names the actor, so the holder can see who ticked it. |
| `C-OPEN-2` | **Notes are the communication channel and are open.** An agent that cannot write a note cannot report anything: notes are the mechanism for "I found something", "I am blocked", "review this". Requiring a claim to comment would make the board unusable for the exact multi-agent conversation it exists to support. |
| `C-OPEN-3` | **Relations are open.** A blocking relation is a graph edit between two tickets, not a change to either ticket's content; adding a duplicate is already `409` (recon §4), so the operation is naturally idempotent-ish. An orchestrator must be able to add a `blocks` edge without owning either ticket. |
| `C-OPEN-4` | **Task and note *deletes* are open too** — the same reasoning as their creates, plus: a note or task is scoped by its own id, so the delete is not a ticket-content write. (Note delete remains unaudited today, which is a pre-existing gap in `C-AUDIT-9`, not a consequence of being open.) |
| `C-OPEN-5` | The cost of this choice is explicit: a non-holder can add noise to a claimed ticket. The mitigation is attribution (every one of these rows carries `actor`, and the UI timeline shows it), not locking. Locking would trade a visible annoyance for an invisible blocker. If the owner disagrees, the smallest change is a per-board config flag that applies `C-GUARD-1` to this whole section — no schema change. |
| `C-OPEN-6` | Task/note/relation writes do not carry `expected_version` because `version` belongs to the ticket row and these tables have no version column. Two actors renaming the same task concurrently is last-write-wins on one string field; that is acceptable for a checklist label and is audited. `[LEAD-DECIDED]` — adding per-row versions to `tasks`/`notes` would be a schema change with no demonstrated failure mode. |

---

## 5. Boards and epics

| Operation (REST · MCP) | Claim required? | `expected_version` | Idem key | Audited as | Actors | Generation effect |
|---|---|---|---|---|---|---|
| `GET /boards` · `board{action:"list"}` | no | no | no | not audited | all | none |
| `GET /boards/{id}` · `board{action:"get"}` | no | no | no | not audited | all | none |
| `POST /boards` · `board{action:"create"}` | no | no | no | *not audited* (`C-AUDIT-9`) | **human, orchestrator** | none |
| `PUT /boards/{id}` · `board{action:"update"}` | no | no | no | *not audited* | **human, orchestrator** | none |
| `DELETE /boards/{id}` · `board{action:"delete"}` | no | no | no | *not audited* | **human, orchestrator** | none (cascades tickets) |
| `GET /boards/{id}/epics` · `epic{action:"list"}` | no | no | no | not audited | all | none |
| `GET /epics/{id}` · `epic{action:"get"}` | no | no | no | not audited | all | none |
| `POST /boards/{id}/epics` · `epic{action:"create"}` | no | no | no | *not audited* | **human, orchestrator** | none |
| `PUT /epics/{id}` · `epic{action:"update"}` | no | no | no | *not audited* | **human, orchestrator** | none |
| `DELETE /epics/{id}` · `epic{action:"delete"}` | no | no | no | *not audited* | **human, orchestrator** | none |

| ID | Rule |
|---|---|
| `C-OPEN-7` | Board and epic structure is the operator's, not the fleet's: agents may read it and may file tickets into it, but may not create, rename or delete boards or epics (`C-AUTH-9`). This is a capability rule (`403`), not a claim rule. |
| `C-OPEN-8` | `DELETE /boards/{id}` cascades to every ticket, and therefore every claim, handoff and audit row on the board (recon §2). It is human/orchestrator only and should require an explicit confirmation in the UI. It does **not** issue generations: the rows are gone. Outbox rows survive (`C-SCHEMA-13`). |

---

## 6. Cross-cutting endpoints

| Operation | Claim required? | Idem key | Audited | Actors | Notes |
|---|---|---|---|---|---|
| `GET /api/v1/events` (SSE) · — | no | no | no | all | Global stream, `board_change` frames (`C-COMPAT-7`). Not per-board: recon R2, the README's `/boards/{id}/events` does not exist and is not added. |
| `GET /outbox` · `memory{action:"list"}` | no | no | no | human, orchestrator; agent limited to its own held tickets | `C-OUTBOX-23` |
| `GET /outbox/summary` | no | no | no | same | counts per status |
| `POST /outbox/{id}/retry` · `memory{action:"retry"}` | no | no | yes (on the origin ticket) | **human, orchestrator** | `C-OUTBOX-13` |
| `memory{action:"search"}` | no | no | no | all | `501 search_unsupported` when the adapter has no `Search` (`C-OUTBOX-20`) |

---

## 7. Where forcing a claim would be needless friction — the calls, and why

| Operation | Forcing a claim would mean | Why it is open |
|---|---|---|
| `PUT /tasks/{id}` (tick a checklist item) | an orchestrator or reviewer must take ownership of the ticket to confirm one item; the holder loses its lease to a `reassign` | single-boolean row, no lost update, audited by actor (`C-OPEN-1`) |
| `POST /tickets/{id}/notes` | an agent cannot report a blocker or a finding on a ticket it does not own — the one thing the board must never prevent | notes *are* the communication channel (`C-OPEN-2`) |
| `POST /tickets/{id}/relations` | a human cannot mark "A blocks B" without owning A or B | graph edit between two tickets, duplicate already `409` (`C-OPEN-3`) |
| `POST /boards/{id}/tickets` | a claim on a ticket that does not exist yet | nothing to protect (`C-GUARD-11`) |
| `GET …` (all reads) | reads would serialise behind ownership | reads never mutate; SSE is a cache invalidator |
| board/epic CRUD | n/a — capability, not claim | structure is the operator's (`C-OPEN-7`) |

And the mirror image — operations that **must** be claim-protected, with the failure they prevent:

| Operation | Failure without protection |
|---|---|
| `PUT /tickets/{id}` content/status | two agents overwrite each other's edits on the ticket itself — the exact collision Ledgerboard exists to prevent (recon §4.4) |
| `POST /tickets/{id}/handoffs` | a non-owner asserts what was done and hands over authority it never had |
| `DELETE /tickets/{id}` | an agent destroys work, audit trail and handoff history it does not own |
| `renew` / `release` | a stale process extends or ends a lease that no longer belongs to it |

---

## 8. Consistency checks this matrix must pass

| ID | Check |
|---|---|
| `C-OPEN-9` | Every REST route in recon §4 and every MCP action in recon §3 appears exactly once in §2–§6. A missing operation is a contract defect; QA should diff this document against `GET /api/v1/...` route registration and `tools/list`. |
| `C-OPEN-10` | Every "claim required? = yes" row also appears in `docs/contracts.md` §3.2's summary. The two documents must not disagree; `concurrency.md` wins if they do. |
| `C-OPEN-11` | No operation is both claim-protected and available to `human` without a claim in a way that would surprise the holder: a human override always bumps the generation when it changes ownership or ends the work (`C-GUARD-9`), and never silently leaves an agent holding a token for work that has moved. |
| `C-OPEN-12` | Every "Audited as" cell that says *not audited* is either a read (fine) or a listed gap in `C-AUDIT-9` (ticket delete, note delete, board/epic CRUD). There is no third category. |
