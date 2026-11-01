package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/db"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimScope is the (actor, operation, key, hash) scope of a claim request.
func claimScope(a models.Actor, key, hash string) IdempotencyScope {
	return IdempotencyScope{Actor: a, Operation: OperationTicketClaim, Key: key, RequestHash: hash}
}

// TestIdempotencyReplayIsByteIdentical is T-IDEM-02 at the store level: the
// replay returns the recorded bytes, changes nothing, and writes no second audit
// row (C-IDEM-6, C-IDEM-9, C-SCHEMA-18).
func TestIdempotencyReplayIsByteIdentical(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	scope := claimScope(a, "claim-key-1", strings.Repeat("a", 64))

	first, replayed, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a, RequestID: "req-1"}, scope)
	require.NoError(t, err)
	require.False(t, replayed)

	// The ticket changes underneath the key; the replay must still return the
	// first response (C-IDEM-6: the entity is not re-read).
	_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{TicketID: ticketID, Actor: a, Generation: first.Claim.Generation})
	require.NoError(t, err)

	second, replayed, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a, RequestID: "req-1"}, scope)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.Version, second.Version, "the replay returns what the first call returned")
	assert.Equal(t, first.Claim.Generation, second.Claim.Generation)
	assert.Equal(t, first.Claim.Holder, second.Claim.Holder)

	var storedBody string
	var storedStatus int
	require.NoError(t, s.db.QueryRow(`SELECT response_body, response_status FROM idempotency_keys
        WHERE actor = ? AND operation = ? AND key = ?`, a.String(), string(OperationTicketClaim), scope.Key).
		Scan(&storedBody, &storedStatus))
	assert.Equal(t, 200, storedStatus)
	assert.Equal(t, string(mustJSON(t, first)), storedBody, "response_body is stored verbatim (C-SCHEMA-18)")

	assert.Len(t, eventsOfType(t, s, ticketID, models.EventClaimed), 1,
		"C-IDEM-9: a replay writes no second audit row")
	// The key itself never reaches an audit payload (C-IDEM-17).
	for _, ev := range eventsOfType(t, s, ticketID, models.EventClaimed) {
		assert.NotContains(t, ev.Payload, "idempotency_key")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// TestIdempotencyConflictOnDifferentHash is C-IDEM-7.
func TestIdempotencyConflictOnDifferentHash(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	scope := claimScope(a, "claim-key-2", strings.Repeat("b", 64))
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a, RequestID: "req-1"}, scope)
	require.NoError(t, err)

	other := claimScope(a, "claim-key-2", strings.Repeat("c", 64))
	_, _, err = s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a, RequestID: "req-1"}, other)
	e := requireCode(t, err, apperr.CodeIdempotencyConflict)
	assert.Equal(t, strings.Repeat("b", 64), e.Details["request_hash"], "details carry the stored hash")
	assert.Equal(t, string(OperationTicketClaim), e.Details["operation"])
	assert.Equal(t, "claim-key-2", e.Details["key"])
}

// TestIdempotencyOnly2xxRecorded is C-IDEM-8: a failed request stores nothing, so
// the same key re-executes.
func TestIdempotencyOnly2xxRecorded(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	holder, loser := agent("builder-1"), agent("builder-2")
	claimTicket(t, s, ticketID, holder)

	scope := claimScope(loser, "retry-key", strings.Repeat("d", 64))
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: loser, RequestID: "req-1"}, scope)
	requireCode(t, err, apperr.CodeAlreadyClaimed)
	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`),
		"a 409 is never recorded (C-IDEM-8)")

	// The same key re-executes once it can succeed.
	_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{TicketID: ticketID, Actor: holder, Generation: 1})
	require.NoError(t, err)
	_, replayed, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: loser, RequestID: "req-1"}, scope)
	require.NoError(t, err)
	assert.False(t, replayed, "the retry executed, it did not replay")
	assert.EqualValues(t, 1, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`))
}

