package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/clocksource"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/db"
	"github.com/rgracey/kanban-mcp/internal/memory"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- harness ---------------------------------------------------------------

// Child-process harness environment. The crash boundaries of C-OUTBOX-9 and
// C-OUTBOX-6 are only real if the process actually dies, so the child half is the
// test binary re-executed with the seam armed from the environment (the pattern
// TestIdempotencyCrashChild established in slice 2).
const (
	outboxChildEnv         = "LEDGERBOARD_TEST_OUTBOX_CHILD"
	outboxChildModeEnv     = "LEDGERBOARD_TEST_OUTBOX_MODE"
	outboxChildDBEnv       = "LEDGERBOARD_TEST_OUTBOX_DB"
	outboxChildCalls       = "LEDGERBOARD_TEST_OUTBOX_CALLS"
	outboxChildModeDeliver = "deliver"
	outboxChildModeHandoff = "handoff"
)

// outboxHarness is a migrated database plus the two files the delivery path
// shares with a child process: the mock's calls.jsonl and the fake clock.
type outboxHarness struct {
	t         *testing.T
	store     *SQLiteStore
	path      string
	clock     *testClock
	dir       string
	callsPath string
	clockFile string
}

func newOutboxHarness(t *testing.T, opts ...Option) *outboxHarness {
	t.Helper()
	s, path, clock := setupClaimStore(t, opts...)
	dir := t.TempDir()
	h := &outboxHarness{
		t: t, store: s, path: path, clock: clock, dir: dir,
		callsPath: filepath.Join(dir, "calls.jsonl"),
		clockFile: filepath.Join(dir, "clock.txt"),
	}
	h.syncClock()
	return h
}

// syncClock publishes the in-process clock to the file the child reads
// (clocksource's fake-clock seam), so parent and child agree on now_ms.
func (h *outboxHarness) syncClock() {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.clockFile,
		[]byte(strconv.FormatInt(h.clock.NowMS(), 10)), 0o600))
}

func (h *outboxHarness) advance(ms int64) {
	h.clock.Advance(ms)
	h.syncClock()
}

// mock builds a mock adapter whose calls land in the shared calls.jsonl.
func (h *outboxHarness) mock(cfg memory.MockConfig) *memory.MockAdapter {
	if cfg.CallsPath == "" {
		cfg.CallsPath = h.callsPath
	}
	return memory.NewMock(cfg)
}

// workerCfg is the worker configuration every test uses: a fixed worker id and a
// poll interval small enough that a loop test finishes in milliseconds.
func (h *outboxHarness) workerCfg(a memory.Adapter) OutboxWorkerConfig {
	return OutboxWorkerConfig{WorkerID: "worker-1", PollMS: 5, Adapter: a}
}

// calls reads calls.jsonl back as decoded adapter calls.
func (h *outboxHarness) calls() []memory.MockCall {
	h.t.Helper()
	raw, err := os.ReadFile(h.callsPath)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(h.t, err)
	var out []memory.MockCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var c memory.MockCall
		require.NoError(h.t, json.Unmarshal([]byte(line), &c), "calls.jsonl line: %s", line)
		out = append(out, c)
	}
	return out
}

