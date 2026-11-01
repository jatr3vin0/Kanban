package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/models"
)

// DefaultLeaseTTLMS is the contract default for LEDGERBOARD_LEASE_TTL_MS: the
// spec's fifteen minutes (C-CONV-10). Changing it is a contract revision, not a
// tuning knob.
const DefaultLeaseTTLMS int64 = 900000

// MinLeaseTTLMS is the contract floor (C-CONV-10). At the floor a lease can
// lapse while its holder is waiting out busy_timeout; that is legal and surfaces
// as lease_expired, which is why the floor is a test value and not a
// recommended one.
const MinLeaseTTLMS int64 = 1000

// ClaimedTicket is a ticket plus the two fields C-CONV-7 requires every
// mutating response to carry: the row version and the derived claim block.
// Embedding keeps the wire shape flat — the ticket's own fields sit beside
// `version` and `claim`, with no envelope (C-COMPAT-1).
type ClaimedTicket struct {
	models.Ticket
	Version int64        `json:"version"`
	Claim   models.Claim `json:"claim"`
}

// ClaimRequest is one claim attempt.
//
// Actor is server-derived (C-AUTH-1) and is never read from a request body.
// ExpectedVersion is optional (contracts.md §3.2): when supplied, a mismatch is
// version_conflict instead of a successful claim.
type ClaimRequest struct {
	TicketID        string
	Actor           models.Actor
	RequestID       string
	ExpectedVersion *int64
}

// ClaimNextRequest is one claim_next attempt on a board.
type ClaimNextRequest struct {
	BoardID   string
	Actor     models.Actor
	RequestID string
}

// RenewRequest is one renewal of a held lease. Generation is the fencing token
// the holder was issued (C-FENCE-2, C-CLAIM-16).
type RenewRequest struct {
	TicketID   string
	Actor      models.Actor
	Generation int64
	RequestID  string
}

// ReleaseRequest is one release of a held lease. Release has no expiry guard
// (C-CLAIM-18): a lapsed holder with its current generation still succeeds.
type ReleaseRequest struct {
	TicketID   string
	Actor      models.Actor
	Generation int64
	RequestID  string
}