// TestIdempotencyRequiredVersusOptionalKey is C-IDEM-4.
func TestIdempotencyRequiredVersusOptionalKey(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")

	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, "", strings.Repeat("e", 64)))
	e := requireCode(t, err, apperr.CodeInvalidRequest)
	assert.Equal(t, "idempotency_key", e.Details["field"])

	_, err = s.CreateHandoff(t.Context(), newHandoffRequest(ticketID, a, "", agentHandoff(t, 1, 1, nil)))
	e = requireCode(t, err, apperr.CodeInvalidRequest)
	assert.Equal(t, "idempotency_key", e.Details["field"], "handoff.create requires a key too")

	// Optional: ticket.create with no key executes and records nothing.
	executed := false
	res, err := s.RunIdempotent(t.Context(),
		IdempotencyScope{Actor: a, Operation: OperationTicketCreate},
		func(ctx context.Context, tx *sql.Tx) (StoredResponse, error) {
			executed = true
			return StoredResponse{Status: 201, Body: []byte(`{"id":"x"}`), EntityID: "x"}, nil
		})
	require.NoError(t, err)
	assert.True(t, executed)
	assert.False(t, res.Replayed)
	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`),
		"an optional operation without a key leaves no record")
}

// TestIdempotencyKeyValidation is C-IDEM-2.
func TestIdempotencyKeyValidation(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")

	valid := strings.Repeat("k", 128)
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, valid, strings.Repeat("f", 64)))
	require.NoError(t, err, "128 characters is the maximum and is legal")

	for _, bad := range []string{strings.Repeat("k", 129), "has space", "slash/key", "question?"} {
		_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
			claimScope(a, bad, strings.Repeat("f", 64)))
		e := requireCode(t, err, apperr.CodeInvalidRequest)
		assert.Equal(t, "idempotency_key", e.Details["field"], "key %q", bad)
	}
}

// TestIdempotencyScopeIsolation is C-IDEM-1: the scope is the triple.
func TestIdempotencyScopeIsolation(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketA := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	_, ticketB := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a, b := agent("builder-1"), agent("builder-2")
	hash := strings.Repeat("1", 64)

	firstClaim, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketA, Actor: a}, claimScope(a, "shared-key", hash))
	require.NoError(t, err)

	// Same key, different actor: an independent request.
	_, replayed, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketB, Actor: b}, claimScope(b, "shared-key", hash))
	require.NoError(t, err)
	assert.False(t, replayed, "two actors may use the same key string (C-IDEM-1)")

	// Same actor and key, different operation: also independent.
	_ = firstClaim
	_, err = s.CreateHandoff(t.Context(), newHandoffRequest(ticketA, a, "shared-key",
		agentHandoff(t, firstClaim.Claim.Generation, firstClaim.Version, nil)))
	require.NoError(t, err, "handoff.create under the same key string is a different scope")
	assert.EqualValues(t, 3, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`))
}

// TestIdempotencyExpiredKeyBehavesAsAbsent is C-IDEM-19: the expiry mechanism is
// the expires_at comparison, not the purge.
func TestIdempotencyExpiredKeyBehavesAsAbsent(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	scope := claimScope(a, "aging-key", strings.Repeat("2", 64))

	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a}, scope)
	require.NoError(t, err)

	clock.Advance(IdempotencyRetentionMS + 1)
	// The ticket's lease lapsed with the same clock advance, so the claim can run
	// again; the key must behave as absent even though no purge has run.
	_, replayed, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a}, scope)
	require.NoError(t, err)
	assert.False(t, replayed, "C-IDEM-19: an expired key row behaves as absent, purged or not")
	assert.EqualValues(t, 1, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`), "delete-and-reinsert, not a duplicate")
}

// TestIdempotencyPurge is C-IDEM-16/18: the purge deletes only expired rows and
// never touches the entity a key created.
func TestIdempotencyPurge(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, "purge-key", strings.Repeat("3", 64)))
	require.NoError(t, err)

	n, err := s.PurgeIdempotencyKeys(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "an unexpired key is never purged")

	clock.Advance(IdempotencyRetentionMS + 1)
	n, err = s.PurgeIdempotencyKeys(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM idempotency_keys`))
	// C-IDEM-18: the entity the key created is untouched.
	row := readRow(t, s, ticketID)
	assert.EqualValues(t, 1, row.generation)
	require.True(t, row.holder.Valid)
}