// runChild executes the child half. When point is non-empty the child arms it
// and is expected to be SIGKILLed at it, which the FIFO marker proves.
func (h *outboxHarness) runChild(mode, point string) string {
	h.t.Helper()
	h.syncClock()

	fifo := filepath.Join(h.dir, "crash.fifo")
	if point != "" {
		require.NoError(h.t, syscall.Mkfifo(fifo, 0o600))
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestOutboxChildProcess")
	cmd.Env = append(os.Environ(),
		outboxChildEnv+"=1",
		outboxChildModeEnv+"="+mode,
		outboxChildDBEnv+"="+h.path,
		outboxChildCalls+"="+h.callsPath,
		clocksource.EnvClock+"="+clocksource.ModeFake,
		clocksource.EnvClockFile+"="+h.clockFile,
		crashpoint.EnvPoints+"="+point,
		crashpoint.EnvFIFO+"="+fifo,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(h.t, cmd.Start())

	if point == "" {
		// The handoff child kills itself after its transaction commits, so a
		// non-zero exit is expected; the output is what a failure needs.
		_ = cmd.Wait()
		return out.String()
	}

	lines := make(chan string, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		if err != nil {
			lines <- ""
			return
		}
		defer f.Close()
		buf := make([]byte, 256)
		n, _ := f.Read(buf)
		lines <- string(buf[:n])
	}()

	select {
	case line := <-lines:
		_ = cmd.Wait()
		require.Contains(h.t, line, "crash "+point, "child output:\n%s", out.String())
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		h.t.Fatalf("no crash marker for %s; child output:\n%s", point, out.String())
	}
	return out.String()
}

// TestOutboxChildProcess is the child half of the delivery harness. It does
// nothing unless the parent started it.
func TestOutboxChildProcess(t *testing.T) {
	if os.Getenv(outboxChildEnv) == "" {
		t.Skip("child process only")
	}
	crashpoint.Init()

	clock, err := clocksource.FromEnv()
	require.NoError(t, err)
	conn, err := db.Open(os.Getenv(outboxChildDBEnv))
	require.NoError(t, err)
	defer conn.Close()
	s := NewSQLiteStore(conn, WithClock(clock))

	switch os.Getenv(outboxChildModeEnv) {
	case outboxChildModeDeliver:
		mock := memory.NewMock(memory.MockConfig{CallsPath: os.Getenv(outboxChildCalls)})
		if _, err := s.ProcessOutboxOnce(context.Background(), OutboxWorkerConfig{
			WorkerID: "child-worker", PollMS: 5, Adapter: mock,
		}); err != nil {
			t.Fatalf("child delivery failed before reaching a crash point: %v", err)
		}
	case outboxChildModeHandoff:
		// One handoff with three memory writes, committed, then an immediate
		// SIGKILL: the crash lands after COMMIT and before any worker tick, which
		// is T-HAND-09's "post-commit" leg. The closed crashpoint vocabulary has no
		// generic tx.post_commit point (C-CONV-11 footnote), so the kill is direct.
		board, err := s.CreateBoard(context.Background(), "restart board", "")
		require.NoError(t, err)
		ticket, err := s.CreateTicket(context.Background(), board.ID, models.Ticket{
			Title: "restart ticket", Status: models.StatusTodo, Priority: models.PriorityMedium,
		})
		require.NoError(t, err)
		a := agent("builder-1")
		claimed, err := s.Claim(context.Background(),
			ClaimRequest{TicketID: ticket.ID, Actor: a, RequestID: "req-child-claim"})
		require.NoError(t, err)
		body := handoffBody(t, map[string]any{
			"generation":       claimed.Claim.Generation,
			"expected_version": claimed.Version,
			"memory_writes": []map[string]any{
				{"type": "fact", "content": "first write", "confidence": 1.0},
				{"type": "decision", "content": "second write", "confidence": 0.5, "tags": []string{"a"}},
				{"type": "finding", "content": "third write", "confidence": 0.25},
			},
		})
		_, err = s.CreateHandoff(context.Background(),
			newHandoffRequest(ticket.ID, a, "child-handoff", body))
		require.NoError(t, err)
		// Committed. Now die without a clean shutdown, exactly like a killed server.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	default:
		t.Fatalf("unknown child mode %q", os.Getenv(outboxChildModeEnv))
	}
}

// ---- helpers on the fixture -----------------------------------------------

// handoffWithWrites creates one claimed ticket and one handoff carrying n memory
// writes, and returns the ticket id and the outbox row ids in creation order.
func handoffWithWrites(t *testing.T, s *SQLiteStore, n int) (string, []string) {
	t.Helper()
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	claimed := claimTicket(t, s, ticketID, a)

	writes := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		writes = append(writes, map[string]any{
			"type": "fact", "content": fmt.Sprintf("write %d", i), "confidence": 0.75,
		})
	}
	body := agentHandoff(t, claimed.Claim.Generation, claimed.Version,
		map[string]any{"memory_writes": writes})
	res, err := s.CreateHandoff(t.Context(),
		newHandoffRequest(ticketID, a, "key-"+newUUID(), body))
	require.NoError(t, err)

	rows := outboxRowsFor(t, s, res.Document.ID)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ticketID, ids
}

// auditPayloads returns the payloads of the given audit types on a ticket.
func auditPayloads(t *testing.T, s *SQLiteStore, ticketID string, types ...models.TicketEventType) []map[string]any {
	t.Helper()
	events := eventsOfType(t, s, ticketID, types...)
	out := make([]map[string]any, len(events))
	for i, ev := range events {
		out[i] = ev.Payload
	}
	return out
}

// ---- C-OUTBOX-14 / 15: delivery off by default -----------------------------

// TestOutboxDeliveryOffByDefault is the store-level half of T-MEM-01/T-MEM-05:
// with delivery off nothing is delivered, nothing is failed, nothing is dropped,
// and the backlog is visible. It also proves C-OUTBOX-18's refusal to start a
// worker with no adapter.
func TestOutboxDeliveryOffByDefault(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 3)

	// No worker was started: the rows are pending, unclaimed, un-attempted.
	for _, id := range ids {
		row, err := h.store.GetOutboxRow(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, OutboxPending, row.Status)
		assert.EqualValues(t, 0, row.Attempts)
		assert.Nil(t, row.NextAttemptAt)
		assert.Nil(t, row.LeaseOwner)
		assert.Nil(t, row.ExternalID)
		assert.Nil(t, row.DeliveredAt)
	}

	// The backlog is visible, per status and per ticket (C-OUTBOX-15, C-OUTBOX-23).
	all, err := h.store.ListOutboxRows(t.Context(), OutboxFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 3)
	pending, err := h.store.ListOutboxRows(t.Context(), OutboxFilter{Status: OutboxPending})
	require.NoError(t, err)
	assert.Len(t, pending, 3)
	byTicket, err := h.store.ListOutboxRows(t.Context(), OutboxFilter{TicketID: ticketID})
	require.NoError(t, err)
	assert.Len(t, byTicket, 3)
	summary, err := h.store.OutboxSummary(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary[OutboxPending])
	assert.EqualValues(t, 0, summary[OutboxSucceeded])
	assert.EqualValues(t, 0, summary[OutboxFailed])

	// Nothing was delivered and nothing was audited as a delivery.
	assert.Empty(t, h.calls(), "no adapter call may happen with delivery off")
	assert.Empty(t, auditPayloads(t, h.store, ticketID,
		models.EventMemoryDelivered, models.EventMemoryDeliveryFailed))

	// C-OUTBOX-18: a worker with no adapter refuses to start and changes nothing.
	_, err = h.store.ProcessOutboxOnce(t.Context(), OutboxWorkerConfig{WorkerID: "w"})
	require.ErrorIs(t, err, ErrNoMemoryAdapter)
	require.ErrorIs(t, h.store.StartOutboxWorker(t.Context(), OutboxWorkerConfig{WorkerID: "w"}),
		ErrNoMemoryAdapter)
	require.ErrorIs(t, h.store.RunOutboxWorker(t.Context(), OutboxWorkerConfig{WorkerID: "w"}),
		ErrNoMemoryAdapter)
	assert.Empty(t, h.calls())
}