// Claim takes a free lease on one ticket (transition T1).
//
// One BEGIN IMMEDIATE transaction: guard and mutation in a single conditional
// UPDATE, the `claimed` audit row in the same transaction (C-AUDIT-4), the whole
// entity read back inside it (C-CLAIM-9), and the post-commit crashpoint after
// the commit (C-CONV-3).
func (s *SQLiteStore) Claim(ctx context.Context, req ClaimRequest) (ClaimedTicket, error) {
	if err := s.requireClaimingActor(req.Actor); err != nil {
		return ClaimedTicket{}, err
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	ticket, err := s.claimInTx(ctx, tx, req, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	if err := tx.Commit(); err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	// Armed, this kills the process after COMMIT: the claim and its audit row
	// must both survive the restart.
	crashpoint.Hit(crashpoint.ClaimPostCommit)
	return ticket, nil
}

// ClaimIdempotent is Claim executed under an idempotency scope (§4.1:
// `ticket.claim`, key required — C-IDEM-4).
//
// The claim, its `claimed` audit row and the idempotency key row all commit in
// the same transaction (C-IDEM-12), so a crash leaves either nothing or
// everything (C-IDEM-13). A replay returns the recorded response and does not
// re-run the mutation, re-publish SSE or write a second audit row (C-IDEM-6,
// C-IDEM-9).
func (s *SQLiteStore) ClaimIdempotent(ctx context.Context, req ClaimRequest, scope IdempotencyScope) (ClaimedTicket, bool, error) {
	if err := s.requireClaimingActor(req.Actor); err != nil {
		return ClaimedTicket{}, false, err
	}
	if scope.Actor.String() != req.Actor.String() {
		return ClaimedTicket{}, false, apperr.WithDetails(apperr.CodeInternal,
			"idempotency scope actor disagrees with the request actor", map[string]any{"field": "actor"})
	}
	scope.Operation = OperationTicketClaim
	if scope.Key != "" && scope.RequestHash == "" {
		return ClaimedTicket{}, false, apperr.WithDetails(apperr.CodeInvalidRequest,
			"request_hash is required with an idempotency key (C-IDEM-10)",
			map[string]any{"field": "request_hash"})
	}
	nowMS := s.clock.NowMS()

	var claimed ClaimedTicket
	res, err := s.RunIdempotent(ctx, scope, func(ctx context.Context, tx *sql.Tx) (StoredResponse, error) {
		ticket, err := s.claimInTx(ctx, tx, req, nowMS)
		if err != nil {
			return StoredResponse{}, err
		}
		body, err := json.Marshal(ticket)
		if err != nil {
			return StoredResponse{}, err
		}
		claimed = ticket
		return StoredResponse{Status: 200, Body: body, EntityID: ticket.ID}, nil
	})
	if err != nil {
		return ClaimedTicket{}, false, err
	}

	if res.Replayed {
		var replayed ClaimedTicket
		if err := json.Unmarshal(res.Response.Body, &replayed); err != nil {
			return ClaimedTicket{}, false, apperr.New(apperr.CodeInternal,
				"recorded claim response could not be decoded (C-SCHEMA-18)")
		}
		return replayed, true, nil
	}
	// Armed, this kills the process after COMMIT: the claim, its audit row and
	// its key row must all survive the restart (C-IDEM-13).
	crashpoint.Hit(crashpoint.ClaimPostCommit)
	return claimed, false, nil
}

// claimInTx is the body of Claim without the transaction boundary, so the
// idempotent path can run the same mutation inside a transaction it shares with
// the key row (C-IDEM-12). It never commits: the caller owns the transaction.
func (s *SQLiteStore) claimInTx(ctx context.Context, tx *sql.Tx, req ClaimRequest, nowMS int64) (ClaimedTicket, error) {
	// The row is read once, inside the write transaction, for two purposes only:
	// the audit payload's pre-transition values and, when the UPDATE affects no
	// rows, the diagnosis. It never decides whether to write (C-CONV-4): the
	// single conditional UPDATE below is the decision.
	prior, found, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	query, args := s.claimStatement(req, nowMS)
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ClaimedTicket{}, err
	}
	if affected == 0 {
		return ClaimedTicket{}, s.diagnoseClaim(ctx, tx, req, prior, found, nowMS)
	}
	if affected > 1 {
		// A single-row `id` predicate cannot affect two rows; if it ever does,
		// that is a bug, not a business outcome (C-CLAIM-9).
		return ClaimedTicket{}, apperr.New(apperr.CodeInternal, "claim updated more than one ticket")
	}

	ticket, _, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}
	if err := insertAudit(ctx, tx, req.TicketID, models.EventClaimed, req.Actor, map[string]any{
		"request_id":          req.RequestID,
		"generation":          ticket.Claim.Generation,
		"actor_type":          string(req.Actor.Type),
		"lease_expires_at":    derefMS(ticket.Claim.ExpiresAtMS),
		"previous_holder":     derefString(prior.Claim.Holder),
		"previous_generation": prior.Claim.Generation,
		"source":              "claim",
	}, nowMS); err != nil {
		return ClaimedTicket{}, err
	}
	// Armed, this kills the process before COMMIT: the claim, its audit row and
	// everything else staged here must not survive (C-CONV-3, C-AUDIT-4). It is
	// fired after insertAudit so the crash test's "no audit row survived"
	// assertion can actually fail; the point keeps its name and its meaning
	// (T-IDEM-05, C-IDEM-12).
	crashpoint.Hit(crashpoint.ClaimPreCommit)
	return ticket, nil
}