// TestIdempotencyPurgeRateLimit is C-IDEM-16's in-process 10-minute guard.
func TestIdempotencyPurgeRateLimit(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, "rate-key", strings.Repeat("4", 64)))
	require.NoError(t, err)
	clock.Advance(IdempotencyRetentionMS + 1)

	purgeInTx := func() int64 {
		tx, err := s.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer tx.Rollback()
		n, err := s.MaybePurgeIdempotencyTx(t.Context(), tx, clock.NowMS())
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return n
	}

	assert.EqualValues(t, 1, purgeInTx(), "the first opportunistic purge runs")
	assert.EqualValues(t, 0, purgeInTx(), "a second purge inside the window is rate-limited")

	clock.Advance(IdempotencyPurgeIntervalMS + 1)
	assert.EqualValues(t, 0, purgeInTx(), "past the window it runs again (nothing left to delete)")

	// The startup purge seeds the same in-process limit.
	s2, _, clock2 := setupClaimStore(t)
	_, ticket2 := newBoardAndTicket(t, s2, models.StatusTodo, models.PriorityMedium)
	_, _, err = s2.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticket2, Actor: a},
		claimScope(a, "startup-key", strings.Repeat("5", 64)))
	require.NoError(t, err)
	clock2.Advance(IdempotencyRetentionMS + 1)
	n, err := s2.PurgeIdempotencyKeysOnStartup(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	tx, err := s2.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	n, err = s2.MaybePurgeIdempotencyTx(t.Context(), tx, clock2.NowMS())
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "the startup purge counts against the same window")
}

// TestIdempotencyPurgeNeverInsideDedupRequest is C-IDEM-16's exclusion: the purge
// never runs inside a request that is itself being deduplicated.
func TestIdempotencyPurgeNeverInsideDedupRequest(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	a := agent("builder-1")
	_, _, err := s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, "old-key", strings.Repeat("6", 64)))
	require.NoError(t, err)

	clock.Advance(IdempotencyRetentionMS + 1)
	// A deduplicated request runs; the expired row must still be there afterwards.
	_, _, err = s.ClaimIdempotent(t.Context(), ClaimRequest{TicketID: ticketID, Actor: a},
		claimScope(a, "new-key", strings.Repeat("7", 64)))
	require.NoError(t, err)
	assert.EqualValues(t, 1, countRows(t, s,
		`SELECT COUNT(*) FROM idempotency_keys WHERE key = 'old-key'`),
		"no purge ran inside the deduplicated request")
}

// ---- crash atomicity (C-IDEM-12..14) --------------------------------------

const (
	crashChildEnv  = "LEDGERBOARD_TEST_CRASH_CHILD"
	crashDBEnv     = "LEDGERBOARD_TEST_CRASH_DB"
	crashTicketEnv = "LEDGERBOARD_TEST_CRASH_TICKET"
	crashKeyEnv    = "LEDGERBOARD_TEST_CRASH_KEY"
)

// TestIdempotencyCrashChild is the child half of the crash harness: it arms the
// crashpoint seam from the environment and runs one idempotent claim, which
// SIGKILLs itself at the armed point.
func TestIdempotencyCrashChild(t *testing.T) {
	if os.Getenv(crashChildEnv) == "" {
		t.Skip("child process only")
	}
	crashpoint.Init()
	conn, err := db.Open(os.Getenv(crashDBEnv))
	require.NoError(t, err)
	defer conn.Close()

	s := NewSQLiteStore(conn)
	a := agent("builder-1")
	_, _, err = s.ClaimIdempotent(context.Background(),
		ClaimRequest{TicketID: os.Getenv(crashTicketEnv), Actor: a, RequestID: "req-crash"},
		claimScope(a, os.Getenv(crashKeyEnv), strings.Repeat("9", 64)))
	if err != nil {
		t.Fatalf("child claim failed before reaching the crash point: %v", err)
	}
}