// ---- C-OUTBOX-2 / 24: the success path ------------------------------------

// TestOutboxSuccessPath is the store-level half of T-MEM-02: pending -> processing
// (attempts incremented at claim) -> succeeded, with external_id, delivered_at and
// exactly one memory_delivered audit, and an adapter that saw nothing but
// MemoryWrite (C-OUTBOX-22).
func TestOutboxSuccessPath(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)

	mock := h.mock(memory.MockConfig{})
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
	require.NoError(t, err)
	require.True(t, processed)

	got, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, got.Status)
	assert.EqualValues(t, 1, got.Attempts, "attempts increment at claim time")
	assert.Equal(t, memory.ExternalIDFor(writeID), *got.ExternalID)
	require.NotNil(t, got.DeliveredAt)
	assert.EqualValues(t, h.clock.NowMS(), *got.DeliveredAt)
	assert.Nil(t, got.LeaseOwner)
	assert.Nil(t, got.LeaseExpires)
	assert.Nil(t, got.LastError)

	// Exactly one memory_delivered audit, with §1.8's payload.
	payloads := auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered)
	require.Len(t, payloads, 1)
	assert.Equal(t, row.RequestID, payloads[0]["request_id"])
	assert.Equal(t, writeID, payloads[0]["write_id"])
	assert.Equal(t, memory.ExternalIDFor(writeID), payloads[0]["external_id"])
	assert.EqualValues(t, 1, payloads[0]["attempts"])

	// calls.jsonl shows the MemoryWrite exactly: id == outbox id, and no field
	// outside §6.6's struct (C-OUTBOX-22).
	calls := h.calls()
	require.Len(t, calls, 1)
	call := calls[0]
	assert.Equal(t, "write", call.Op)
	assert.Equal(t, writeID, call.ID)
	assert.Equal(t, row.TicketID, call.TicketID)
	assert.Equal(t, row.HandoffID, call.HandoffID)
	assert.Equal(t, row.Actor, call.Actor)
	assert.Equal(t, row.ActorType, call.ActorType)
	assert.Equal(t, row.MemoryType, call.Type)
	assert.Equal(t, row.Content, call.Content)
	assert.InDelta(t, row.Confidence, call.Confidence, 1e-9)
	assert.Equal(t, row.Tags, call.Tags)
	assert.Equal(t, models.FormatMS(row.CreatedAt), call.CreatedAt)
	assert.Equal(t, memory.ExternalIDFor(writeID), call.ExternalID)

	// A second pass has nothing left to claim: a succeeded row is never re-claimed.
	processed, err = h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Len(t, h.calls(), 1)
}

// ---- C-OUTBOX-10 / 11 / 16: classification and the retry schedule ---------

