package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- helpers ---------------------------------------------------------------

// hashOf is the C-IDEM-10 request hash of a decoded request. Tests pass the
// payload they send, which is exactly what a transport hashes.
func hashOf(t *testing.T, v any) string {
	t.Helper()
	h, err := RequestHash(v)
	require.NoError(t, err)
	return h
}

// handoffBody builds a valid §5.1 document and applies overrides. A nil override
// value removes the field, which is how "absent" is expressed.
func handoffBody(t *testing.T, overrides map[string]any) json.RawMessage {
	t.Helper()
	doc := map[string]any{
		"schema_version": 1,
		"summary":        "Parser rewrite is done and green; next is wiring the CLI flag.",
		"done":           []string{"Rewrote the tokenizer", "Added 14 table tests"},
		"next":           []string{"Wire --strict flag"},
		"blockers":       []string{},
		"artifacts": []map[string]any{
			{"kind": "commit", "ref": "a1b2c3d", "description": "tokenizer rewrite"},
		},
		"memory_writes": []map[string]any{
			{"type": "finding", "content": "modernc.org/sqlite supports UPDATE ... RETURNING",
				"confidence": 0.9, "tags": []string{"sqlite", "driver"}},
		},
		"memory_refs": []string{"mem_01J9X8Z4QK"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return raw
}

// agentHandoff builds the body an agent must send: the document plus the two
// request-level fields the concurrency matrix requires.
func agentHandoff(t *testing.T, gen, version int64, overrides map[string]any) json.RawMessage {
	t.Helper()
	all := map[string]any{"generation": gen, "expected_version": version}
	for k, v := range overrides {
		all[k] = v
	}
	return handoffBody(t, all)
}

// newHandoffRequest assembles a creation request with a fresh key.
func newHandoffRequest(ticketID string, a models.Actor, key string, body json.RawMessage) CreateHandoffRequest {
	return CreateHandoffRequest{
		TicketID:  ticketID,
		Actor:     a,
		RequestID: "req-handoff",
		Payload:   body,
		Scope: IdempotencyScope{
			Actor:       a,
			Key:         key,
			RequestHash: mustHash(body),
		},
	}
}

func mustHash(v any) string {
	h, err := RequestHash(v)
	if err != nil {
		panic(err)
	}
	return h
}

// claimTicket claims a ticket and returns the claimed entity.
func claimTicket(t *testing.T, s *SQLiteStore, ticketID string, a models.Actor) ClaimedTicket {
	t.Helper()
	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a, RequestID: "req-claim"})
	require.NoError(t, err)
	return claimed
}

// eventsOfType returns the audit rows of the given types, oldest first
// (C-AUDIT-7 order, which ListTicketEvents already applies).
func eventsOfType(t *testing.T, s *SQLiteStore, ticketID string, types ...models.TicketEventType) []models.TicketEvent {
	t.Helper()
	events, err := s.ListTicketEvents(t.Context(), ticketID)
	require.NoError(t, err)
	want := map[models.TicketEventType]bool{}
	for _, typ := range types {
		want[typ] = true
	}
	var out []models.TicketEvent
	for _, ev := range events {
		if want[ev.Type] {
			out = append(out, ev)
		}
	}
	return out
}

// eventTypes lists the event types of the rows, in order, for sequence asserts.
func eventTypes(events []models.TicketEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = string(ev.Type)
	}
	return out
}

type outboxRow struct {
	ID             string
	HandoffID      string
	BoardID        string
	Actor          string
	ActorType      string
	MemoryType     string
	Content        string
	Confidence     float64
	TagsJSON       string
	Status         string
	Attempts       int64
	IDempotencyKey string
	RequestID      string
}

func outboxRowsFor(t *testing.T, s *SQLiteStore, handoffID string) []outboxRow {
	t.Helper()
	rows, err := s.db.Query(`
        SELECT id, COALESCE(handoff_id,''), board_id, actor, actor_type, memory_type, content,
               confidence, tags_json, status, attempts, idempotency_key, request_id
          FROM outbox WHERE handoff_id = ? ORDER BY rowid`, handoffID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.ID, &r.HandoffID, &r.BoardID, &r.Actor, &r.ActorType,
			&r.MemoryType, &r.Content, &r.Confidence, &r.TagsJSON, &r.Status, &r.Attempts,
			&r.IDempotencyKey, &r.RequestID))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func countRows(t *testing.T, s *SQLiteStore, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, s.db.QueryRow(query, args...).Scan(&n))
	return n
}

