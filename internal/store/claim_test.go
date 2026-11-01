package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/clocksource"
	"github.com/rgracey/kanban-mcp/internal/db"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock is the in-process fake clock of test-plan §1.3 (H1): the store takes
// a Clock, so expiry and fencing are asserted exactly instead of waited for.
type testClock struct {
	mu sync.Mutex
	ms int64
}

func newTestClock(ms int64) *testClock { return &testClock{ms: ms} }

func (c *testClock) NowMS() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

func (c *testClock) Advance(deltaMS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += deltaMS
}

// startMS is an arbitrary fixed instant: every assertion is relative to it, so
// the tests do not depend on the wall clock.
const startMS int64 = 1761865702180

// setupClaimStore returns a store on a fresh migrated database, the database
// path (for extra connections) and its clock.
func setupClaimStore(t *testing.T, opts ...Option) (*SQLiteStore, string, *testClock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claim.db")
	conn, err := db.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	clock := newTestClock(startMS)
	all := append([]Option{WithClock(clock)}, opts...)
	return NewSQLiteStore(conn, all...), path, clock
}

// newHandle opens an additional independent connection to the same file. Each
// handle is its own pool with its own single connection, which is the
// in-process equivalent of a separate process (C-INV-1 cannot be proved by
// goroutines sharing one handle).
func newHandle(t *testing.T, path string, clock clocksource.Clock, opts ...Option) *SQLiteStore {
	t.Helper()
	conn, err := sql.Open("sqlite", db.DSN(path))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	all := append([]Option{WithClock(clock)}, opts...)
	return NewSQLiteStore(conn, all...)
}

// newBoardAndTicket creates one board and one ticket with the given status and
// priority, returning both ids.
func newBoardAndTicket(t *testing.T, s *SQLiteStore, status models.Status, priority models.Priority) (string, string) {
	t.Helper()
	ctx := t.Context()
	board, err := s.CreateBoard(ctx, "Claim board", "")
	require.NoError(t, err)
	ticket, err := s.CreateTicket(ctx, board.ID, models.Ticket{
		Title:    "A claimable ticket",
		Status:   status,
		Priority: priority,
	})
	require.NoError(t, err)
	return board.ID, ticket.ID
}

func agent(name string) models.Actor { return models.Actor{Type: models.ActorAgent, ID: name} }

// requireCode asserts err is a service error with the given code.
func requireCode(t *testing.T, err error, code apperr.Code) *apperr.Error {
	t.Helper()
	require.Error(t, err)
	e, ok := apperr.From(err)
	require.True(t, ok, "expected a service error, got %T: %v", err, err)
	assert.Equal(t, code, e.Code, "unexpected error code (message: %s)", e.Message)
	return e
}

// ticketRow reads the raw lease columns for direct assertions.
type ticketRow struct {
	status     string
	version    int64
	generation int64
	renewals   int64
	holder     sql.NullString
	holderType sql.NullString
	acquiredAt sql.NullInt64
	expiresAt  sql.NullInt64
	updatedAt  string
}

func readRow(t *testing.T, s *SQLiteStore, ticketID string) ticketRow {
	t.Helper()
	var r ticketRow
	require.NoError(t, s.db.QueryRow(`
		SELECT status, version, lease_generation, lease_renewals,
		       lease_holder, lease_holder_type, lease_acquired_at, lease_expires_at, updated_at
		  FROM tickets WHERE id = ?`, ticketID).
		Scan(&r.status, &r.version, &r.generation, &r.renewals,
			&r.holder, &r.holderType, &r.acquiredAt, &r.expiresAt, &r.updatedAt))
	return r
}

// claimEvents returns the ownership audit rows for a ticket, oldest first.
// The `created` row written by CreateTicket is filtered out: these tests assert
// exactly what the ownership engine wrote (C-AUDIT-4), not what the fixture did.
func claimEvents(t *testing.T, s *SQLiteStore, ticketID string) []models.TicketEvent {
	t.Helper()
	events, err := s.ListTicketEvents(t.Context(), ticketID)
	require.NoError(t, err)
	var out []models.TicketEvent
	for _, ev := range events {
		switch ev.Type {
		case models.EventClaimed, models.EventClaimRenewed, models.EventClaimReleased,
			models.EventClaimReassigned, models.EventClaimRevoked:
			out = append(out, ev)
		}
	}
	return out
}