// ClaimNext claims the highest-priority claimable ticket on a board (C-CLAIM-12)
// with one statement: the id is selected by the same UPDATE that mutates it, so
// there is no read-then-write and no window between picking and claiming.
func (s *SQLiteStore) ClaimNext(ctx context.Context, req ClaimNextRequest) (ClaimedTicket, error) {
	if err := s.requireClaimingActor(req.Actor); err != nil {
		return ClaimedTicket{}, err
	}
	if strings.TrimSpace(req.BoardID) == "" {
		return ClaimedTicket{}, apperr.WithDetails(apperr.CodeInvalidRequest,
			"board_id is required", map[string]any{"field": "board_id"})
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	ticket, err := s.claimNextInTx(ctx, tx, req, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	if err := tx.Commit(); err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	crashpoint.Hit(crashpoint.ClaimPostCommit)
	return ticket, nil
}

// ClaimNextIdempotent is ClaimNext executed under an idempotency scope (§4.1:
// `ticket.claim`, key required — C-IDEM-4). The claimed row, its audit row and
// the key row commit together (C-IDEM-12).
func (s *SQLiteStore) ClaimNextIdempotent(ctx context.Context, req ClaimNextRequest, scope IdempotencyScope) (ClaimedTicket, bool, error) {
	if err := s.requireClaimingActor(req.Actor); err != nil {
		return ClaimedTicket{}, false, err
	}
	scope.Operation = OperationTicketClaim
	nowMS := s.clock.NowMS()

	var claimed ClaimedTicket
	res, err := s.RunIdempotent(ctx, scope, func(ctx context.Context, tx *sql.Tx) (StoredResponse, error) {
		ticket, err := s.claimNextInTx(ctx, tx, req, nowMS)
		if err != nil {
			return StoredResponse{}, err
		}
		body, err := json.Marshal(ticket)
		if err != nil {
			return StoredResponse{}, err
		}
		claimed = ticket
		return StoredResponse{Status: 200, Body: body, EntityID: ticket.ID}, nil
	})
	if err != nil {
		return ClaimedTicket{}, false, err
	}
	if res.Replayed {
		var replayed ClaimedTicket
		if err := json.Unmarshal(res.Response.Body, &replayed); err != nil {
			return ClaimedTicket{}, false, apperr.New(apperr.CodeInternal,
				"recorded claim_next response could not be decoded (C-SCHEMA-18)")
		}
		return replayed, true, nil
	}
	crashpoint.Hit(crashpoint.ClaimPostCommit)
	return claimed, false, nil
}

// claimNextInTx is the body of ClaimNext without the transaction boundary, so
// the idempotent path can run it inside the transaction that also carries the
// key row (C-IDEM-12).
func (s *SQLiteStore) claimNextInTx(ctx context.Context, tx *sql.Tx, req ClaimNextRequest, nowMS int64) (ClaimedTicket, error) {
	// The pre-transition values the §1.8 `claimed` payload must report are read
	// once, inside the write transaction and before the mutation (C-CONV-4 as
	// amended by A1). This read decides nothing — the UPDATE below picks the
	// row and its RETURNING reports what it did — and it cannot disagree with
	// it either: the transaction holds the write lock, so no other writer can
	// change which row is claimable between the two statements.
	prior, found, err := s.readClaimNextCandidate(ctx, tx, req.BoardID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	var id string
	var generation, version int64
	query, args := s.claimNextStatement(req.Actor, req.BoardID, nowMS)
	err = tx.QueryRowContext(ctx, query, args...).Scan(&id, &generation, &version)
	// The statement returns version for callers that want it; the entity read
	// back below is the authoritative copy, so the returned value is not reused.
	_ = version
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing claimable is a 404 with a reason, not an error state to retry
		// (C-CLAIM-13).
		return ClaimedTicket{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("board %s has no claimable ticket", req.BoardID),
			map[string]any{"board_id": req.BoardID, "reason": "no_claimable_ticket"})
	}
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}

	if !found || prior.ID != id {
		// Unreachable while the write lock is held: the same predicate and the
		// same ORDER BY chose both rows. If it ever fires, refuse to write an
		// audit row whose pre-transition values belong to another ticket.
		return ClaimedTicket{}, apperr.New(apperr.CodeInternal,
			"claim_next pre-read disagreed with the claim statement")
	}

	ticket, _, err := readClaimedTicket(ctx, tx, id, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}
	// previous_holder is the holder the row had immediately before this
	// transition: the lapsed holder's id when claim_next takes over a lapsed
	// lease, null only when the row was genuinely unclaimed (§1.8, A2). The
	// guard admits both arms, so a hardcoded null here would contradict the
	// previous_generation written beside it.
	if err := insertAudit(ctx, tx, id, models.EventClaimed, req.Actor, map[string]any{
		"request_id":          req.RequestID,
		"generation":          generation,
		"actor_type":          string(req.Actor.Type),
		"lease_expires_at":    derefMS(ticket.Claim.ExpiresAtMS),
		"previous_holder":     derefString(prior.Claim.Holder),
		"previous_generation": prior.Claim.Generation,
		"source":              "claim_next",
	}, nowMS); err != nil {
		return ClaimedTicket{}, err
	}
	// Armed, this kills the process before COMMIT: fired after insertAudit for
	// the same reason as in claimInTx, so the audit row is staged and the
	// rollback must take it (T-IDEM-05, C-AUDIT-4).
	crashpoint.Hit(crashpoint.ClaimPreCommit)
	return ticket, nil
}

// RenewClaim extends a live lease (transition T2). It never resurrects a lapsed
// lease (C-CLAIM-14) and it never bumps version or generation (C-CLAIM-15): it
// is a heartbeat, not a state change.
func (s *SQLiteStore) RenewClaim(ctx context.Context, req RenewRequest) (ClaimedTicket, error) {
	if err := s.requireClaimingActor(req.Actor); err != nil {
		return ClaimedTicket{}, err
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	// C-IDEM-16(b): this write transaction is not itself being deduplicated, so it
	// may carry the opportunistic purge. The 10-minute rate limit is in-process.
	if _, err := s.MaybePurgeIdempotencyTx(ctx, tx, nowMS); err != nil {
		return ClaimedTicket{}, err
	}

	prior, found, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	res, err := tx.ExecContext(ctx, `
        UPDATE tickets
           SET lease_expires_at = ?,
               lease_renewals = lease_renewals + 1,
               updated_at = ?
         WHERE id = ?
           AND lease_holder = ?
           AND lease_generation = ?
           AND lease_expires_at > ?
           AND status = 'in_progress'`,
		nowMS+s.ttlMS, timeText(nowMS), req.TicketID, req.Actor.ID, req.Generation, nowMS)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ClaimedTicket{}, err
	}
	if affected == 0 {
		return ClaimedTicket{}, diagnoseRenew(req, prior, found, nowMS)
	}
	if affected > 1 {
		return ClaimedTicket{}, apperr.New(apperr.CodeInternal, "renew updated more than one ticket")
	}

	crashpoint.Hit(crashpoint.RenewPreCommit)

	ticket, _, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}
	if err := insertAudit(ctx, tx, req.TicketID, models.EventClaimRenewed, req.Actor, map[string]any{
		"request_id":          req.RequestID,
		"generation":          ticket.Claim.Generation,
		"previous_expires_at": derefMS(prior.Claim.ExpiresAtMS),
		"lease_expires_at":    derefMS(ticket.Claim.ExpiresAtMS),
		"renewals":            ticket.Claim.Renewals,
	}, nowMS); err != nil {
		return ClaimedTicket{}, err
	}

	if err := tx.Commit(); err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	crashpoint.Hit(crashpoint.RenewPostCommit)
	return ticket, nil
}