// ---- creation --------------------------------------------------------------

// TestHandoffCreateHappyPath is T-HAND-03 + T-HAND-05 at the store level: one
// transaction writes the handoff row, its three outbox rows and both audit rows,
// and the author keeps the lease (C-HANDOFF-14, C-HANDOFF-15, C-AUDIT-4).
func TestHandoffCreateHappyPath(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	boardID, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, a)

	body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, map[string]any{
		"memory_writes": []map[string]any{
			{"type": "finding", "content": "the tokenizer is 40% faster", "confidence": 0.8,
				"tags": []string{"perf"}},
			{"type": "decision", "content": "keep the recursive-descent parser"},
			{"type": "lesson", "content": "table tests caught two off-by-ones", "confidence": 1.0},
		},
	})

	res, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "handoff-key-1", body))
	require.NoError(t, err)
	doc := res.Document

	assert.False(t, res.Replayed)
	assert.Equal(t, HandoffStatusOffered, doc.Status)
	assert.Equal(t, HandoffSchemaVersion, doc.SchemaVersion)
	assert.Equal(t, ticketID, doc.TicketID)
	assert.Equal(t, boardID, doc.BoardID)
	assert.Equal(t, "agent:builder-1", doc.FromActor)
	assert.Equal(t, "agent", doc.FromActorType)
	assert.Equal(t, claimed.Claim.Generation, doc.Generation, "the generation the author held (C-SCHEMA-11)")
	assert.Nil(t, doc.ToActor)
	assert.Equal(t, "Parser rewrite is done and green; next is wiring the CLI flag.", doc.Summary)
	assert.Equal(t, []string{"Rewrote the tokenizer", "Added 14 table tests"}, doc.Done)
	assert.Len(t, doc.Artifacts, 1)
	assert.Len(t, doc.MemoryWrites, 3)
	assert.NotEmpty(t, doc.Checksum)

	// C-HANDOFF-14: the handoff exists together with its memory-write obligations.
	rows := outboxRowsFor(t, s, doc.ID)
	require.Len(t, rows, 3)
	for i, r := range rows {
		assert.Equal(t, "pending", r.Status, "C-OUTBOX-15: rows are created even with delivery off")
		assert.EqualValues(t, 0, r.Attempts)
		assert.Equal(t, r.ID, r.IDempotencyKey, "C-SCHEMA-14: the stable write id is the adapter key")
		assert.Equal(t, boardID, r.BoardID)
		assert.Equal(t, "agent:builder-1", r.Actor)
		assert.Equal(t, "agent", r.ActorType)
		assert.Equal(t, "req-handoff", r.RequestID)
		assert.Equal(t, doc.MemoryWrites[i].Type, r.MemoryType)
		assert.Equal(t, doc.MemoryWrites[i].Content, r.Content)
		assert.InDelta(t, doc.MemoryWrites[i].ConfidenceOr(), r.Confidence, 1e-9)
	}
	assert.Equal(t, 1.0, rows[1].Confidence, "C-HANDOFF-6: an absent confidence defaults to 1.0")
	assert.JSONEq(t, `["perf"]`, rows[0].TagsJSON)
	assert.JSONEq(t, `[]`, rows[1].TagsJSON)

	// C-HANDOFF-15: creating a handoff does not move ownership.
	row := readRow(t, s, ticketID)
	require.True(t, row.holder.Valid)
	assert.Equal(t, "builder-1", row.holder.String)
	assert.Equal(t, claimed.Claim.Generation, row.generation)
	assert.Equal(t, claimed.Version, row.version)

	// §1.8: handoff_created then outbox_enqueued, both after the claim.
	events := eventsOfType(t, s, ticketID, models.EventClaimed, models.EventHandoffCreated, models.EventOutboxEnqueued)
	assert.Equal(t, []string{"claimed", "handoff_created", "outbox_enqueued"}, eventTypes(events))

	createdPayload := events[1].Payload
	enqueuedPayload := events[2].Payload
	assert.Equal(t, doc.ID, createdPayload["handoff_id"])
	assert.EqualValues(t, 1, createdPayload["schema_version"])
	assert.Equal(t, "req-handoff", createdPayload["request_id"])
	assert.EqualValues(t, 3, enqueuedPayload["count"])
	assert.Len(t, enqueuedPayload["write_ids"], 3)

	ok, err := doc.VerifyChecksum()
	require.NoError(t, err)
	assert.True(t, ok, "C-HANDOFF-9: the stored checksum matches the creation-time field set")
}