// TestClaimEightConcurrentClaimantsOneWinner is T-CLAIM-01: eight claimants on
// eight separate connections race one ticket; exactly one wins and the seven
// losers get the full C-ERR-4 detail set describing the winner.
func TestClaimEightConcurrentClaimantsOneWinner(t *testing.T) {
	s, path, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	const claimants = 8
	stores := make([]*SQLiteStore, claimants)
	for i := range stores {
		stores[i] = newHandle(t, path, clock)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]ClaimedTicket, claimants)
	errs := make([]error, claimants)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = stores[i].Claim(t.Context(), ClaimRequest{
				TicketID:  ticketID,
				Actor:     agent("builder-" + string(rune('a'+i))),
				RequestID: "req-race",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	winnerIndex := -1
	for i, err := range errs {
		if err == nil {
			winners++
			winnerIndex = i
			continue
		}
		e := requireCode(t, err, apperr.CodeAlreadyClaimed)
		// C-ERR-4: every loser's details describe the *winner*, not itself.
		for _, key := range []string{"ticket_id", "status", "version", "holder", "holder_type",
			"lease_expires_at", "lease_generation", "reason"} {
			assert.Contains(t, e.Details, key, "already_claimed details must carry %q", key)
		}
	}
	require.Equal(t, 1, winners, "exactly one claimant may win (C-INV-1, C-CLAIM-5)")

	winner := results[winnerIndex]
	require.NotNil(t, winner.Claim.Holder)
	assert.Equal(t, "builder-"+string(rune('a'+winnerIndex)), *winner.Claim.Holder)
	assert.Equal(t, models.ClaimStateActive, winner.Claim.State)
	assert.Equal(t, int64(1), winner.Claim.Generation, "the first claim issues generation 1")
	assert.Equal(t, int64(2), winner.Version, "a claim bumps version by exactly one")
	require.NotNil(t, winner.Claim.ExpiresAtMS)
	assert.Equal(t, startMS+DefaultLeaseTTLMS, *winner.Claim.ExpiresAtMS,
		"lease_expires_at is now_ms + LEDGERBOARD_LEASE_TTL_MS")
	require.NotNil(t, winner.Claim.AcquiredAtMS)
	assert.Equal(t, startMS, *winner.Claim.AcquiredAtMS)

	// The losers must describe the winner.
	for i, err := range errs {
		if err == nil {
			continue
		}
		e, _ := apperr.From(err)
		assert.Equal(t, *winner.Claim.Holder, e.Details["holder"], "claimant %d sees the winner", i)
		assert.Equal(t, int64(1), e.Details["lease_generation"])
	}

	row := readRow(t, s, ticketID)
	assert.Equal(t, "in_progress", row.status)
	assert.Equal(t, int64(1), row.generation, "exactly one generation was issued")
	assert.Equal(t, int64(2), row.version)
	require.True(t, row.holder.Valid)
	assert.Equal(t, *winner.Claim.Holder, row.holder.String)

	// Exactly one `claimed` audit row exists for the whole race.
	claimed := 0
	for _, ev := range claimEvents(t, s, ticketID) {
		if ev.Type == models.EventClaimed {
			claimed++
		}
	}
	assert.Equal(t, 1, claimed, "a losing claim writes no audit row (C-AUDIT-4)")
}

// TestClaimNextRaceGivesDistinctTickets is T-CLAIM-02: concurrent claim_next
// calls never hand the same ticket to two actors, and a sixth caller finds
// nothing claimable.
func TestClaimNextRaceGivesDistinctTickets(t *testing.T) {
	s, path, clock := setupClaimStore(t)
	ctx := t.Context()

	board, err := s.CreateBoard(ctx, "Fleet board", "")
	require.NoError(t, err)
	priorities := []models.Priority{
		models.PriorityCritical, models.PriorityCritical,
		models.PriorityMedium, models.PriorityMedium, models.PriorityLow,
	}
	for i, p := range priorities {
		_, err := s.CreateTicket(ctx, board.ID, models.Ticket{
			Title:    "Ticket " + string(rune('A'+i)),
			Status:   models.StatusTodo,
			Priority: p,
		})
		require.NoError(t, err)
	}

	const workers = 5
	stores := make([]*SQLiteStore, workers)
	for i := range stores {
		stores[i] = newHandle(t, path, clock)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	got := make([]ClaimedTicket, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = stores[i].ClaimNext(t.Context(), ClaimNextRequest{
				BoardID: board.ID, Actor: agent("worker-" + string(rune('a'+i))), RequestID: "req-next",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]string)
	for i := range got {
		require.NoError(t, errs[i], "claim_next %d", i)
		require.NotNil(t, got[i].Claim.Holder)
		id := got[i].ID
		if other, dup := seen[id]; dup {
			t.Fatalf("ticket %s was claimed twice (%s and %s)", id, other, *got[i].Claim.Holder)
		}
		seen[id] = *got[i].Claim.Holder
	}
	assert.Len(t, seen, workers, "five workers get five distinct tickets")

	// A sixth caller: nothing claimable is a 404 with a reason (C-CLAIM-13).
	_, err = s.ClaimNext(ctx, ClaimNextRequest{BoardID: board.ID, Actor: agent("late")})
	e := requireCode(t, err, apperr.CodeNotFound)
	assert.Equal(t, "no_claimable_ticket", e.Details["reason"])
	assert.Equal(t, board.ID, e.Details["board_id"])
}

// TestClaimNextPriorityOrder asserts the C-CLAIM-12 ordering directly:
// critical first, then created_at, then id.
func TestClaimNextPriorityOrder(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	ctx := t.Context()

	board, err := s.CreateBoard(ctx, "Ordering", "")
	require.NoError(t, err)
	order := []models.Priority{
		models.PriorityLow, models.PriorityMedium,
		models.PriorityCritical, models.PriorityHigh,
	}
	ids := make([]string, len(order))
	for i, p := range order {
		ticket, err := s.CreateTicket(ctx, board.ID, models.Ticket{
			Title:    "Ticket " + string(rune('A'+i)),
			Status:   models.StatusTodo,
			Priority: p,
		})
		require.NoError(t, err)
		ids[i] = ticket.ID
	}

	want := []string{ids[2], ids[3], ids[1], ids[0]} // critical, high, medium, low
	var got []string
	for i := 0; i < len(order); i++ {
		claimed, err := s.ClaimNext(ctx, ClaimNextRequest{
			BoardID: board.ID, Actor: agent("picker" + string(rune('a'+i))),
		})
		require.NoError(t, err)
		got = append(got, claimed.ID)
	}
	assert.Equal(t, want, got, "claim_next picks by the priority CASE, then created_at, then id")
}

// TestClaimSuccessShape is T-CLAIM-03: the response is the whole entity with the
// claim block, and the audit row lands in the same transaction.
func TestClaimSuccessShape(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityHigh)

	got, err := s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: agent("builder-1"), RequestID: "req-1",
	})
	require.NoError(t, err)

	assert.Equal(t, ticketID, got.ID)
	assert.Equal(t, models.StatusInProgress, got.Status, "a claim moves the ticket to in_progress (C-CLAIM-6)")
	assert.Equal(t, int64(2), got.Version)
	assert.Equal(t, models.ClaimStateActive, got.Claim.State)
	assert.Equal(t, int64(1), got.Claim.Generation)
	assert.Equal(t, int64(0), got.Claim.Renewals)
	require.NotNil(t, got.Claim.HolderType)
	assert.Equal(t, models.ActorAgent, *got.Claim.HolderType)

	events := claimEvents(t, s, ticketID)
	require.Len(t, events, 1, "exactly one audit row for one claim")
	ev := events[0]
	assert.Equal(t, models.EventClaimed, ev.Type)
	assert.Equal(t, "agent:builder-1", ev.Actor, "the actor is rendered type:id (C-AUDIT-1)")
	assert.Equal(t, "req-1", ev.Payload["request_id"])
	assert.Equal(t, float64(1), ev.Payload["generation"], "the payload carries the post-transition generation")
	assert.Equal(t, "agent", ev.Payload["actor_type"])
	assert.Equal(t, float64(startMS+DefaultLeaseTTLMS), ev.Payload["lease_expires_at"],
		"lease values in the payload are INTEGER ms, not TEXT (C-AUDIT-8)")
	assert.Nil(t, ev.Payload["previous_holder"])
	assert.Equal(t, float64(0), ev.Payload["previous_generation"])
	assert.Equal(t, "claim", ev.Payload["source"])

	// updated_at stays the legacy TEXT RFC3339 seconds form (C-CONV-1).
	row := readRow(t, s, ticketID)
	_, err = time.Parse(time.RFC3339, row.updatedAt)
	require.NoError(t, err, "updated_at must stay RFC3339 text")
	assert.Equal(t, time.UnixMilli(clock.NowMS()).UTC().Format(time.RFC3339), row.updatedAt)
}