// TestOutboxFailureClassification is the store-level half of T-MEM-03: explicit
// classification, scrubbed last_error, and a timeout that is uncertain even when
// the adapter calls itself retryable.
func TestOutboxFailureClassification(t *testing.T) {
	t.Run("retryable schedules C-OUTBOX-10's first backoff", func(t *testing.T) {
		h := newOutboxHarness(t)
		ticketID, ids := handoffWithWrites(t, h.store, 1)
		mock := h.mock(memory.MockConfig{
			Behaviour:    memory.BehaviourRetryable,
			ErrorMessage: "dial tcp: connection refused; Authorization: Bearer sekrit-token",
		})
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxRetryable, row.Status)
		assert.EqualValues(t, 1, row.Attempts)
		require.NotNil(t, row.NextAttemptAt)
		assert.EqualValues(t, h.clock.NowMS()+30000, *row.NextAttemptAt)
		assert.Nil(t, row.LeaseOwner, "the attempt lease is released on failure")
		require.NotNil(t, row.LastError)
		assert.LessOrEqual(t, len(*row.LastError), 500)
		assert.Contains(t, *row.LastError, memory.CodeAdapterUnavailable)
		assert.NotContains(t, *row.LastError, "sekrit-token", "C-SCHEMA-16: credentials are scrubbed")
		assert.Contains(t, *row.LastError, "[redacted]")
		// C-OUTBOX-24: a retryable transition is not audited per attempt.
		assert.Empty(t, auditPayloads(t, h.store, ticketID,
			models.EventMemoryDelivered, models.EventMemoryDeliveryFailed))
	})

	t.Run("permanent rejection dead-letters immediately and is audited once", func(t *testing.T) {
		h := newOutboxHarness(t)
		ticketID, ids := handoffWithWrites(t, h.store, 1)
		mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourPermanent, ErrorMessage: "invalid payload"})
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxFailed, row.Status, "Retryable=false goes straight to failed")
		assert.Nil(t, row.NextAttemptAt)
		payloads := auditPayloads(t, h.store, ticketID, models.EventMemoryDeliveryFailed)
		require.Len(t, payloads, 1)
		assert.Equal(t, memory.CodeAdapterRejected, payloads[0]["error_code"])
		assert.EqualValues(t, 1, payloads[0]["attempts"])

		// A failed row is terminal: the worker never claims it again (C-OUTBOX-4).
		processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
		require.NoError(t, err)
		assert.False(t, processed)
	})

	t.Run("timeout is uncertain even when flagged retryable", func(t *testing.T) {
		h := newOutboxHarness(t)
		_, ids := handoffWithWrites(t, h.store, 1)
		mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourTimeout})
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxUncertain, row.Status,
			"C-OUTBOX-11: a timeout may have arrived, so it is never a definite failure")
	})

	t.Run("unclassified error is uncertain", func(t *testing.T) {
		h := newOutboxHarness(t)
		_, ids := handoffWithWrites(t, h.store, 1)
		mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourUnclassified})
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxUncertain, row.Status)
	})

	t.Run("classification ignores message text", func(t *testing.T) {
		// Two different messages with the same code must behave identically
		// (C-OUTBOX-11: never string matching).
		statuses := make([]string, 0, 2)
		for _, msg := range []string{"first message", "a completely different one"} {
			h := newOutboxHarness(t)
			_, ids := handoffWithWrites(t, h.store, 1)
			mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourRetryable, ErrorMessage: msg})
			_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
			require.NoError(t, err)
			row, err := h.store.GetOutboxRow(t.Context(), ids[0])
			require.NoError(t, err)
			statuses = append(statuses, row.Status)
		}
		assert.Equal(t, []string{OutboxRetryable, OutboxRetryable}, statuses)
	})
}

// TestOutboxRetryScheduleAndExhaustion is the store-level half of T-MEM-04: the
// fake clock walks C-OUTBOX-10's schedule, and attempt 8 dead-letters the row.
func TestOutboxRetryScheduleAndExhaustion(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourRetryable})
	want := []int64{30000, 120000, 600000, 1800000, 7200000, 21600000, 21600000}

	for i, backoff := range want {
		attempt := int64(i + 1)
		processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)
		require.True(t, processed, "attempt %d should be claimable", attempt)

		row, err := h.store.GetOutboxRow(t.Context(), writeID)
		require.NoError(t, err)
		require.Equal(t, OutboxRetryable, row.Status, "attempt %d", attempt)
		require.EqualValues(t, attempt, row.Attempts)
		require.NotNil(t, row.NextAttemptAt)
		assert.EqualValues(t, h.clock.NowMS()+backoff, *row.NextAttemptAt, "attempt %d backoff", attempt)

		// Not claimable before its next_attempt_at, claimable at it.
		processed, err = h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)
		assert.False(t, processed, "attempt %d must not be claimable early", attempt)

		h.advance(backoff)
	}

	// Attempt 8 exhausts the schedule and dead-letters the row.
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
	require.NoError(t, err)
	require.True(t, processed)

	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxFailed, row.Status)
	assert.EqualValues(t, OutboxMaxAttempts, row.Attempts)
	assert.Nil(t, row.NextAttemptAt)

	// Terminal: never claimed again, and exactly one delivery-failure audit.
	processed, err = h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Len(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDeliveryFailed), 1)
	assert.Empty(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered))
}

// ---- C-OUTBOX-13: operator retry of a dead letter -------------------------

// TestOutboxDeadLetterOperatorRetry is the store-level half of T-MEM-07: the
// reset, the audit, the 409 for a succeeded row, the 403 for an agent, and the
// retried row delivering normally.
func TestOutboxDeadLetterOperatorRetry(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	// Dead-letter it with a permanent rejection.
	_, err := h.store.ProcessOutboxOnce(t.Context(),
		h.workerCfg(h.mock(memory.MockConfig{Behaviour: memory.BehaviourPermanent})))
	require.NoError(t, err)

	// C-AUTH-7: an agent may not recover a dead letter.
	_, err = h.store.RetryOutboxRow(t.Context(), OutboxRetryRequest{
		WriteID: writeID, Actor: agent("builder-1"), RequestID: "req-retry",
	})
	requireCode(t, err, apperr.CodeForbidden)

	// The human reset.
	human := models.Actor{Type: models.ActorHuman, ID: "alice"}
	row, err := h.store.RetryOutboxRow(t.Context(), OutboxRetryRequest{
		WriteID: writeID, Actor: human, RequestID: "req-retry",
	})
	require.NoError(t, err)
	assert.Equal(t, OutboxPending, row.Status)
	assert.EqualValues(t, 0, row.Attempts)
	assert.Nil(t, row.NextAttemptAt)
	assert.Nil(t, row.LastError)
	assert.Nil(t, row.LeaseOwner)

	// Audited (C-OUTBOX-13), and the delivery-failure audit is not duplicated.
	retried := auditPayloads(t, h.store, ticketID, models.EventOutboxRetried)
	require.Len(t, retried, 1)
	assert.Equal(t, writeID, retried[0]["write_id"])
	assert.Equal(t, OutboxFailed, retried[0]["previous_status"])
	assert.Len(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDeliveryFailed), 1)

	// The retried row delivers normally.
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
	require.NoError(t, err)
	require.True(t, processed)
	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, row.Status)

	// A succeeded row is not retryable: 409 outbox_not_retryable (C-OUTBOX-13).
	_, err = h.store.RetryOutboxRow(t.Context(), OutboxRetryRequest{
		WriteID: writeID, Actor: human, RequestID: "req-retry-2",
	})
	requireCode(t, err, apperr.CodeOutboxNotRetryable)

	// An unknown id is 404, and no audit row is invented for it.
	_, err = h.store.RetryOutboxRow(t.Context(), OutboxRetryRequest{
		WriteID: newUUID(), Actor: human, RequestID: "req-retry-3",
	})
	requireCode(t, err, apperr.CodeNotFound)
}