// TestHandoffCreateAuthorityMatrix is T-HAND-02: who may create one, and with
// what proof.
func TestHandoffCreateAuthorityMatrix(t *testing.T) {
	t.Run("agent without generation is 400", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		body := handoffBody(t, map[string]any{"expected_version": claimed.Version})
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		e := requireCode(t, err, apperr.CodeInvalidRequest)
		assert.Equal(t, "generation", e.Details["field"])
	})

	t.Run("agent without expected_version is 400", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		body := handoffBody(t, map[string]any{"generation": claimed.Claim.Generation})
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		e := requireCode(t, err, apperr.CodeInvalidRequest)
		assert.Equal(t, "expected_version", e.Details["field"])
	})

	t.Run("agent not holding a live lease is already_claimed", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		holder := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, holder)

		other := agent("builder-2")
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, other, "k1", body))
		e := requireCode(t, err, apperr.CodeAlreadyClaimed)
		assert.Equal(t, "builder-1", e.Details["holder"], "C-ERR-4 detail set names the winner")
	})

	t.Run("stale generation is lease_generation_mismatch", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		// A human reassign to the same holder bumps the generation but not the
		// holder (C-CLAIM-25 route (a)), so the holder arm matches and the
		// generation arm does not.
		body := agentHandoff(t, claimed.Claim.Generation-1, claimed.Version, nil)
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		e := requireCode(t, err, apperr.CodeLeaseGenerationMismatch)
		assert.EqualValues(t, claimed.Claim.Generation, e.Details["current_generation"])
	})

	t.Run("stale expected_version is version_conflict", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version+7, nil)
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		e := requireCode(t, err, apperr.CodeVersionConflict)
		assert.EqualValues(t, claimed.Version+7, e.Details["expected_version"])
		assert.EqualValues(t, claimed.Version, e.Details["current_version"])
	})

	t.Run("lapsed lease is lease_expired", func(t *testing.T) {
		s, _, clock := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		clock.Advance(DefaultLeaseTTLMS + 1)
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		requireCode(t, err, apperr.CodeLeaseExpired)
	})

	// C-HANDOFF-13: the carve-out. A human and an orchestrator may create a
	// handoff with no lease at all, and the audit says who they were.
	for _, actorType := range []models.ActorType{models.ActorHuman, models.ActorOrchestrator} {
		t.Run(string(actorType)+" creates without a lease", func(t *testing.T) {
			s, _, _ := setupClaimStore(t)
			_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
			actor := models.Actor{Type: actorType, ID: "operator-1"}

			body := handoffBody(t, nil)
			res, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, actor, "k1", body))
			require.NoError(t, err)
			assert.Equal(t, string(actorType), res.Document.FromActorType)
			assert.Equal(t, string(actorType)+":operator-1", res.Document.FromActor)

			events := eventsOfType(t, s, ticketID, models.EventHandoffCreated)
			require.Len(t, events, 1)
			assert.Equal(t, string(actorType)+":operator-1", events[0].Actor, "C-AUDIT-1")
		})
	}

	t.Run("orchestrator holding the lease is fenced", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		orch := models.Actor{Type: models.ActorOrchestrator, ID: "fleet-1"}
		claimed := claimTicket(t, s, ticketID, orch)
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, orch, "k1", body))
		require.NoError(t, err)
	})

	t.Run("system may not create", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		sys := models.Actor{Type: models.ActorSystem, ID: "sweeper"}
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, sys, "k1", handoffBody(t, nil)))
		e := requireCode(t, err, apperr.CodeForbidden)
		assert.Equal(t, "handoff.create", e.Details["operation"])
	})

	t.Run("absent ticket is 404", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		a := agent("builder-1")
		_, err := s.CreateHandoff(t.Context(), newHandoffRequest("no-such-ticket", a, "k1", agentHandoff(t, 1, 1, nil)))
		requireCode(t, err, apperr.CodeNotFound)
	})
}