// TestClaimDiagnosisOrder is T-CLAIM-04 and C-CLAIM-22: the first matching row
// of the diagnosis table wins, and absence is always diagnosed first.
func TestClaimDiagnosisOrder(t *testing.T) {
	t.Run("done is not claimable", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusDone, models.PriorityMedium)
		_, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		e := requireCode(t, err, apperr.CodeStatusNotClaimable)
		assert.Equal(t, ticketID, e.Details["ticket_id"])
		assert.Equal(t, "done", e.Details["status"])
		assert.Equal(t, []string{"todo"}, e.Details["claimable_statuses"])
	})

	t.Run("blocked is not claimable", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusBlocked, models.PriorityMedium)
		_, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		requireCode(t, err, apperr.CodeStatusNotClaimable)
	})

	t.Run("unfinished blocker", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, blockedID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		_, blockerID := newBoardAndTicket(t, s, models.StatusInProgress, models.PriorityMedium)
		_, err := s.AddRelation(t.Context(), blockerID, blockedID, models.RelationBlocks)
		require.NoError(t, err)

		_, err = s.Claim(t.Context(), ClaimRequest{TicketID: blockedID, Actor: agent("a")})
		e := requireCode(t, err, apperr.CodeDependencyBlocked)
		assert.Equal(t, []string{blockerID}, e.Details["blocking_ticket_ids"])
		assert.Equal(t, []string{"in_progress"}, e.Details["blocking_statuses"])
	})

	t.Run("absent ticket is 404 before any 409", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, err := s.Claim(t.Context(), ClaimRequest{
			TicketID: "00000000000000000000000000000000", Actor: agent("a"),
		})
		requireCode(t, err, apperr.CodeNotFound)
	})

	t.Run("live lease is already_claimed with the full detail set", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		_, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("holder")})
		require.NoError(t, err)

		_, err = s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("other")})
		e := requireCode(t, err, apperr.CodeAlreadyClaimed)
		assert.Equal(t, "holder", e.Details["holder"])
		assert.Equal(t, "agent", e.Details["holder_type"])
		assert.Equal(t, startMS+DefaultLeaseTTLMS, e.Details["lease_expires_at"], "details carry the raw int64 ms value")
		assert.Equal(t, int64(1), e.Details["lease_generation"])
		assert.Equal(t, int64(2), e.Details["version"])
		assert.Equal(t, "lease_active", e.Details["reason"])
	})
}