// ---- C-OUTBOX-24: audit economy ------------------------------------------

// TestOutboxAuditEconomy is the store-level half of T-MEM-11: a row that flaps
// through several retryable transitions and then succeeds writes exactly one
// memory_delivered audit, and a row that dead-letters writes exactly one
// memory_delivery_failed. The attempt counter is the record for everything in
// between (C-OUTBOX-24).
func TestOutboxAuditEconomy(t *testing.T) {
	t.Run("flapping then success writes one memory_delivered", func(t *testing.T) {
		h := newOutboxHarness(t)
		ticketID, ids := handoffWithWrites(t, h.store, 1)

		mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourRetryable})
		for i := 0; i < 2; i++ {
			_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
			require.NoError(t, err)
			h.advance(OutboxBackoffMS(int64(i + 1)))
		}
		mock.SetBehaviour(memory.BehaviourOK, "")
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxSucceeded, row.Status)
		assert.EqualValues(t, 3, row.Attempts, "three attempts, one audit row")

		payloads := auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered)
		require.Len(t, payloads, 1, "retries within retryable write no audit row (C-OUTBOX-24)")
		assert.EqualValues(t, 3, payloads[0]["attempts"])
		assert.Empty(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDeliveryFailed))
	})

	t.Run("dead letter writes one memory_delivery_failed", func(t *testing.T) {
		h := newOutboxHarness(t)
		ticketID, ids := handoffWithWrites(t, h.store, 1)

		mock := h.mock(memory.MockConfig{Behaviour: memory.BehaviourRetryable})
		for i := 0; i < 3; i++ {
			_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
			require.NoError(t, err)
			h.advance(OutboxBackoffMS(int64(i + 1)))
		}
		mock.SetBehaviour(memory.BehaviourPermanent, "rejected")
		_, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)

		row, err := h.store.GetOutboxRow(t.Context(), ids[0])
		require.NoError(t, err)
		assert.Equal(t, OutboxFailed, row.Status)
		assert.EqualValues(t, 4, row.Attempts)

		require.Len(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDeliveryFailed), 1)
		assert.Empty(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered))
	})
}

// ---- C-OUTBOX-5 / 6 / 7: recovery, uncertainty and fencing ----------------

// TestOutboxStuckWorkerRecovery is the single-process subset of T-MEM-06: a
// worker claims a row and dies after the adapter returned but before the ack
// (crashpoint memory.post_adapter_write). The row stays processing; the clock
// passes its 60 s lease; the next worker reclaims it as `uncertain` first
// (C-OUTBOX-6), visible in the status listing, and only then delivers.
func TestOutboxStuckWorkerRecovery(t *testing.T) {
	h := newOutboxHarness(t)
	_, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	h.runChild(outboxChildModeDeliver, crashpoint.MemoryPostAdapterWrite)

	// The child delivered to the far end and died before any local ack.
	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxProcessing, row.Status, "the attempt lease is still held by the dead worker")
	require.NotNil(t, row.LeaseOwner)
	assert.Equal(t, "child-worker", *row.LeaseOwner)
	assert.EqualValues(t, 1, row.Attempts)
	assert.Nil(t, row.ExternalID, "C-OUTBOX-2: no ack committed, so no external id")
	assert.Nil(t, row.DeliveredAt)
	assert.Len(t, h.calls(), 1)

	// Not reclaimable yet: the 60 s attempt lease has not lapsed.
	reclaimed, err := h.store.ReclaimExpiredOutboxRows(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 0, reclaimed)

	// Past the lease, the pre-pass marks it uncertain — and that state is visible
	// before anything resolves it (C-OUTBOX-3, C-OUTBOX-6).
	h.advance(OutboxAttemptLeaseMS + 1)
	reclaimed, err = h.store.ReclaimExpiredOutboxRows(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, reclaimed)

	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxUncertain, row.Status)
	assert.GreaterOrEqual(t, row.Attempts, int64(1))
	assert.Nil(t, row.ExternalID)
	// The lapsed lease is cleared with the status, so an `uncertain` row is never
	// rendered as leased to a worker that is gone (§6.2, C-OUTBOX-3).
	assert.Nil(t, row.LeaseOwner)
	assert.Nil(t, row.LeaseExpires)

	uncertainRows, err := h.store.ListOutboxRows(t.Context(), OutboxFilter{Status: OutboxUncertain})
	require.NoError(t, err)
	require.Len(t, uncertainRows, 1)
	assert.Equal(t, writeID, uncertainRows[0].ID)
	summary, err := h.store.OutboxSummary(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary[OutboxUncertain])
	assert.EqualValues(t, 0, summary[OutboxSucceeded], "uncertain is never reported as success")

	// The next worker reclaims and delivers it with the same write id.
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
	require.NoError(t, err)
	require.True(t, processed)

	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, row.Status)
	assert.EqualValues(t, 2, row.Attempts)
	assert.Equal(t, memory.ExternalIDFor(writeID), *row.ExternalID)
}