// TestHandoffValidationMatrix is T-HAND-01: every §5.2 rule, one sub-assert each,
// all at the service boundary (C-HANDOFF-1).
func TestHandoffValidationMatrix(t *testing.T) {
	longSummary := strings.Repeat("x", 4001)
	longContent := strings.Repeat("c", 2001)
	manyDone := make([]string, 51)
	for i := range manyDone {
		manyDone[i] = fmt.Sprintf("item %d", i)
	}
	bigItem := strings.Repeat("y", 500)
	bigArtifacts := make([]map[string]any, 50)
	for i := range bigArtifacts {
		bigArtifacts[i] = map[string]any{
			"kind":        "file",
			"ref":         strings.Repeat("r", 1000),
			"description": strings.Repeat("d", 500),
		}
	}
	bigList := make([]string, 50)
	for i := range bigList {
		bigList[i] = bigItem
	}
	manyTags := make([]string, 17)
	for i := range manyTags {
		manyTags[i] = fmt.Sprintf("tag%d", i)
	}

	cases := []struct {
		name      string
		overrides map[string]any
		wantField string
	}{
		{"schema_version 2", map[string]any{"schema_version": 2}, "schema_version"},
		{"empty summary", map[string]any{"summary": "   "}, "summary"},
		{"summary too long", map[string]any{"summary": longSummary}, "summary"},
		{"bad memory type", map[string]any{"memory_writes": []map[string]any{
			{"type": "factoid", "content": "x"}}}, "memory_writes[0].type"},
		{"confidence below range", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "confidence": -0.1}}}, "memory_writes[0].confidence"},
		{"confidence above range", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "confidence": 1.1}}}, "memory_writes[0].confidence"},
		{"confidence as string", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "confidence": "0.9"}}}, "memory_writes[0].confidence"},
		{"too many tags", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "tags": manyTags}}}, "memory_writes[0].tags"},
		{"tag too long", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "tags": []string{strings.Repeat("t", 65)}}}},
			"memory_writes[0].tags"},
		{"tag bad charset", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": "x", "tags": []string{"has space"}}}}, "memory_writes[0].tags"},
		{"memory content too long", map[string]any{"memory_writes": []map[string]any{
			{"type": "fact", "content": longContent}}}, "memory_writes[0].content"},
		{"to_actor too long", map[string]any{"to_actor": strings.Repeat("a", 201)}, "to_actor"},
		{"artifact credentials", map[string]any{"artifacts": []map[string]any{
			{"kind": "url", "ref": "https://user:pass@example.com/x"}}}, "artifacts[0].ref"},
		{"artifact bad kind", map[string]any{"artifacts": []map[string]any{
			{"kind": "gist", "ref": "abc"}}}, "artifacts[0].kind"},
		{"too many done items", map[string]any{"done": manyDone}, "done"},
		{"document too large", map[string]any{"done": bigList, "next": bigList, "artifacts": bigArtifacts}, "document"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := setupClaimStore(t)
			_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
			a := agent("builder-1")
			claimed := claimTicket(t, s, ticketID, a)
			body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, tc.overrides)

			_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
			e := requireCode(t, err, apperr.CodeHandoffInvalid)
			assert.Equal(t, tc.wantField, e.Details["field"], "details.field must name the offending field")
			assert.Equal(t, int64(0), countRows(t, s, `SELECT COUNT(*) FROM handoffs`), "nothing written")
		})
	}

	// R1 (C-HANDOFF-6): the NaN/Infinity arms of §5.3. JSON has no NaN or
	// Infinity literal, but the wire can still carry one: 1e400 and -1e400 overflow
	// float64, so they arrive as *numbers* and must be refused as handoff_invalid
	// naming the field — not as a generic "cannot unmarshal" document error.
	// checkJSONNumber catches them via strconv.ParseFloat's range error before the
	// typed decode, which is why the body is built as a raw string: json.Marshal
	// refuses math.Inf, so a Go map cannot express what a wire can.
	for _, lit := range []string{"1e400", "-1e400"} {
		t.Run("confidence is "+lit+" (overflows float64)", func(t *testing.T) {
			s, _, _ := setupClaimStore(t)
			_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
			a := agent("builder-1")
			claimed := claimTicket(t, s, ticketID, a)
			body := json.RawMessage(fmt.Sprintf(
				`{"schema_version":1,"generation":%d,"expected_version":%d,"summary":"overflowing confidence",`+
					`"memory_writes":[{"type":"fact","content":"x","confidence":%s}]}`,
				claimed.Claim.Generation, claimed.Version, lit))

			_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
			e := requireCode(t, err, apperr.CodeHandoffInvalid)
			assert.Equal(t, "memory_writes[0].confidence", e.Details["field"],
				"C-HANDOFF-6: the rejection names the field, not the document")
			assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM handoffs`), "nothing written")
			assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM outbox`), "no obligations written")
		})
	}

	// R1 (C-HANDOFF-7): duplicates collapse first-seen — in the stored document and
	// in the outbox row's tags_json, which is the value that reaches the operator's
	// memory. Neither side may carry "a" twice.
	t.Run("tags dedupe keeps first-seen order", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, map[string]any{
			"memory_writes": []map[string]any{
				{"type": "fact", "content": "x", "tags": []string{"a", "b", "a"}},
			},
		})

		res, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		require.NoError(t, err)
		require.Len(t, res.Document.MemoryWrites, 1)
		assert.Equal(t, []string{"a", "b"}, res.Document.MemoryWrites[0].Tags,
			"C-HANDOFF-7: duplicates collapse, first-seen order preserved")

		rows := outboxRowsFor(t, s, res.Document.ID)
		require.Len(t, rows, 1)
		assert.JSONEq(t, `["a","b"]`, rows[0].TagsJSON,
			"C-HANDOFF-7: the outbox row carries the deduped list")
	})

	t.Run("unknown field warns by default and fails in strict mode", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)
		body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, map[string]any{"summry": "typo"})

		res, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
		require.NoError(t, err, "C-HANDOFF-4: unknown fields warn by default")
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "summry")

		strictReq := newHandoffRequest(ticketID, a, "k2", body)
		strictReq.Strict = true
		_, err = s.CreateHandoff(t.Context(), strictReq)
		e := requireCode(t, err, apperr.CodeUnknownField)
		assert.Equal(t, []string{"summry"}, e.Details["fields"])
	})
}