// ReleaseClaim surrenders a held lease (transition T3). The guard is holder plus
// generation and contains no clock (C-CLAIM-18): a lapsed holder may still
// release the row it left behind, while a stale generation is rejected because
// the release bumped the generation and someone else may own the ticket now.
func (s *SQLiteStore) ReleaseClaim(ctx context.Context, req ReleaseRequest) (ClaimedTicket, error) {
	if err := req.Actor.Validate(); err != nil {
		return ClaimedTicket{}, invalidActor(err)
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	// C-IDEM-16(b): this write transaction is not itself being deduplicated, so it
	// may carry the opportunistic purge. The 10-minute rate limit is in-process.
	if _, err := s.MaybePurgeIdempotencyTx(ctx, tx, nowMS); err != nil {
		return ClaimedTicket{}, err
	}

	prior, found, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}

	// Every lease field is cleared in one row update (C-INV-3); status returns to
	// todo only from in_progress (C-CLAIM-6).
	res, err := tx.ExecContext(ctx, `
        UPDATE tickets
           SET status = CASE WHEN status = 'in_progress' THEN 'todo' ELSE status END,
               lease_holder = NULL,
               lease_holder_type = NULL,
               lease_acquired_at = NULL,
               lease_expires_at = NULL,
               lease_renewals = 0,
               lease_generation = lease_generation + 1,
               version = version + 1,
               updated_at = ?
         WHERE id = ?
           AND lease_holder = ?
           AND lease_generation = ?`,
		timeText(nowMS), req.TicketID, req.Actor.ID, req.Generation)
	if err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ClaimedTicket{}, err
	}
	if affected == 0 {
		return ClaimedTicket{}, diagnoseRelease(req, prior, found)
	}
	if affected > 1 {
		return ClaimedTicket{}, apperr.New(apperr.CodeInternal, "release updated more than one ticket")
	}

	ticket, _, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return ClaimedTicket{}, err
	}
	if err := insertAudit(ctx, tx, req.TicketID, models.EventClaimReleased, req.Actor, map[string]any{
		"request_id":      req.RequestID,
		"generation":      ticket.Claim.Generation,
		"previous_holder": derefString(prior.Claim.Holder),
		"actor_type":      string(req.Actor.Type),
		"status_from":     string(prior.Status),
		"status_to":       string(ticket.Status),
		"reason":          "release",
	}, nowMS); err != nil {
		return ClaimedTicket{}, err
	}

	if err := tx.Commit(); err != nil {
		return ClaimedTicket{}, mapDriverErr(err)
	}
	return ticket, nil
}

// ---- statements ------------------------------------------------------------