// TestClaimLapsedLeaseIsClaimable is T-CLAIM-05 and C-CLAIM-7: a lease that
// lapsed without any sweeper running leaves status = in_progress, and the ticket
// is still claimable.
func TestClaimLapsedLeaseIsClaimable(t *testing.T) {
	t.Run("lapsed in_progress lease", func(t *testing.T) {
		s, _, clock := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

		first, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("slow")})
		require.NoError(t, err)

		clock.Advance(DefaultLeaseTTLMS + 1) // one millisecond past expiry

		second, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("fast")})
		require.NoError(t, err, "a lapsed lease is free (C-CLAIM-1)")
		assert.Equal(t, int64(2), second.Claim.Generation, "the new claim issues the next generation")
		assert.Equal(t, "fast", *second.Claim.Holder)
		assert.Equal(t, int64(1), second.Claim.Generation-first.Claim.Generation,
			"a re-claim moves the generation by exactly one (C-INV-2)")
	})

	t.Run("in_progress with no holder", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusInProgress, models.PriorityMedium)
		got, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err, "the third arm of C-CLAIM-7 makes a human-moved in_progress ticket claimable")
		assert.Equal(t, int64(1), got.Claim.Generation)
	})
}

// TestClaimHumanForbidden is C-CLAIM-11: humans do not take leases.
func TestClaimHumanForbidden(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	_, err := s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: models.Actor{Type: models.ActorHuman, ID: "alice"},
	})
	e := requireCode(t, err, apperr.CodeForbidden)
	assert.Equal(t, "human", e.Details["actor_type"])
	assert.Equal(t, "claim", e.Details["operation"])

	_, err = s.ClaimNext(t.Context(), ClaimNextRequest{
		BoardID: "board", Actor: models.Actor{Type: models.ActorHuman, ID: "alice"},
	})
	requireCode(t, err, apperr.CodeForbidden)
}