// TestOutboxAckFencing is C-OUTBOX-7 at the store level: a worker whose lease was
// taken writes no external id and reports no success, and the row keeps the
// outcome of whoever holds it now.
func TestOutboxAckFencing(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	// Worker A claims and then stalls.
	stale, found, err := h.store.ClaimOutboxRow(t.Context(), "worker-a")
	require.NoError(t, err)
	require.True(t, found)

	// Its lease lapses; worker B reclaims.
	h.advance(OutboxAttemptLeaseMS + 1)
	_, err = h.store.ReclaimExpiredOutboxRows(t.Context())
	require.NoError(t, err)
	current, found, err := h.store.ClaimOutboxRow(t.Context(), "worker-b")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, writeID, current.ID)

	// A's late ack must change nothing and audit nothing.
	acked, err := h.store.AckOutboxRow(t.Context(), stale, "worker-a", "mem_stale")
	require.NoError(t, err)
	assert.False(t, acked)

	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxProcessing, row.Status)
	assert.Nil(t, row.ExternalID)
	assert.Empty(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered))

	// A's late failure must not steal the row either.
	failed, err := h.store.FailOutboxRow(t.Context(), stale, "worker-a",
		OutboxFailure{Status: OutboxRetryable, Code: "adapter_unavailable", Message: "late"})
	require.NoError(t, err)
	assert.False(t, failed)
	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxProcessing, row.Status)
	assert.Equal(t, "worker-b", *row.LeaseOwner)

	// B's ack is the one that counts.
	acked, err = h.store.AckOutboxRow(t.Context(), current, "worker-b", memory.ExternalIDFor(writeID))
	require.NoError(t, err)
	assert.True(t, acked)
	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, row.Status)
	assert.Len(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered), 1)
}

// ---- C-OUTBOX-8 / 9: at-least-once with a stable id -----------------------

// TestOutboxAtLeastOnceStableIDDedup is the store-level half of T-MEM-08: the
// crashpoint kills the worker after the adapter accepted the write and before the
// local ack committed (memory.pre_ack_commit). The retry delivers the same write
// id again, the far end deduplicates it, and the board ends with exactly one
// succeeded row and one memory_delivered audit — never a success it cannot prove.
func TestOutboxAtLeastOnceStableIDDedup(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	h.runChild(outboxChildModeDeliver, crashpoint.MemoryPreAckCommit)

	// The far end has the write; the board cannot prove it yet.
	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxProcessing, row.Status)
	assert.Nil(t, row.ExternalID)
	assert.Nil(t, row.DeliveredAt)
	assert.Empty(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered))

	// The lease lapses, the row goes uncertain, and a later worker retries it.
	h.advance(OutboxAttemptLeaseMS + 1)
	_, err = h.store.ReclaimExpiredOutboxRows(t.Context())
	require.NoError(t, err)
	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	require.Equal(t, OutboxUncertain, row.Status)

	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
	require.NoError(t, err)
	require.True(t, processed)

	row, err = h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, row.Status)
	assert.EqualValues(t, 2, row.Attempts)

	// calls.jsonl: the same write id twice, the same external id both times.
	calls := h.calls()
	require.Len(t, calls, 2, "at-least-once delivery: the write reached the far end twice")
	assert.Equal(t, writeID, calls[0].ID)
	assert.Equal(t, writeID, calls[1].ID)
	assert.Equal(t, calls[0].ExternalID, calls[1].ExternalID,
		"C-IDEM-5: a repeated write id must return the same external id")
	assert.Equal(t, memory.ExternalIDFor(writeID), calls[1].ExternalID)

	// Exactly one succeeded row and exactly one memory_delivered audit.
	require.Len(t, auditPayloads(t, h.store, ticketID, models.EventMemoryDelivered), 1)
	assert.EqualValues(t, 1, countRows(t, h.store,
		`SELECT COUNT(*) FROM outbox WHERE status = 'succeeded'`))
}

// ---- C-OUTBOX-15 / 16 + T-HAND-09's delivery leg: restart survival --------