// claimStatusArms builds the two status placeholders of the claim guard. The
// status list is bound, never interpolated (C-CLAIM-3's configurable list).
func (s *SQLiteStore) claimStatusArms() (string, []any) {
	placeholders := make([]string, len(s.claimableStatuses))
	args := make([]any, len(s.claimableStatuses))
	for i, st := range s.claimableStatuses {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	return strings.Join(placeholders, ", "), args
}

// claimStatement is the contracted claim statement (C-CLAIM-7's three-way
// status predicate, C-CLAIM-8's blocker guard) plus, when the caller supplied
// one, the optional expected_version arm of the §3.2 matrix.
func (s *SQLiteStore) claimStatement(req ClaimRequest, nowMS int64) (string, []any) {
	statusList, statusArgs := s.claimStatusArms()
	query := `
        UPDATE tickets
           SET status = 'in_progress',
               lease_holder = ?,
               lease_holder_type = ?,
               lease_acquired_at = ?,
               lease_expires_at = ?,
               lease_generation = lease_generation + 1,
               lease_renewals = 0,
               version = version + 1,
               updated_at = ?
         WHERE id = ?
           AND (lease_holder IS NULL OR lease_expires_at <= ?)
           AND ( status IN (` + statusList + `)
                 OR (status = 'in_progress' AND (lease_holder IS NULL OR lease_expires_at <= ?)) )
           AND NOT EXISTS (
               SELECT 1 FROM ticket_relations r
                 JOIN tickets b ON b.id = r.from_ticket_id
                WHERE r.to_ticket_id = tickets.id
                  AND r.kind = 'blocks'
                  AND b.status <> 'done')`
	args := []any{
		req.Actor.ID, string(req.Actor.Type), nowMS, nowMS + s.ttlMS, timeText(nowMS),
		req.TicketID, nowMS,
	}
	args = append(args, statusArgs...)
	args = append(args, nowMS)
	if req.ExpectedVersion != nil {
		query += ` AND version = ?`
		args = append(args, *req.ExpectedVersion)
	}
	return query, args
}

// claimNextSelection is the row-selection half of C-CLAIM-12's statement: the
// board predicate, the blocker guard and the priority CASE copied from
// ListTickets/ReadyTickets (priority is TEXT, so a numeric sort is wrong).
// One predicate, two readers: the UPDATE that claims the row and the read of
// its pre-transition values, so the two cannot disagree about which row it is.
func (s *SQLiteStore) claimNextSelection(boardID string, nowMS int64) (string, []any) {
	statusList, statusArgs := s.claimStatusArms()
	query := `
           SELECT id FROM tickets
            WHERE board_id = ?
              AND (lease_holder IS NULL OR lease_expires_at <= ?)
              AND ( status IN (` + statusList + `)
                    OR (status = 'in_progress' AND (lease_holder IS NULL OR lease_expires_at <= ?)) )
              AND NOT EXISTS (
                  SELECT 1 FROM ticket_relations r
                    JOIN tickets b ON b.id = r.from_ticket_id
                   WHERE r.to_ticket_id = tickets.id AND r.kind = 'blocks' AND b.status <> 'done')
            ORDER BY CASE priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1
                                   WHEN 'medium'   THEN 2 ELSE 3 END,
                     created_at ASC, id ASC
            LIMIT 1
`
	args := []any{boardID, nowMS}
	args = append(args, statusArgs...)
	args = append(args, nowMS)
	return query, args
}

// claimNextStatement is C-CLAIM-12's single statement, verbatim: the id is
// chosen and mutated by the same UPDATE.
func (s *SQLiteStore) claimNextStatement(actor models.Actor, boardID string, nowMS int64) (string, []any) {
	selection, selectionArgs := s.claimNextSelection(boardID, nowMS)
	query := `
        UPDATE tickets
           SET status = 'in_progress', lease_holder = ?, lease_holder_type = ?,
               lease_acquired_at = ?, lease_expires_at = ?,
               lease_generation = lease_generation + 1, lease_renewals = 0,
               version = version + 1, updated_at = ?
         WHERE id = (` + selection + `)
        RETURNING id, lease_generation, version`
	args := []any{actor.ID, string(actor.Type), nowMS, nowMS + s.ttlMS, timeText(nowMS)}
	args = append(args, selectionArgs...)
	return query, args
}

// readClaimNextCandidate reads the pre-transition values of the row claim_next
// is about to claim, with the same predicate and order as the claim statement
// itself (C-CONV-4 as amended by A1: one read of the target row, inside the
// write transaction, for the audit payload only).
func (s *SQLiteStore) readClaimNextCandidate(ctx context.Context, tx *sql.Tx, boardID string, nowMS int64) (ClaimedTicket, bool, error) {
	selection, args := s.claimNextSelection(boardID, nowMS)
	return readClaimedTicketWhere(ctx, tx, "WHERE id = ("+selection+")", args, nowMS)
}

// ---- diagnosis (C-CLAIM-22: first matching row wins) ----------------------

// diagnoseClaim explains a 0-row claim from the row state read under the same
// write lock. Order is normative: absent, then status, then blocker, then a live
// lease, then the optional version arm, then internal.
func (s *SQLiteStore) diagnoseClaim(ctx context.Context, tx *sql.Tx, req ClaimRequest, prior ClaimedTicket, found bool, nowMS int64) error {
	if !found {
		return apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("ticket %s not found", req.TicketID),
			map[string]any{"ticket_id": req.TicketID})
	}
	status := string(prior.Status)
	leaseFree := prior.Claim.Holder == nil ||
		(prior.Claim.ExpiresAtMS != nil && *prior.Claim.ExpiresAtMS <= nowMS)

	// A `done`, `blocked` or unknown status is never claimable. `in_progress` is
	// deliberately not diagnosed here: the guard admits it when its lease is
	// free, so a failure on an in_progress row is a lease or blocker failure.
	if !s.isClaimableStatus(prior.Status) && prior.Status != models.StatusInProgress {
		return apperr.WithDetails(apperr.CodeStatusNotClaimable,
			fmt.Sprintf("ticket %s has status %q, which is not claimable", req.TicketID, status),
			map[string]any{
				"ticket_id":          req.TicketID,
				"status":             status,
				"claimable_statuses": s.claimableStatusStrings(),
			})
	}

	blockers, err := blockingTickets(ctx, tx, req.TicketID)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		ids := make([]string, 0, len(blockers))
		statuses := make([]string, 0, len(blockers))
		for _, b := range blockers {
			ids = append(ids, b.id)
			statuses = append(statuses, b.status)
		}
		return apperr.WithDetails(apperr.CodeDependencyBlocked,
			fmt.Sprintf("ticket %s is blocked by %d unfinished ticket(s)", req.TicketID, len(blockers)),
			map[string]any{
				"ticket_id":           req.TicketID,
				"blocking_ticket_ids": ids,
				"blocking_statuses":   statuses,
			})
	}

	if !leaseFree {
		// The full C-ERR-4 detail set: a client that only knows "409 means
		// someone else has it" still behaves correctly.
		return apperr.WithDetails(apperr.CodeAlreadyClaimed,
			fmt.Sprintf("ticket %s is held by %s until %s", req.TicketID,
				derefString(prior.Claim.Holder), models.FormatMS(*prior.Claim.ExpiresAtMS)),
			conflictDetails(prior, req.TicketID, "lease_active"))
	}

	if req.ExpectedVersion != nil && prior.Version != *req.ExpectedVersion {
		return apperr.WithDetails(apperr.CodeVersionConflict,
			fmt.Sprintf("ticket %s is at version %d, not the expected %d", req.TicketID, prior.Version, *req.ExpectedVersion),
			map[string]any{
				"ticket_id":        req.TicketID,
				"expected_version": *req.ExpectedVersion,
				"current_version":  prior.Version,
			})
	}

	// C-CLAIM-23: the guard and the diagnosis disagree, which must never happen
	// silently.
	return apperr.Newf(apperr.CodeInternal,
		"claim guard rejected ticket %s but no diagnosis row explains it", req.TicketID)
}