// TestClaimExpectedVersionGuard covers the optional expected_version arm of the
// §3.2 matrix: supplied and stale is version_conflict, supplied and current is a
// normal claim.
func TestClaimExpectedVersionGuard(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	stale := int64(7)
	_, err := s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: agent("a"), ExpectedVersion: &stale,
	})
	e := requireCode(t, err, apperr.CodeVersionConflict)
	assert.Equal(t, int64(7), e.Details["expected_version"])
	assert.Equal(t, int64(1), e.Details["current_version"])

	current := int64(1)
	got, err := s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: agent("a"), ExpectedVersion: &current,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Version)
}

// TestRenewHeartbeat is T-CLAIM-07 and C-CLAIM-15: renewal extends the lease and
// bumps neither version nor generation.
func TestRenewHeartbeat(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("builder-1")})
	require.NoError(t, err)
	firstExpiry := *claimed.Claim.ExpiresAtMS

	clock.Advance(60_000) // one minute into a fifteen-minute lease
	renewed, err := s.RenewClaim(t.Context(), RenewRequest{
		TicketID: ticketID, Actor: agent("builder-1"),
		Generation: claimed.Claim.Generation, RequestID: "req-renew",
	})
	require.NoError(t, err)

	assert.Equal(t, clock.NowMS()+DefaultLeaseTTLMS, *renewed.Claim.ExpiresAtMS)
	assert.Equal(t, int64(1), renewed.Claim.Renewals)
	assert.Equal(t, claimed.Version, renewed.Version, "renewal does not bump version (C-CLAIM-15)")
	assert.Equal(t, claimed.Claim.Generation, renewed.Claim.Generation, "renewal does not bump generation (C-CLAIM-15)")
	assert.Equal(t, models.ClaimStateActive, renewed.Claim.State)

	events := claimEvents(t, s, ticketID)
	require.Len(t, events, 2)
	ev := events[1]
	assert.Equal(t, models.EventClaimRenewed, ev.Type)
	assert.Equal(t, float64(firstExpiry), ev.Payload["previous_expires_at"])
	assert.Equal(t, float64(clock.NowMS()+DefaultLeaseTTLMS), ev.Payload["lease_expires_at"])
	assert.Equal(t, float64(1), ev.Payload["renewals"])
	assert.Equal(t, float64(claimed.Claim.Generation), ev.Payload["generation"])
}

// TestRenewNeverResurrectsLapsedLease is T-CLAIM-08(i) and C-CLAIM-14.
func TestRenewNeverResurrectsLapsedLease(t *testing.T) {
	s, _, clock := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("zombie")})
	require.NoError(t, err)
	before := readRow(t, s, ticketID)

	clock.Advance(DefaultLeaseTTLMS) // now == lease_expires_at: lapsed

	_, err = s.RenewClaim(t.Context(), RenewRequest{
		TicketID: ticketID, Actor: agent("zombie"), Generation: claimed.Claim.Generation,
	})
	e := requireCode(t, err, apperr.CodeLeaseExpired)
	assert.Equal(t, *claimed.Claim.ExpiresAtMS, e.Details["lease_expires_at"])
	assert.Equal(t, claimed.Claim.Generation, e.Details["current_generation"])

	after := readRow(t, s, ticketID)
	assert.Equal(t, before, after, "a rejected renewal must leave the row untouched")
	assert.Equal(t, 1, len(claimEvents(t, s, ticketID)), "a rejected renewal writes no audit row")
}