// runCrashChild runs one child, waits for its crash marker on the FIFO and
// returns the child's output.
func runCrashChild(t *testing.T, dbPath, ticketID, key, point string) string {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "crash.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	cmd := exec.Command(os.Args[0], "-test.run=TestIdempotencyCrashChild")
	cmd.Env = append(os.Environ(),
		crashChildEnv+"=1",
		crashpoint.EnvPoints+"="+point,
		crashpoint.EnvFIFO+"="+fifo,
		crashDBEnv+"="+dbPath,
		crashTicketEnv+"="+ticketID,
		crashKeyEnv+"="+key,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())

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
		require.Contains(t, line, "crash "+point, "child output:\n%s", out.String())
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("no crash marker for %s; child output:\n%s", point, out.String())
	}
	return out.String()
}

// newCrashFixture makes a fresh migrated database with one claimable ticket and
// closes the writer, so the child process owns the file.
func newCrashFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crash.db")
	conn, err := db.Open(path)
	require.NoError(t, err)
	s := NewSQLiteStore(conn)
	board, err := s.CreateBoard(t.Context(), "crash board", "")
	require.NoError(t, err)
	ticket, err := s.CreateTicket(t.Context(), board.ID, models.Ticket{
		Title: "crash ticket", Status: models.StatusTodo, Priority: models.PriorityMedium,
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	return path, ticket.ID
}

// inspectAfterCrash opens the crashed database and reports what survived.
func inspectAfterCrash(t *testing.T, path, ticketID string) (keyRows int64, keyStatus string, holder sql.NullString, generation int64, claimedEvents int64) {
	t.Helper()
	conn, err := db.Open(path)
	require.NoError(t, err)
	defer conn.Close()
	s := NewSQLiteStore(conn)

	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys`).Scan(&keyRows))
	if keyRows > 0 {
		require.NoError(t, s.db.QueryRow(`SELECT status FROM idempotency_keys`).Scan(&keyStatus))
	}
	require.NoError(t, s.db.QueryRow(`SELECT lease_holder, lease_generation FROM tickets WHERE id = ?`, ticketID).
		Scan(&holder, &generation))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM ticket_events WHERE ticket_id = ? AND type = 'claimed'`, ticketID).
		Scan(&claimedEvents))
	return
}

// TestIdempotencyCrashAtomicity is T-IDEM-05: the key row and the mutation commit
// together, so a kill before COMMIT leaves neither and a kill after leaves both —
// never a third state. Five kills per leg; the iteration count is the determinism
// argument.
func TestIdempotencyCrashAtomicity(t *testing.T) {
	const iterations = 5

	t.Run("pre_commit leaves neither", func(t *testing.T) {
		for i := 0; i < iterations; i++ {
			path, ticketID := newCrashFixture(t)
			runCrashChild(t, path, ticketID, "pre-key", crashpoint.ClaimPreCommit)

			keyRows, _, holder, generation, claimed := inspectAfterCrash(t, path, ticketID)
			assert.EqualValues(t, 0, keyRows, "iteration %d: no key row (C-IDEM-13a)", i)
			assert.False(t, holder.Valid, "iteration %d: no mutation", i)
			assert.EqualValues(t, 0, generation)
			assert.EqualValues(t, 0, claimed, "iteration %d: no audit row", i)
		}
	})

	t.Run("post_commit leaves both", func(t *testing.T) {
		for i := 0; i < iterations; i++ {
			path, ticketID := newCrashFixture(t)
			runCrashChild(t, path, ticketID, "post-key", crashpoint.ClaimPostCommit)

			keyRows, status, holder, generation, claimed := inspectAfterCrash(t, path, ticketID)
			assert.EqualValues(t, 1, keyRows, "iteration %d: the key row survived", i)
			assert.Equal(t, "completed", status, "C-IDEM-14: in_flight is never observable after commit")
			require.True(t, holder.Valid, "iteration %d: the mutation survived", i)
			assert.Equal(t, "builder-1", holder.String)
			assert.EqualValues(t, 1, generation)
			assert.EqualValues(t, 1, claimed)
		}
	})
}