// TestHandoffIdempotentCreateReplay is the create half of T-HAND-10: a replay is
// byte-identical and writes nothing a second time (C-IDEM-6, C-IDEM-9).
func TestHandoffIdempotentCreateReplay(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, a)
	body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)
	req := newHandoffRequest(ticketID, a, "replay-key", body)

	first, err := s.CreateHandoff(t.Context(), req)
	require.NoError(t, err)
	require.False(t, first.Replayed)

	second, err := s.CreateHandoff(t.Context(), req)
	require.NoError(t, err)
	assert.True(t, second.Replayed, "C-IDEM-6: same key + same hash replays")
	assert.Equal(t, first.Document, second.Document, "the replay returns the recorded document")

	assert.EqualValues(t, 1, countRows(t, s, `SELECT COUNT(*) FROM handoffs WHERE ticket_id = ?`, ticketID))
	assert.EqualValues(t, 1, countRows(t, s, `SELECT COUNT(*) FROM outbox WHERE ticket_id = ?`, ticketID))
	assert.Len(t, eventsOfType(t, s, ticketID, models.EventHandoffCreated), 1,
		"C-IDEM-9: a replay writes no second audit row")
	assert.EqualValues(t, 1, countRows(t, s,
		`SELECT COUNT(*) FROM ticket_events WHERE ticket_id = ? AND type = 'outbox_enqueued'`, ticketID))
}