// TestRenewDiagnosisOrder is T-CLAIM-09: holder mismatch is diagnosed before
// generation mismatch, absence before both, and the defensive row is a 409 not a
// 500 (C-CLAIM-23, C-CLAIM-25).
func TestRenewDiagnosisOrder(t *testing.T) {
	t.Run("another actor reusing the generation number is not the holder", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err)

		_, err = s.RenewClaim(t.Context(), RenewRequest{
			TicketID: ticketID, Actor: agent("b"), Generation: claimed.Claim.Generation,
		})
		e := requireCode(t, err, apperr.CodeNotClaimHolder)
		assert.Equal(t, "a", e.Details["holder"])
		assert.Equal(t, "agent", e.Details["holder_type"])
	})

	t.Run("holder with a stale generation", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err)

		_, err = s.RenewClaim(t.Context(), RenewRequest{
			TicketID: ticketID, Actor: agent("a"), Generation: claimed.Claim.Generation + 5,
		})
		e := requireCode(t, err, apperr.CodeLeaseGenerationMismatch)
		assert.Equal(t, claimed.Claim.Generation+5, e.Details["expected_generation"])
		assert.Equal(t, claimed.Claim.Generation, e.Details["current_generation"])
	})

	t.Run("absent ticket", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, err := s.RenewClaim(t.Context(), RenewRequest{
			TicketID: "00000000000000000000000000000000", Actor: agent("a"), Generation: 1,
		})
		requireCode(t, err, apperr.CodeNotFound)
	})

	t.Run("defensive row is a 409, never a 500", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err)

		// Unreachable through the contract's own operations (C-INV-4): only a
		// direct UPDATE can make a live lease on a non-in_progress ticket.
		_, err = s.db.Exec(`UPDATE tickets SET status = 'done' WHERE id = ?`, ticketID)
		require.NoError(t, err)

		_, err = s.RenewClaim(t.Context(), RenewRequest{
			TicketID: ticketID, Actor: agent("a"), Generation: claimed.Claim.Generation,
		})
		e := requireCode(t, err, apperr.CodeStatusNotClaimable)
		assert.Equal(t, ticketID, e.Details["ticket_id"])
		assert.Equal(t, "done", e.Details["status"])
	})
}

// TestReleaseClearsLeaseInOneRow is T-CLAIM-10/T-CLAIM-11 and C-INV-3: every
// lease field is cleared in one row update, generation and version each move by
// exactly one, and status returns to todo.
func TestReleaseClearsLeaseInOneRow(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("builder-1")})
	require.NoError(t, err)

	released, err := s.ReleaseClaim(t.Context(), ReleaseRequest{
		TicketID: ticketID, Actor: agent("builder-1"),
		Generation: claimed.Claim.Generation, RequestID: "req-release",
	})
	require.NoError(t, err)

	assert.Equal(t, models.ClaimStateUnclaimed, released.Claim.State)
	assert.Nil(t, released.Claim.Holder)
	assert.Nil(t, released.Claim.HolderType)
	assert.Nil(t, released.Claim.AcquiredAtMS)
	assert.Nil(t, released.Claim.ExpiresAtMS)
	assert.Equal(t, int64(0), released.Claim.Renewals)
	assert.Equal(t, claimed.Claim.Generation+1, released.Claim.Generation, "release bumps the generation (C-CLAIM-17)")
	assert.Equal(t, claimed.Version+1, released.Version)
	assert.Equal(t, models.StatusTodo, released.Status)

	row := readRow(t, s, ticketID)
	assert.False(t, row.holder.Valid, "C-INV-3: holder NULL")
	assert.False(t, row.holderType.Valid, "C-INV-3: holder_type NULL")
	assert.False(t, row.acquiredAt.Valid, "C-INV-3: acquired_at NULL")
	assert.False(t, row.expiresAt.Valid, "C-INV-3: expires_at NULL")
	assert.Equal(t, int64(0), row.renewals, "C-INV-3: renewals 0")

	events := claimEvents(t, s, ticketID)
	require.Len(t, events, 2)
	ev := events[1]
	assert.Equal(t, models.EventClaimReleased, ev.Type)
	assert.Equal(t, "release", ev.Payload["reason"])
	assert.Equal(t, "builder-1", ev.Payload["previous_holder"])
	assert.Equal(t, "in_progress", ev.Payload["status_from"])
	assert.Equal(t, "todo", ev.Payload["status_to"])
	assert.Equal(t, float64(claimed.Claim.Generation+1), ev.Payload["generation"])

	// A stale claim-protected write from the old holder cannot land: the holder
	// is NULL and the generation moved on (C-FENCE-9).
	_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{
		TicketID: ticketID, Actor: agent("builder-1"), Generation: claimed.Claim.Generation,
	})
	requireCode(t, err, apperr.CodeNotClaimHolder)
}