// diagnoseRenew explains a 0-row renewal. Order: absent, not the holder, stale
// generation, lapsed lease, status (the defensive row of C-CLAIM-25), internal.
func diagnoseRenew(req RenewRequest, prior ClaimedTicket, found bool, nowMS int64) error {
	if !found {
		return apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("ticket %s not found", req.TicketID),
			map[string]any{"ticket_id": req.TicketID})
	}
	if prior.Claim.Holder == nil || *prior.Claim.Holder != req.Actor.ID {
		return apperr.WithDetails(apperr.CodeNotClaimHolder,
			fmt.Sprintf("ticket %s is not held by %s", req.TicketID, req.Actor.String()),
			map[string]any{
				"ticket_id":   req.TicketID,
				"holder":      derefString(prior.Claim.Holder),
				"holder_type": derefActorType(prior.Claim.HolderType),
			})
	}
	if prior.Claim.Generation != req.Generation {
		return apperr.WithDetails(apperr.CodeLeaseGenerationMismatch,
			fmt.Sprintf("ticket %s is at generation %d, not the presented %d",
				req.TicketID, prior.Claim.Generation, req.Generation),
			map[string]any{
				"ticket_id":           req.TicketID,
				"expected_generation": req.Generation,
				"current_generation":  prior.Claim.Generation,
			})
	}
	if prior.Claim.ExpiresAtMS == nil || *prior.Claim.ExpiresAtMS <= nowMS {
		// C-CLAIM-14: a lapsed lease is never resurrected by its old holder.
		return apperr.WithDetails(apperr.CodeLeaseExpired,
			fmt.Sprintf("the lease on ticket %s lapsed; re-claim it instead of renewing", req.TicketID),
			map[string]any{
				"ticket_id":          req.TicketID,
				"lease_expires_at":   derefMS(prior.Claim.ExpiresAtMS),
				"current_generation": prior.Claim.Generation,
			})
	}
	// Defensive row (C-CLAIM-25): unreachable through the contract's own
	// operations, reachable only by a direct UPDATE. A truthful 409 beats a 500.
	return apperr.WithDetails(apperr.CodeStatusNotClaimable,
		fmt.Sprintf("ticket %s is %q, so its lease cannot be renewed", req.TicketID, string(prior.Status)),
		map[string]any{"ticket_id": req.TicketID, "status": string(prior.Status)})
}