// TestHandoffSupersession is T-HAND-07 at the store level.
func TestHandoffSupersession(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, a)

	first, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)))
	require.NoError(t, err)

	second, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k2",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version,
			map[string]any{"supersedes": first.Document.ID, "summary": "correction: the flag is --strict"})))
	require.NoError(t, err)

	oldDoc, err := s.GetHandoff(t.Context(), first.Document.ID)
	require.NoError(t, err)
	assert.Equal(t, HandoffStatusSuperseded, oldDoc.Status, "C-HANDOFF-11: the old row is superseded in the same tx")
	assert.Equal(t, HandoffStatusOffered, second.Document.Status)
	require.NotNil(t, second.Document.Supersedes)
	assert.Equal(t, first.Document.ID, *second.Document.Supersedes)

	events := eventsOfType(t, s, ticketID, models.EventHandoffSuperseded)
	require.Len(t, events, 1)
	payload := events[0].Payload
	assert.Equal(t, first.Document.ID, payload["handoff_id"])
	assert.Equal(t, second.Document.ID, payload["superseded_by"])

	// A superseded row cannot be superseded again (the cycle guard).
	_, err = s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k3",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version,
			map[string]any{"supersedes": first.Document.ID})))
	e := requireCode(t, err, apperr.CodeHandoffInvalid)
	assert.Equal(t, "supersedes", e.Details["field"])

	// C-HANDOFF-12: the chain is scoped to one ticket.
	_, otherTicket := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	otherClaim := claimTicket(t, s, otherTicket, a)
	_, err = s.CreateHandoff(t.Context(), newHandoffRequest(otherTicket, a, "k4",
		agentHandoff(t, otherClaim.Claim.Generation, otherClaim.Version,
			map[string]any{"supersedes": second.Document.ID})))
	e = requireCode(t, err, apperr.CodeHandoffInvalid)
	assert.Equal(t, "supersedes", e.Details["field"])
}

// ---- acceptance ------------------------------------------------------------

// TestHandoffAcceptTransfersOwnership is T-HAND-06: accept is the ownership
// transfer, and it needs no lease (C-HANDOFF-16, C-HANDOFF-17).
func TestHandoffAcceptTransfersOwnership(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	boardID, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	author := agent("builder-1")
	acceptor := agent("builder-2")
	claimed := claimTicket(t, s, ticketID, author)

	body := agentHandoff(t, claimed.Claim.Generation, claimed.Version,
		map[string]any{"to_actor": "agent:builder-2"})
	created, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, author, "k1", body))
	require.NoError(t, err)

	acceptReq := AcceptHandoffRequest{
		HandoffID: created.Document.ID,
		Actor:     acceptor,
		RequestID: "req-accept",
		Scope: IdempotencyScope{
			Actor: acceptor, Key: "accept-key-1", RequestHash: mustHash(created.Document.ID),
		},
	}
	accepted, err := s.AcceptHandoff(t.Context(), acceptReq)
	require.NoError(t, err)
	assert.False(t, accepted.Replayed)
	assert.Equal(t, HandoffStatusAccepted, accepted.Document.Status)
	require.NotNil(t, accepted.Document.AcceptedBy)
	assert.Equal(t, "agent:builder-2", *accepted.Document.AcceptedBy)
	require.NotNil(t, accepted.Document.AcceptedAt)
	assert.Equal(t, models.FormatMS(clock.NowMS()), *accepted.Document.AcceptedAt)

	row := readRow(t, s, ticketID)
	require.True(t, row.holder.Valid)
	assert.Equal(t, "builder-2", row.holder.String, "C-HANDOFF-16: the acceptor becomes the holder")
	require.True(t, row.holderType.Valid)
	assert.Equal(t, "agent", row.holderType.String)
	assert.Equal(t, claimed.Claim.Generation+1, row.generation, "the generation is bumped exactly once")
	assert.Equal(t, claimed.Version+1, row.version)
	assert.Equal(t, "in_progress", row.status)
	require.True(t, row.expiresAt.Valid)
	assert.Equal(t, clock.NowMS()+s.LeaseTTLMS(), row.expiresAt.Int64)
	assert.EqualValues(t, 0, row.renewals)

	var handoffID string
	require.NoError(t, s.db.QueryRow(`SELECT handoff_id FROM tickets WHERE id = ?`, ticketID).Scan(&handoffID))
	assert.Equal(t, created.Document.ID, handoffID, "C-HANDOFF-18")

	events := eventsOfType(t, s, ticketID, models.EventHandoffAccepted)
	require.Len(t, events, 1)
	payload := events[0].Payload
	assert.Equal(t, created.Document.ID, payload["handoff_id"])
	assert.EqualValues(t, claimed.Claim.Generation+1, payload["generation"])
	assert.EqualValues(t, claimed.Claim.Generation, payload["previous_generation"])
	assert.Equal(t, "agent:builder-1", payload["from_holder"])
	assert.Equal(t, "agent:builder-2", payload["to_holder"])

	// The author's old token is dead: its next claim-protected write fails.
	staleBody := agentHandoff(t, claimed.Claim.Generation, claimed.Version+1, nil)
	_, err = s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, author, "k2", staleBody))
	requireCode(t, err, apperr.CodeAlreadyClaimed)

	// Accepting twice is 409 (the handoff is no longer offered).
	_, err = s.AcceptHandoff(t.Context(), AcceptHandoffRequest{
		HandoffID: created.Document.ID, Actor: agent("builder-3"), RequestID: "req-accept-2",
		Scope: IdempotencyScope{Actor: agent("builder-3"), Key: "accept-key-2",
			RequestHash: mustHash("x")},
	})
	requireCode(t, err, apperr.CodeHandoffNotAcceptable)

	// C-HANDOFF-18: superseding does not move tickets.handoff_id.
	superseding, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, acceptor, "k3",
		agentHandoff(t, claimed.Claim.Generation+1, claimed.Version+1,
			map[string]any{"supersedes": created.Document.ID, "summary": "correction"})))
	require.NoError(t, err)
	assert.Equal(t, HandoffStatusSuperseded, mustGetHandoff(t, s, created.Document.ID).Status)
	require.NoError(t, s.db.QueryRow(`SELECT handoff_id FROM tickets WHERE id = ?`, ticketID).Scan(&handoffID))
	assert.Equal(t, created.Document.ID, handoffID)
	assert.Equal(t, HandoffStatusOffered, superseding.Document.Status)
	_ = boardID
}

