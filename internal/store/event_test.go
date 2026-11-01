package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rgracey/kanban-mcp/internal/models"
)

// TestListTicketEventsRowidTiebreak is C-AUDIT-7's read ordering. created_at is
// RFC3339 *seconds*, and the engine writes several rows per second routinely
// (claim, renew and release inside one second is normal), so the tie order has
// to be named: created_at first, then rowid — the order the rows were written.
// Without the rowid term SQLite is free to return ties in any order, and every
// caller that reads "oldest first" is passing on incidental behaviour.
func TestListTicketEventsRowidTiebreak(t *testing.T) {
	s, _, _ := setupClaimStore(t)
	ctx := t.Context()
	_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)

	// Start from an empty timeline: CreateTicket's `created` row is stamped by
	// the legacy writer with the real clock, which would otherwise interleave
	// with the fixed instants below (test-plan §1.3, amendment A4).
	_, err := s.db.Exec(`DELETE FROM ticket_events WHERE ticket_id = ?`, ticketID)
	require.NoError(t, err)

	const sameSecond = "2026-11-01T12:00:00Z"
	insert := func(id, createdAt string) {
		t.Helper()
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO ticket_events (id, ticket_id, type, actor, payload, created_at)
			 VALUES (?, ?, 'note_added', 'agent:writer', '{}', ?)`,
			id, ticketID, createdAt)
		require.NoError(t, err)
	}

	// Three rows inside one second, written in this order.
	for _, id := range []string{"evt-a", "evt-b", "evt-c"} {
		insert(id, sameSecond)
	}
	// A row written last whose created_at is a second earlier: created_at still
	// dominates rowid, so it sorts first.
	insert("evt-early", "2026-11-01T11:59:59Z")

	events, err := s.ListTicketEvents(ctx, ticketID)
	require.NoError(t, err)

	var got []string
	for _, ev := range events {
		got = append(got, ev.ID)
	}
	assert.Equal(t, []string{"evt-early", "evt-a", "evt-b", "evt-c"}, got,
		"created_at orders the timeline; rowid breaks same-second ties in write order")

	// The assertion above only means anything if the three rows really do share
	// one second, i.e. if the tiebreak was exercised at all.
	require.Len(t, events, 4)
	assert.True(t, events[1].CreatedAt.Equal(events[2].CreatedAt) &&
		events[2].CreatedAt.Equal(events[3].CreatedAt),
		"the last three events must share one created_at second for the tiebreak to be tested")
}