// TestOutboxRestartSurvivalDelivery is T-HAND-09's delivery leg at the store
// level: a handoff with three memory writes commits and the process is SIGKILLed
// before any worker runs. On restart the handoff, its three outbox rows and its
// audit rows are intact, and switching delivery on drains the backlog in
// created_at order (C-OUTBOX-16).
func TestOutboxRestartSurvivalDelivery(t *testing.T) {
	h := newOutboxHarness(t)

	// The child creates the board, ticket, claim and handoff, then dies.
	out := h.runChild(outboxChildModeHandoff, "")
	require.NotContains(t, out, "FAIL", "child output:\n%s", out)

	// Reopen on the same file: the handoff, its three obligations and the audits
	// all survived the kill (C-HANDOFF-14, C-COMPAT-4).
	var handoffID, ticketID string
	require.NoError(t, h.store.db.QueryRow(
		`SELECT id, ticket_id FROM handoffs ORDER BY created_at ASC, rowid ASC LIMIT 1`).
		Scan(&handoffID, &ticketID))
	rows := outboxRowsFor(t, h.store, handoffID)
	require.Len(t, rows, 3, "C-HANDOFF-14: the handoff never exists without its obligations")
	for _, r := range rows {
		assert.Equal(t, OutboxPending, r.Status)
		assert.EqualValues(t, 0, r.Attempts)
	}
	assert.Len(t, auditPayloads(t, h.store, ticketID, models.EventHandoffCreated), 1)
	assert.Len(t, auditPayloads(t, h.store, ticketID, models.EventOutboxEnqueued), 1)

	// Two more handoffs at later instants, so delivery order is observable: the
	// three writes of the first handoff share one created_at, later ones do not.
	human := models.Actor{Type: models.ActorHuman, ID: "alice"}
	var laterIDs []string
	for i := 0; i < 2; i++ {
		h.advance(1000)
		body := handoffBody(t, map[string]any{
			"summary": fmt.Sprintf("follow-up handoff %d", i),
			"memory_writes": []map[string]any{
				{"type": "fact", "content": fmt.Sprintf("later write %d", i), "confidence": 1.0},
			},
		})
		res, err := h.store.CreateHandoff(t.Context(),
			newHandoffRequest(ticketID, human, fmt.Sprintf("key-later-%d", i), body))
		require.NoError(t, err)
		later := outboxRowsFor(t, h.store, res.Document.ID)
		require.Len(t, later, 1)
		laterIDs = append(laterIDs, later[0].ID)
	}

	// Delivery is switched on and the backlog drains with no replay command.
	mock := h.mock(memory.MockConfig{})
	for i := 0; i < 6; i++ {
		processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err)
		if !processed {
			break
		}
	}

	summary, err := h.store.OutboxSummary(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 5, summary[OutboxSucceeded], "the whole backlog delivered")
	assert.EqualValues(t, 0, summary[OutboxPending])
	assert.EqualValues(t, 0, summary[OutboxFailed])

	// created_at order: the first handoff's three writes (ordered by id, their
	// tiebreaker), then the two later ones in the order they were created.
	calls := h.calls()
	require.Len(t, calls, 5)
	firstThree := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	assert.ElementsMatch(t, firstThree, []string{calls[0].ID, calls[1].ID, calls[2].ID},
		"the first handoff's writes share one created_at, so they come first as a group")
	assert.Equal(t, laterIDs[0], calls[3].ID, "delivery follows created_at order (C-OUTBOX-16)")
	assert.Equal(t, laterIDs[1], calls[4].ID)
}

// ---- C-OUTBOX-17: one loop, poll knob honoured ---------------------------

// TestOutboxWorkerLoopAndPollKnob proves the loop claims one row at a time and
// sleeps LEDGERBOARD_WORKER_POLL_MS when there is nothing to do, and that shutdown
// does not wait out a poll interval (C-OUTBOX-17).
func TestOutboxWorkerLoopAndPollKnob(t *testing.T) {
	h := newOutboxHarness(t)
	_, ids := handoffWithWrites(t, h.store, 2)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg := OutboxWorkerConfig{WorkerID: "loop-worker", PollMS: 5, Adapter: h.mock(memory.MockConfig{})}
	require.NoError(t, h.store.StartOutboxWorker(ctx, cfg))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		summary, err := h.store.OutboxSummary(t.Context())
		require.NoError(t, err)
		if summary[OutboxSucceeded] == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	summary, err := h.store.OutboxSummary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, summary[OutboxSucceeded], "the loop drains the backlog")

	for _, id := range ids {
		row, err := h.store.GetOutboxRow(t.Context(), id)
		require.NoError(t, err)
		assert.EqualValues(t, 1, row.Attempts, "one claim per row, no double delivery")
	}

	// Shutdown is a select on the loop's context: it must not wait out a poll.
	cancel()
	done := make(chan error, 1)
	go func() { done <- h.store.RunOutboxWorker(ctx, cfg) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("RunOutboxWorker did not return promptly after cancellation")
	}
}

// ---- F1: a delivery transition commits when its ticket is gone ------------