func mustGetHandoff(t *testing.T, s *SQLiteStore, id string) Handoff {
	t.Helper()
	doc, err := s.GetHandoff(t.Context(), id)
	require.NoError(t, err)
	return doc
}

// TestHandoffAcceptWrongActor is the T7 rejection path: an offered handoff
// addressed to someone else may not be taken (C-HANDOFF-17).
func TestHandoffAcceptWrongActor(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	author := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, author)

	created, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, author, "k1",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version,
			map[string]any{"to_actor": "agent:builder-2"})))
	require.NoError(t, err)

	intruder := agent("builder-9")
	_, err = s.AcceptHandoff(t.Context(), AcceptHandoffRequest{
		HandoffID: created.Document.ID, Actor: intruder, RequestID: "req-x",
		Scope: IdempotencyScope{Actor: intruder, Key: "accept-x", RequestHash: mustHash("x")},
	})
	e := requireCode(t, err, apperr.CodeHandoffNotAcceptable)
	assert.Equal(t, "agent:builder-2", e.Details["to_actor"])
	assert.Equal(t, HandoffStatusOffered, e.Details["status"])

	// The ticket did not move.
	row := readRow(t, s, ticketID)
	require.True(t, row.holder.Valid)
	assert.Equal(t, "builder-1", row.holder.String)
	assert.Equal(t, claimed.Claim.Generation, row.generation)

	// Unknown handoff is 404, never a conflict (C-ERR-15).
	_, err = s.AcceptHandoff(t.Context(), AcceptHandoffRequest{
		HandoffID: "00000000000000000000000000000000", Actor: intruder, RequestID: "req-y",
		Scope: IdempotencyScope{Actor: intruder, Key: "accept-y", RequestHash: mustHash("y")},
	})
	requireCode(t, err, apperr.CodeNotFound)
}