// TestReleaseDiagnosis covers the release arms of the §2.7 table, including the
// deliberate absence of a lease_expired row (C-CLAIM-18).
func TestReleaseDiagnosis(t *testing.T) {
	t.Run("non-holder", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err)

		_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{
			TicketID: ticketID, Actor: agent("b"), Generation: claimed.Claim.Generation,
		})
		e := requireCode(t, err, apperr.CodeNotClaimHolder)
		assert.Equal(t, "a", e.Details["holder"])
	})

	t.Run("stale generation", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
		require.NoError(t, err)

		_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{
			TicketID: ticketID, Actor: agent("a"), Generation: claimed.Claim.Generation + 1,
		})
		requireCode(t, err, apperr.CodeLeaseGenerationMismatch)
	})

	t.Run("absent ticket", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, err := s.ReleaseClaim(t.Context(), ReleaseRequest{
			TicketID: "00000000000000000000000000000000", Actor: agent("a"), Generation: 1,
		})
		requireCode(t, err, apperr.CodeNotFound)
	})

	t.Run("lapsed holder with its current generation succeeds", func(t *testing.T) {
		s, _, clock := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("crashed")})
		require.NoError(t, err)

		clock.Advance(DefaultLeaseTTLMS * 3) // long lapsed

		released, err := s.ReleaseClaim(t.Context(), ReleaseRequest{
			TicketID: ticketID, Actor: agent("crashed"), Generation: claimed.Claim.Generation,
		})
		require.NoError(t, err, "release has no clock in its guard (C-CLAIM-18)")
		assert.Equal(t, models.ClaimStateUnclaimed, released.Claim.State)
		assert.Nil(t, released.Claim.Holder)
		assert.Nil(t, released.Claim.ExpiresAtMS)
		assert.Equal(t, int64(0), released.Claim.Renewals)
	})

	t.Run("lapsed holder with a stale generation is rejected", func(t *testing.T) {
		s, _, clock := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("crashed")})
		require.NoError(t, err)

		clock.Advance(DefaultLeaseTTLMS * 3)

		_, err = s.ReleaseClaim(t.Context(), ReleaseRequest{
			TicketID: ticketID, Actor: agent("crashed"), Generation: claimed.Claim.Generation + 1,
		})
		requireCode(t, err, apperr.CodeLeaseGenerationMismatch)
	})
}

// TestGenerationIsMonotonic is T-CLAIM-17 (the store-level half): every
// ownership transition moves the generation by exactly one, and a renewal leaves
// it alone.
func TestGenerationIsMonotonic(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	var generations []int64
	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
	require.NoError(t, err)
	generations = append(generations, claimed.Claim.Generation)

	renewed, err := s.RenewClaim(t.Context(), RenewRequest{
		TicketID: ticketID, Actor: agent("a"), Generation: claimed.Claim.Generation,
	})
	require.NoError(t, err)
	generations = append(generations, renewed.Claim.Generation) // unchanged

	released, err := s.ReleaseClaim(t.Context(), ReleaseRequest{
		TicketID: ticketID, Actor: agent("a"), Generation: renewed.Claim.Generation,
	})
	require.NoError(t, err)
	generations = append(generations, released.Claim.Generation)

	reclaimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("b")})
	require.NoError(t, err)
	generations = append(generations, reclaimed.Claim.Generation)

	assert.Equal(t, []int64{1, 1, 2, 3}, generations,
		"claim +1, renew unchanged, release +1, claim +1 (C-INV-2)")
}