// TestOutboxDeliveryAfterTicketDeleted is the regression test for the audit-FK
// trap. C-SCHEMA-13 deliberately keeps outbox rows alive after their ticket is
// deleted, while ticket_events has a NOT NULL FK to tickets (0001_init.up.sql:34)
// that cascades. Before the guarded insert every delivery transition for a
// deleted ticket failed on that FK: the ack never committed, so the row cycled
// processing -> uncertain -> processing forever and the adapter was called again
// on every poll. After it the transition commits, the audit is simply absent
// (C-AUDIT-9), and there is exactly one adapter call per write.
func TestOutboxDeliveryAfterTicketDeleted(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 2)

	require.NoError(t, h.store.DeleteTicket(t.Context(), ticketID))
	// The ticket's audits cascade away; its obligations do not (C-SCHEMA-13).
	assert.EqualValues(t, 0, countRows(t, h.store,
		`SELECT COUNT(*) FROM ticket_events WHERE ticket_id = ?`, ticketID))
	assert.EqualValues(t, 2, countRows(t, h.store,
		`SELECT COUNT(*) FROM outbox WHERE ticket_id = ?`, ticketID))

	mock := h.mock(memory.MockConfig{})
	for i := 0; i < 3; i++ {
		processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
		require.NoError(t, err, "a delivery transition for a deleted ticket must commit (C-AUDIT-9)")
		if !processed {
			break
		}
	}

	for _, id := range ids {
		row, err := h.store.GetOutboxRow(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, OutboxSucceeded, row.Status)
		require.NotNil(t, row.ExternalID)
		assert.Equal(t, memory.ExternalIDFor(id), *row.ExternalID)
		require.NotNil(t, row.DeliveredAt)
		assert.EqualValues(t, 1, row.Attempts, "one attempt: the ack committed, so there is no retry storm")
		assert.Nil(t, row.LastError)
		assert.Nil(t, row.LeaseOwner)
	}

	// Exactly one adapter call per write — not one per iteration.
	calls := h.calls()
	require.Len(t, calls, 2, "the adapter must not be called again once the ack committed")
	assert.ElementsMatch(t, ids, []string{calls[0].ID, calls[1].ID})

	// The audit row was skipped, not invented: the ticket is gone and its events
	// cascaded, so nothing anywhere claims the delivery was audited (C-AUDIT-9).
	assert.EqualValues(t, 0, countRows(t, h.store, `SELECT COUNT(*) FROM ticket_events`))

	summary, err := h.store.OutboxSummary(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 2, summary[OutboxSucceeded])
	assert.EqualValues(t, 0, summary[OutboxPending])
	assert.EqualValues(t, 0, summary[OutboxFailed])
	assert.EqualValues(t, 0, summary[OutboxUncertain])

	// And nothing is left to claim.
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(mock))
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Len(t, h.calls(), 2)
}

// TestOutboxRetryAfterTicketDeleted proves the operator's dead-letter retry still
// works once the ticket is gone (C-OUTBOX-13, C-SCHEMA-13): the reset commits, no
// audit row is invented for a ticket that no longer exists, and the recovered row
// delivers.
func TestOutboxRetryAfterTicketDeleted(t *testing.T) {
	h := newOutboxHarness(t)
	ticketID, ids := handoffWithWrites(t, h.store, 1)
	writeID := ids[0]

	// Dead-letter it, then delete the ticket the obligation outlives.
	_, err := h.store.ProcessOutboxOnce(t.Context(),
		h.workerCfg(h.mock(memory.MockConfig{Behaviour: memory.BehaviourPermanent})))
	require.NoError(t, err)
	row, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	require.Equal(t, OutboxFailed, row.Status)
	require.NoError(t, h.store.DeleteTicket(t.Context(), ticketID))

	human := models.Actor{Type: models.ActorHuman, ID: "alice"}
	retried, err := h.store.RetryOutboxRow(t.Context(), OutboxRetryRequest{
		WriteID: writeID, Actor: human, RequestID: "req-retry-deleted",
	})
	require.NoError(t, err, "C-OUTBOX-13: the retry must not fail on a deleted ticket's audit")
	assert.Equal(t, OutboxPending, retried.Status)
	assert.EqualValues(t, 0, retried.Attempts)
	assert.Nil(t, retried.NextAttemptAt)
	assert.Nil(t, retried.LastError)
	assert.EqualValues(t, 0, countRows(t, h.store, `SELECT COUNT(*) FROM ticket_events`))

	// The recovered row delivers normally.
	processed, err := h.store.ProcessOutboxOnce(t.Context(), h.workerCfg(h.mock(memory.MockConfig{})))
	require.NoError(t, err)
	require.True(t, processed)
	got, err := h.store.GetOutboxRow(t.Context(), writeID)
	require.NoError(t, err)
	assert.Equal(t, OutboxSucceeded, got.Status)
	assert.Equal(t, memory.ExternalIDFor(writeID), *got.ExternalID)
}

// ---- F2: a row-level error must not kill the loop --------------------------

// TestOutboxWorkerSurvivesErrors is F2's regression test: every iteration fails
// here (the write handle is closed), and the loop must still be running until its
// context is cancelled. Before the fix it returned on the first error and
// StartOutboxWorker discarded that return, so delivery stopped for the whole
// process with nothing in the log and nothing in the outbox to say so.
func TestOutboxWorkerSurvivesErrors(t *testing.T) {
	h := newOutboxHarness(t)
	handoffWithWrites(t, h.store, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// From here on every ProcessOutboxOnce fails: the pool is closed.
	require.NoError(t, h.store.db.Close())

	done := make(chan error, 1)
	cfg := OutboxWorkerConfig{WorkerID: "survivor", PollMS: 5, Adapter: h.mock(memory.MockConfig{})}
	go func() { done <- h.store.RunOutboxWorker(ctx, cfg) }()

	select {
	case err := <-done:
		t.Fatalf("the loop returned after an iteration error instead of logging and continuing: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled context is a clean exit, not an error")
	case <-time.After(2 * time.Second):
		t.Fatal("RunOutboxWorker did not return promptly after cancellation")
	}
}