// TestHandoffImmutability is T-HAND-04: no update path exists, and the checksum
// covers exactly the creation-time fields, so it survives accept unchanged
// (C-HANDOFF-9, C-HANDOFF-10, C-SCHEMA-9, C-SCHEMA-10).
func TestHandoffImmutability(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	author := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, author)

	created, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, author, "k1",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version,
			map[string]any{"to_actor": "agent:builder-2"})))
	require.NoError(t, err)
	before := created.Document
	storedBefore := countChecksum(t, s, before.ID)

	acceptor := agent("builder-2")
	_, err = s.AcceptHandoff(t.Context(), AcceptHandoffRequest{
		HandoffID: before.ID, Actor: acceptor, RequestID: "req-a",
		Scope: IdempotencyScope{Actor: acceptor, Key: "accept-1", RequestHash: mustHash("a")},
	})
	require.NoError(t, err)

	after := mustGetHandoff(t, s, before.ID)
	assert.Equal(t, HandoffStatusAccepted, after.Status)

	// Every creation-time field is byte-identical; only the three transition
	// fields moved.
	assert.Equal(t, before.Summary, after.Summary)
	assert.Equal(t, before.Done, after.Done)
	assert.Equal(t, before.Next, after.Next)
	assert.Equal(t, before.Blockers, after.Blockers)
	assert.Equal(t, before.Artifacts, after.Artifacts)
	assert.Equal(t, before.MemoryWrites, after.MemoryWrites)
	assert.Equal(t, before.MemoryRefs, after.MemoryRefs)
	assert.Equal(t, before.Generation, after.Generation)
	assert.Equal(t, before.CreatedAt, after.CreatedAt)
	assert.Equal(t, before.FromActor, after.FromActor)

	ok, err := after.VerifyChecksum()
	require.NoError(t, err)
	assert.True(t, ok, "the recomputed checksum still matches after accept")
	assert.Equal(t, storedBefore, after.Checksum, "accept leaves the stored checksum byte-identical")

	// A direct UPDATE of a creation-time field is detectable drift.
	_, err = s.db.Exec(`UPDATE handoffs SET summary = 'tampered' WHERE id = ?`, before.ID)
	require.NoError(t, err)
	tampered := mustGetHandoff(t, s, before.ID)
	ok, err = tampered.VerifyChecksum()
	require.NoError(t, err)
	assert.False(t, ok, "C-SCHEMA-10: drift in a creation-time field is detectable")
}

func countChecksum(t *testing.T, s *SQLiteStore, id string) string {
	t.Helper()
	var c string
	require.NoError(t, s.db.QueryRow(`SELECT checksum FROM handoffs WHERE id = ?`, id).Scan(&c))
	return c
}

// TestHandoffAcceptIdempotentReplay is the accept half of T-HAND-10: a replayed
// accept bumps no second generation and writes no second audit row.
func TestHandoffAcceptIdempotentReplay(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	author := agent("builder-1")
	acceptor := agent("builder-2")
	claimed := claimTicket(t, s, ticketID, author)
	created, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, author, "k1",
		agentHandoff(t, claimed.Claim.Generation, claimed.Version, nil)))
	require.NoError(t, err)

	req := AcceptHandoffRequest{
		HandoffID: created.Document.ID, Actor: acceptor, RequestID: "req-a",
		Scope: IdempotencyScope{Actor: acceptor, Key: "accept-key", RequestHash: mustHash("same")},
	}
	first, err := s.AcceptHandoff(t.Context(), req)
	require.NoError(t, err)
	require.False(t, first.Replayed)
	rowAfterFirst := readRow(t, s, ticketID)

	second, err := s.AcceptHandoff(t.Context(), req)
	require.NoError(t, err)
	assert.True(t, second.Replayed)
	assert.Equal(t, first.Document, second.Document)

	rowAfterSecond := readRow(t, s, ticketID)
	assert.Equal(t, rowAfterFirst.generation, rowAfterSecond.generation, "no second generation bump")
	assert.Equal(t, rowAfterFirst.version, rowAfterSecond.version)
	assert.Len(t, eventsOfType(t, s, ticketID, models.EventHandoffAccepted), 1)
}

// TestHandoffCreateWritesNothingOnInvalidMemoryWrite is T-HAND-05's second leg:
// validation happens before the insert, so a bad third memory write leaves no
// handoff and no partial outbox rows.
func TestHandoffCreateWritesNothingOnInvalidMemoryWrite(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, a)

	body := agentHandoff(t, claimed.Claim.Generation, claimed.Version, map[string]any{
		"memory_writes": []map[string]any{
			{"type": "fact", "content": "one"},
			{"type": "fact", "content": "two"},
			{"type": "nonsense", "content": "three"},
		},
	})
	_, err := s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "k1", body))
	e := requireCode(t, err, apperr.CodeHandoffInvalid)
	assert.Equal(t, "memory_writes[2].type", e.Details["field"])

	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM handoffs`))
	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM outbox`))
	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`),
		"C-IDEM-8: a rejected request records no key")
	assert.Len(t, eventsOfType(t, s, ticketID, models.EventHandoffCreated), 0)
	_ = context.Background()
}