// TestClaimJSONShape asserts C-CONV-8 on the claim block: INTEGER ms columns are
// rendered as RFC3339 UTC with exactly three fractional digits.
func TestClaimJSONShape(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	got, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
	require.NoError(t, err)

	body, err := json.Marshal(got)
	require.NoError(t, err)

	var decoded struct {
		Version int64 `json:"version"`
		Claim   struct {
			State      string  `json:"state"`
			Holder     *string `json:"holder"`
			HolderType *string `json:"holder_type"`
			AcquiredAt *string `json:"acquired_at"`
			ExpiresAt  *string `json:"expires_at"`
			Generation int64   `json:"generation"`
			Renewals   int64   `json:"renewals"`
		} `json:"claim"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, int64(2), decoded.Version)
	assert.Equal(t, "active", decoded.Claim.State)
	require.NotNil(t, decoded.Claim.Holder)
	assert.Equal(t, "a", *decoded.Claim.Holder)
	require.NotNil(t, decoded.Claim.AcquiredAt)
	assert.Equal(t, "2025-10-30T23:08:22.180Z", *decoded.Claim.AcquiredAt,
		"ms columns render as RFC3339 with three fractional digits (C-CONV-8)")
}

// TestClaimActorValidation: the store rejects an identity it cannot reason
// about, rather than trusting the transport (C-CONV-5).
func TestClaimActorValidation(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	_, err := s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: models.Actor{Type: "robot", ID: "r2"},
	})
	requireCode(t, err, apperr.CodeInvalidRequest)

	_, err = s.Claim(t.Context(), ClaimRequest{
		TicketID: ticketID, Actor: models.Actor{Type: models.ActorAgent, ID: "bad id!"},
	})
	requireCode(t, err, apperr.CodeInvalidRequest)
}

// TestClaimableStatusConfig covers C-CLAIM-3's validator.
func TestClaimableStatusConfig(t *testing.T) {
	statuses, err := ParseClaimableStatuses("todo")
	require.NoError(t, err)
	assert.Equal(t, []models.Status{models.StatusTodo}, statuses)

	statuses, err = ParseClaimableStatuses(" todo , in_progress ")
	require.NoError(t, err)
	assert.Equal(t, []models.Status{models.StatusTodo, models.StatusInProgress}, statuses)

	for _, bad := range []string{"done", "blocked", "todo,done", "weird", ""} {
		_, err := ParseClaimableStatuses(bad)
		assert.Error(t, err, "%q must be rejected", bad)
	}

	// The configured set is what the guard uses.
	s, _, _ := setupClaimStore(t, WithClaimableStatuses([]models.Status{models.StatusInProgress}))
	_, todoID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
	_, err = s.Claim(t.Context(), ClaimRequest{TicketID: todoID, Actor: agent("a")})
	requireCode(t, err, apperr.CodeStatusNotClaimable)

	_, inProgressID := newBoardAndTicket(t, s, models.StatusInProgress, models.PriorityMedium)
	_, err = s.Claim(t.Context(), ClaimRequest{TicketID: inProgressID, Actor: agent("a")})
	require.NoError(t, err)
}

// TestClaimSortsLeaseTTL: the TTL is read at the moment the lease is issued, and
// a changed knob never rewrites an existing lease (C-CONV-10).
func TestClaimSortsLeaseTTL(t *testing.T) {
	s, _, clock := setupClaimStore(t, WithLeaseTTLMS(60_000))
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	claimed, err := s.Claim(t.Context(), ClaimRequest{TicketID: ticketID, Actor: agent("a")})
	require.NoError(t, err)
	assert.Equal(t, int64(60_000), *claimed.Claim.ExpiresAtMS-clock.NowMS())

	// A shorter TTL on another store over the same database does not touch the
	// live lease.
	other := NewSQLiteStore(s.db, WithClock(clock), WithLeaseTTLMS(1_000))
	_, err = other.RenewClaim(t.Context(), RenewRequest{
		TicketID: ticketID, Actor: agent("a"), Generation: claimed.Claim.Generation,
	})
	require.NoError(t, err)
	row := readRow(t, s, ticketID)
	assert.Equal(t, clock.NowMS()+1_000, row.expiresAt.Int64, "the new TTL applies at the next renewal")
}

// sortedIDs is a small helper for stable assertions on id sets.
func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

var _ = context.Background