// diagnoseRelease explains a 0-row release. There is deliberately no
// lease_expired row: release has no clock in its guard (C-CLAIM-18).
func diagnoseRelease(req ReleaseRequest, prior ClaimedTicket, found bool) error {
	if !found {
		return apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("ticket %s not found", req.TicketID),
			map[string]any{"ticket_id": req.TicketID})
	}
	if prior.Claim.Holder == nil || *prior.Claim.Holder != req.Actor.ID {
		return apperr.WithDetails(apperr.CodeNotClaimHolder,
			fmt.Sprintf("ticket %s is not held by %s", req.TicketID, req.Actor.String()),
			map[string]any{
				"ticket_id":   req.TicketID,
				"holder":      derefString(prior.Claim.Holder),
				"holder_type": derefActorType(prior.Claim.HolderType),
			})
	}
	if prior.Claim.Generation != req.Generation {
		return apperr.WithDetails(apperr.CodeLeaseGenerationMismatch,
			fmt.Sprintf("ticket %s is at generation %d, not the presented %d",
				req.TicketID, prior.Claim.Generation, req.Generation),
			map[string]any{
				"ticket_id":           req.TicketID,
				"expected_generation": req.Generation,
				"current_generation":  prior.Claim.Generation,
			})
	}
	return apperr.Newf(apperr.CodeInternal,
		"release guard rejected ticket %s but no diagnosis row explains it", req.TicketID)
}

// ---- helpers ---------------------------------------------------------------

// readClaimedTicket reads one ticket with its version and lease columns and
// derives its claim state (contracts.md §2.1: the state is never stored).
func readClaimedTicket(ctx context.Context, q queryRower, id string, nowMS int64) (ClaimedTicket, bool, error) {
	return readClaimedTicketWhere(ctx, q, "WHERE id = ?", []any{id}, nowMS)
}

// readClaimedTicketWhere is readClaimedTicket with the row chosen by the
// caller's predicate. claim_next uses it to read the pre-transition values of
// exactly the row its statement will claim (C-CONV-4 as amended by A1).
func readClaimedTicketWhere(ctx context.Context, q queryRower, where string, args []any, nowMS int64) (ClaimedTicket, bool, error) {
	var t ClaimedTicket
	var epicID, resolution sql.NullString
	var createdAt, updatedAt string
	var holder, holderType sql.NullString
	var acquiredAt, expiresAt sql.NullInt64

	err := q.QueryRowContext(ctx, `
        SELECT id, board_id, epic_id, title, description, status, priority, assignee,
               resolution, created_at, updated_at, version,
               lease_holder, lease_holder_type, lease_acquired_at, lease_expires_at,
               lease_generation, lease_renewals
          FROM tickets `+where, args...).
		Scan(&t.ID, &t.BoardID, &epicID, &t.Title, &t.Description, &t.Status, &t.Priority, &t.Assignee,
			&resolution, &createdAt, &updatedAt, &t.Version,
			&holder, &holderType, &acquiredAt, &expiresAt,
			&t.Claim.Generation, &t.Claim.Renewals)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimedTicket{}, false, nil
	}
	if err != nil {
		return ClaimedTicket{}, false, err
	}

	if epicID.Valid {
		t.EpicID = &epicID.String
	}
	t.Resolution = unmarshalResolution(resolution)
	if t.CreatedAt, err = rfc3339ToTime(createdAt); err != nil {
		return ClaimedTicket{}, false, err
	}
	if t.UpdatedAt, err = rfc3339ToTime(updatedAt); err != nil {
		return ClaimedTicket{}, false, err
	}

	if holder.Valid {
		h := holder.String
		t.Claim.Holder = &h
	}
	if holderType.Valid {
		ht := models.ActorType(holderType.String)
		t.Claim.HolderType = &ht
	}
	if acquiredAt.Valid {
		v := acquiredAt.Int64
		t.Claim.AcquiredAtMS = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Int64
		t.Claim.ExpiresAtMS = &v
	}
	t.Claim.State = claimState(t.Claim, nowMS)
	return t, true, nil
}

// claimState derives ACTIVE / LAPSED / UNCLAIMED from the row and now_ms
// (contracts.md §2.1).
func claimState(c models.Claim, nowMS int64) models.ClaimState {
	if c.Holder == nil {
		return models.ClaimStateUnclaimed
	}
	if c.ExpiresAtMS != nil && *c.ExpiresAtMS > nowMS {
		return models.ClaimStateActive
	}
	return models.ClaimStateLapsed
}

// insertAudit writes one ticket_events row inside the caller's transaction
// (C-AUDIT-4). created_at stays TEXT RFC3339 seconds (C-AUDIT-8); lease values
// inside the payload are unix milliseconds because they are data.
func insertAudit(ctx context.Context, tx *sql.Tx, ticketID string, typ models.TicketEventType,
	actor models.Actor, payload map[string]any, nowMS int64) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
        INSERT INTO ticket_events (id, ticket_id, type, actor, payload, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		newUUID(), ticketID, string(typ), actor.String(), string(body), timeText(nowMS))
	return err
}

// queryRower is the subset of *sql.Tx and *sql.DB the read helper needs, so the
// same scan serves a transaction and a plain read.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// blockingTickets returns the unfinished `blocks` predecessors of a ticket
// (C-CLAIM-8: anything whose status is not `done` is unfinished, including
// `blocked`).
func blockingTickets(ctx context.Context, tx *sql.Tx, ticketID string) ([]struct{ id, status string }, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT b.id, b.status FROM ticket_relations r
          JOIN tickets b ON b.id = r.from_ticket_id
         WHERE r.to_ticket_id = ? AND r.kind = 'blocks' AND b.status <> 'done'
         ORDER BY b.id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ id, status string }
	for rows.Next() {
		var b struct{ id, status string }
		if err := rows.Scan(&b.id, &b.status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// conflictDetails builds the C-ERR-4 detail set for an ownership conflict.
func conflictDetails(t ClaimedTicket, ticketID, reason string) map[string]any {
	return map[string]any{
		"ticket_id":        ticketID,
		"status":           string(t.Status),
		"version":          t.Version,
		"holder":           derefString(t.Claim.Holder),
		"holder_type":      derefActorType(t.Claim.HolderType),
		"lease_expires_at": derefMS(t.Claim.ExpiresAtMS),
		"lease_generation": t.Claim.Generation,
		"reason":           reason,
	}
}

// requireClaimingActor enforces C-CLAIM-11 and C-AUTH-6 at the service
// boundary: humans do not take leases (they reassign), and `system` may claim
// only through the in-process sweeper/worker path, never over a transport.
func (s *SQLiteStore) requireClaimingActor(actor models.Actor) error {
	if err := actor.Validate(); err != nil {
		return invalidActor(err)
	}
	if actor.Type == models.ActorHuman {
		return apperr.WithDetails(apperr.CodeForbidden,
			"humans do not take leases; reassign the ticket instead (C-CLAIM-11)",
			map[string]any{"actor_type": string(actor.Type), "operation": "claim"})
	}
	return nil
}

// isClaimableStatus reports whether status is in the configured claimable set.
func (s *SQLiteStore) isClaimableStatus(status models.Status) bool {
	for _, st := range s.claimableStatuses {
		if st == status {
			return true
		}
	}
	return false
}

// claimableStatusStrings renders the configured set for error details.
func (s *SQLiteStore) claimableStatusStrings() []string {
	out := make([]string, len(s.claimableStatuses))
	for i, st := range s.claimableStatuses {
		out[i] = string(st)
	}
	return out
}

// ParseClaimableStatuses validates LEDGERBOARD_CLAIMABLE_STATUSES (C-CLAIM-3).
// `done` and `blocked` are rejected outright: a config override must not be able
// to create a ticket that is unclaimable by design.
func ParseClaimableStatuses(csv string) ([]models.Status, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, fmt.Errorf("claimable statuses list is empty")
	}
	var out []models.Status
	seen := make(map[models.Status]bool)
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		switch models.Status(name) {
		case models.StatusTodo, models.StatusInProgress:
			st := models.Status(name)
			if !seen[st] {
				seen[st] = true
				out = append(out, st)
			}
		case models.StatusDone, models.StatusBlocked:
			return nil, fmt.Errorf("%q is never claimable and must not appear in the claimable status list", name)
		default:
			return nil, fmt.Errorf("unknown status %q: must be todo or in_progress", name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("claimable statuses list is empty")
	}
	return out, nil
}

// timeText renders an instant as the RFC3339 second-precision TEXT form the
// existing columns use (C-CONV-1). It is only ever written to TEXT columns.
func timeText(nowMS int64) string {
	return time.UnixMilli(nowMS).UTC().Format(time.RFC3339)
}

// invalidActor maps an identity validation failure to 400 invalid_request.
func invalidActor(err error) error {
	return apperr.WithDetails(apperr.CodeInvalidRequest, err.Error(), map[string]any{"field": "actor"})
}

// mapDriverErr turns a SQLite lock failure into the one retryable code
// (C-ERR-7); everything else is returned unchanged for the transport to render
// as internal with the real cause logged.
func mapDriverErr(err error) error {
	var coder interface{ Code() int }
	if errors.As(err, &coder) {
		switch coder.Code() {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return apperr.WithDetails(apperr.CodeDatabaseBusy,
				"the database write lock could not be acquired; retry", map[string]any{"retry_after_ms": 1000})
		}
	}
	return err
}

func derefMS(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefActorType(v *models.ActorType) any {
	if v == nil {
		return nil
	}
	return string(*v)
}
