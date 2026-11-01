package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/memory"
	"github.com/rgracey/kanban-mcp/internal/models"
)

// The outbox status set of contracts.md §6.1 (C-OUTBOX-1). It is validated in Go
// (C-CONV-5): the column is plain TEXT and no CHECK constraint may enforce it.
const (
	OutboxPending    = "pending"
	OutboxProcessing = "processing"
	OutboxRetryable  = "retryable"
	OutboxSucceeded  = "succeeded"
	OutboxUncertain  = "uncertain"
	OutboxFailed     = "failed"
)

// OutboxStatuses is the closed set, in §6.1's order.
func OutboxStatuses() []string {
	return []string{OutboxPending, OutboxProcessing, OutboxRetryable, OutboxSucceeded, OutboxUncertain, OutboxFailed}
}

// ValidOutboxStatus reports whether s is in the closed set.
func ValidOutboxStatus(s string) bool {
	for _, st := range OutboxStatuses() {
		if st == s {
			return true
		}
	}
	return false
}

const (
	// OutboxAttemptLeaseMS is the processing lease of §6.3: a claimed row that is
	// not acked within 60 s becomes reclaimable by any worker (C-OUTBOX-5).
	OutboxAttemptLeaseMS int64 = 60000
	// OutboxMaxAttempts is C-OUTBOX-10's exhaustion boundary: attempts are
	// incremented at claim time, and `attempts >= 8` dead-letters the row.
	OutboxMaxAttempts int64 = 8
	// DefaultWorkerPollMS is LEDGERBOARD_WORKER_POLL_MS's default (C-CONV-10).
	DefaultWorkerPollMS int64 = 1000
	// outboxLastErrorMax caps last_error (C-SCHEMA-16).
	outboxLastErrorMax = 500
)

// The MEMORY_DELIVERY modes (C-OUTBOX-14). The default is off, per the spec.
const (
	MemoryDeliveryOff    = "off"
	MemoryDeliveryWorker = "worker"
)

// DefaultMemoryDelivery is the spec's default (§10, C-OUTBOX-14).
const DefaultMemoryDelivery = MemoryDeliveryOff

// ParseMemoryDelivery validates MEMORY_DELIVERY. Anything other than `off` or
// `worker` is an error naming the variable — never a silent fallback.
func ParseMemoryDelivery(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", MemoryDeliveryOff:
		return MemoryDeliveryOff, nil
	case MemoryDeliveryWorker:
		return MemoryDeliveryWorker, nil
	default:
		return "", fmt.Errorf("invalid MEMORY_DELIVERY %q: must be %q or %q", raw, MemoryDeliveryOff, MemoryDeliveryWorker)
	}
}

// outboxBackoffMS is C-OUTBOX-10's schedule: seconds after the failed attempt,
// indexed by attempts-1. Attempt 7 and later use the last entry; attempts >= 8
// is terminal.
var outboxBackoffMS = []int64{30000, 120000, 600000, 1800000, 7200000, 21600000, 21600000}

// OutboxBackoffMS returns the backoff after the given (already incremented)
// attempt count.
func OutboxBackoffMS(attempts int64) int64 {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > int64(len(outboxBackoffMS)) {
		return outboxBackoffMS[len(outboxBackoffMS)-1]
	}
	return outboxBackoffMS[attempts-1]
}

// OutboxRow is one row of §6.2, typed. Times are unix milliseconds; the wire
// rendering (RFC3339 with three fractional digits, C-CONV-8) belongs to the
// transport slice.
type OutboxRow struct {
	ID            string
	TicketID      string
	HandoffID     string
	BoardID       string
	Actor         string
	ActorType     string
	MemoryType    string
	Content       string
	Confidence    float64
	Tags          []string
	Status        string
	Attempts      int64
	NextAttemptAt *int64
	LeaseOwner    *string
	LeaseExpires  *int64
	ExternalID    *string
	LastError     *string
	CreatedAt     int64
	UpdatedAt     int64
	DeliveredAt   *int64
	RequestID     string
	Idempotency   string
}

// MemoryWrite renders the row as the adapter's view of it (C-OUTBOX-22): the
// stable id is the adapter idempotency key (C-IDEM-5), and nothing outside
// §6.6's struct crosses the boundary.
func (r OutboxRow) MemoryWrite() memory.MemoryWrite {
	return memory.MemoryWrite{
		ID:         r.ID,
		TicketID:   r.TicketID,
		HandoffID:  r.HandoffID,
		Actor:      r.Actor,
		ActorType:  r.ActorType,
		Type:       r.MemoryType,
		Content:    r.Content,
		Confidence: r.Confidence,
		Tags:       r.Tags,
		CreatedAt:  time.UnixMilli(r.CreatedAt).UTC(),
	}
}

// OutboxFilter selects rows for the store-level listing (C-OUTBOX-23's shape).
type OutboxFilter struct {
	Status   string
	TicketID string
	Limit    int
	Offset   int
}

// OutboxRetryRequest is an operator's dead-letter retry (C-OUTBOX-13).
type OutboxRetryRequest struct {
	WriteID   string
	Actor     models.Actor
	RequestID string
}

// OutboxFailure is a classified delivery failure: the status the row moves to
// and the code/message recorded in last_error.
type OutboxFailure struct {
	Status  string // retryable | uncertain | failed
	Code    string
	Message string
}

// outboxColumns is the §6.2 column list in a fixed order, so every scan is
// explicit rather than SELECT *.
const outboxColumns = `id, ticket_id, COALESCE(handoff_id, ''), board_id, actor, actor_type,
       memory_type, content, confidence, tags_json, status, attempts,
       next_attempt_at, lease_owner, lease_expires_at, external_id, last_error,
       created_at, updated_at, delivered_at, request_id, idempotency_key`

// scanOutboxRow reads one §6.2 row.
func scanOutboxRow(sc interface{ Scan(...any) error }) (OutboxRow, error) {
	var (
		row          OutboxRow
		tagsJSON     string
		nextAttempt  sql.NullInt64
		leaseOwner   sql.NullString
		leaseExpires sql.NullInt64
		externalID   sql.NullString
		lastError    sql.NullString
		deliveredAt  sql.NullInt64
	)
	if err := sc.Scan(&row.ID, &row.TicketID, &row.HandoffID, &row.BoardID, &row.Actor,
		&row.ActorType, &row.MemoryType, &row.Content, &row.Confidence, &tagsJSON,
		&row.Status, &row.Attempts, &nextAttempt, &leaseOwner, &leaseExpires,
		&externalID, &lastError, &row.CreatedAt, &row.UpdatedAt, &deliveredAt,
		&row.RequestID, &row.Idempotency); err != nil {
		return OutboxRow{}, err
	}
	if err := json.Unmarshal([]byte(tagsJSON), &row.Tags); err != nil {
		return OutboxRow{}, fmt.Errorf("decode outbox tags_json: %w", err)
	}
	if row.Tags == nil {
		row.Tags = []string{}
	}
	if nextAttempt.Valid {
		v := nextAttempt.Int64
		row.NextAttemptAt = &v
	}
	if leaseOwner.Valid {
		v := leaseOwner.String
		row.LeaseOwner = &v
	}
	if leaseExpires.Valid {
		v := leaseExpires.Int64
		row.LeaseExpires = &v
	}
	if externalID.Valid {
		v := externalID.String
		row.ExternalID = &v
	}
	if lastError.Valid {
		v := lastError.String
		row.LastError = &v
	}
	if deliveredAt.Valid {
		v := deliveredAt.Int64
		row.DeliveredAt = &v
	}
	return row, nil
}

// ReclaimExpiredOutboxRows is C-OUTBOX-6's one-statement pre-pass: every
// `processing` row whose attempt lease has lapsed becomes `uncertain`, because
// the previous worker may have delivered before dying and "unknown" is the only
// honest label.
//
// It is its own transaction on purpose. The contract allows the pre-pass to ride
// inside the claim transaction, but then `uncertain` would never be observable,
// and C-OUTBOX-3 requires it to be visible until it resolves. Committing it
// separately costs one write transaction per worker iteration and buys the
// observable state the contract promises.
//
// The lease is cleared with the status. §6.2 defines `lease_owner` as "worker id
// holding the processing lease", and every other transition clears both fields
// (ack, fail, retry); leaving them set would render an `uncertain` row — the
// state that must be honest and visible (C-OUTBOX-3) — as leased to a worker
// that is gone. C-OUTBOX-6 constrains the guard, not the SET list.
func (s *SQLiteStore) ReclaimExpiredOutboxRows(ctx context.Context) (int64, error) {
	nowMS := s.clock.NowMS()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapDriverErr(err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
        UPDATE outbox
           SET status = 'uncertain', lease_owner = NULL, lease_expires_at = NULL,
               updated_at = ?
         WHERE status = 'processing' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?`,
		nowMS, nowMS)
	if err != nil {
		return 0, mapDriverErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, mapDriverErr(err)
	}
	return n, nil
}

// ClaimOutboxRow claims at most one due row for workerID, using §6.3's
// claim-and-mark statement verbatim: `pending`/`retryable`/`uncertain` rows whose
// next_attempt_at is due, plus any `processing` row whose lease lapsed. The
// attempts counter is incremented *at claim time*, so a crash mid-attempt still
// counts (C-OUTBOX-10).
//
// The claim and the mark are one statement inside one BEGIN IMMEDIATE
// transaction (C-CONV-3), so two workers can never hold the same row.
func (s *SQLiteStore) ClaimOutboxRow(ctx context.Context, workerID string) (OutboxRow, bool, error) {
	if strings.TrimSpace(workerID) == "" {
		return OutboxRow{}, false, apperr.WithDetails(apperr.CodeInvalidRequest,
			"worker id is required", map[string]any{"field": "worker_id"})
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxRow{}, false, mapDriverErr(err)
	}
	defer tx.Rollback()

	row, found, err := claimOutboxRowTx(ctx, tx, workerID, nowMS)
	if err != nil || !found {
		return OutboxRow{}, found, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxRow{}, false, mapDriverErr(err)
	}
	return row, true, nil
}

// claimOutboxRowTx is §6.3's statement plus one read of the columns the claim's
// RETURNING list does not carry (request_id, created_at) — read after the
// deciding mutation, inside the same transaction, never deciding anything
// (C-CONV-4).
func claimOutboxRowTx(ctx context.Context, tx *sql.Tx, workerID string, nowMS int64) (OutboxRow, bool, error) {
	var (
		row      OutboxRow
		tagsJSON string
	)
	err := tx.QueryRowContext(ctx, `
        UPDATE outbox
           SET status = 'processing',
               lease_owner = ?,
               lease_expires_at = ?,
               attempts = attempts + 1,
               updated_at = ?
         WHERE id = (
           SELECT id FROM outbox
            WHERE ( (status IN ('pending','retryable','uncertain')
                     AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
                 OR (status = 'processing' AND lease_expires_at <= ?) )
            ORDER BY created_at ASC, id ASC
            LIMIT 1)
        RETURNING id, ticket_id, handoff_id, actor, actor_type, memory_type,
                  content, confidence, tags_json, attempts, idempotency_key`,
		workerID, nowMS+OutboxAttemptLeaseMS, nowMS, nowMS, nowMS).
		Scan(&row.ID, &row.TicketID, &row.HandoffID, &row.Actor, &row.ActorType,
			&row.MemoryType, &row.Content, &row.Confidence, &tagsJSON,
			&row.Attempts, &row.Idempotency)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, mapDriverErr(err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &row.Tags); err != nil {
		return OutboxRow{}, false, fmt.Errorf("decode outbox tags_json: %w", err)
	}
	if row.Tags == nil {
		row.Tags = []string{}
	}
	row.Status = OutboxProcessing
	row.LeaseOwner = &workerID
	leaseExpires := nowMS + OutboxAttemptLeaseMS
	row.LeaseExpires = &leaseExpires
	row.UpdatedAt = nowMS

	// The remaining fields the MemoryWrite and the audit payload need. A read
	// after the mutation is permitted and decides nothing (C-CONV-4).
	if err := tx.QueryRowContext(ctx,
		`SELECT board_id, created_at, request_id FROM outbox WHERE id = ?`, row.ID).
		Scan(&row.BoardID, &row.CreatedAt, &row.RequestID); err != nil {
		return OutboxRow{}, false, mapDriverErr(err)
	}
	return row, true, nil
}

// AckOutboxRow records a delivery as `succeeded` (C-OUTBOX-2). The guard is
// `WHERE id = ? AND lease_owner = ?`: a worker whose lease was stolen or lapsed
// affects 0 rows, writes no external id, reports no success, and the row belongs
// to whoever holds the lease now (C-OUTBOX-7).
//
// The update and the `memory_delivered` audit row commit together (C-AUDIT-4,
// C-OUTBOX-24), and `memory.pre_ack_commit` fires between them and COMMIT: that
// is the success-then-crash-before-local-ack window of C-OUTBOX-9.
func (s *SQLiteStore) AckOutboxRow(ctx context.Context, row OutboxRow, workerID, externalID string) (bool, error) {
	if externalID == "" {
		// C-OUTBOX-2: success needs a non-empty external id. Nothing else may
		// claim the far end holds the write.
		return false, apperr.New(apperr.CodeInternal, "ack requires a non-empty external id (C-OUTBOX-2)")
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, mapDriverErr(err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
        UPDATE outbox
           SET status = 'succeeded', external_id = ?, delivered_at = ?,
               lease_owner = NULL, lease_expires_at = NULL, last_error = NULL,
               updated_at = ?
         WHERE id = ? AND lease_owner = ?`,
		externalID, nowMS, nowMS, row.ID, workerID)
	if err != nil {
		return false, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		// C-OUTBOX-7: not ours any more. Roll back and say nothing.
		return false, nil
	}

	if err := insertAuditIfTicketExists(ctx, tx, row.TicketID, models.EventMemoryDelivered,
		deliveryActor(workerID), map[string]any{
			"request_id":  row.RequestID,
			"write_id":    row.ID,
			"external_id": externalID,
			"attempts":    row.Attempts,
		}, nowMS); err != nil {
		return false, err
	}

	// Armed, this kills the process after the far end accepted the write and
	// before the local ack commits: the row stays `processing`, and a later
	// worker retries with the same id (C-OUTBOX-9).
	crashpoint.Hit(crashpoint.MemoryPreAckCommit)

	if err := tx.Commit(); err != nil {
		return false, mapDriverErr(err)
	}
	return true, nil
}

// FailOutboxRow records a classified delivery failure. Retryable and uncertain
// rows are rescheduled with C-OUTBOX-10's backoff and are *not* audited per
// attempt (C-OUTBOX-24 — the attempt counter is the record); a row that reaches
// `failed` is audited once with `memory_delivery_failed` in the same transaction.
//
// attempts >= 8 is the exhaustion boundary: whatever the classification, the row
// dead-letters (C-OUTBOX-4, C-OUTBOX-10).
func (s *SQLiteStore) FailOutboxRow(ctx context.Context, row OutboxRow, workerID string, f OutboxFailure) (bool, error) {
	if f.Status != OutboxRetryable && f.Status != OutboxUncertain && f.Status != OutboxFailed {
		return false, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("outbox failure status %q is not retryable, uncertain or failed", f.Status))
	}
	nowMS := s.clock.NowMS()

	status := f.Status
	var nextAttempt any
	if status != OutboxFailed {
		if row.Attempts >= OutboxMaxAttempts {
			status = OutboxFailed
		} else {
			next := nowMS + OutboxBackoffMS(row.Attempts)
			nextAttempt = next
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, mapDriverErr(err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
        UPDATE outbox
           SET status = ?, next_attempt_at = ?, last_error = ?,
               lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
         WHERE id = ? AND lease_owner = ?`,
		status, nextAttempt, scrubLastError(f.Code, f.Message), nowMS, row.ID, workerID)
	if err != nil {
		return false, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		// The lease is gone: the row belongs to another worker now (C-OUTBOX-7).
		return false, nil
	}

	if status == OutboxFailed {
		if err := insertAuditIfTicketExists(ctx, tx, row.TicketID, models.EventMemoryDeliveryFailed,
			deliveryActor(workerID), map[string]any{
				"request_id": row.RequestID,
				"write_id":   row.ID,
				"attempts":   row.Attempts,
				"error_code": f.Code,
			}, nowMS); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, mapDriverErr(err)
	}
	return true, nil
}

// RetryOutboxRow is C-OUTBOX-13's operator capability at the service level: a
// `failed` row is reset to `pending` with attempts 0, next_attempt_at NULL and
// last_error NULL, and the reset is audited.
//
// C-AUTH-7: dead-letter recovery is an operator decision, so an `agent` (and
// `system`) is 403. Retrying a `succeeded` row is 409 outbox_not_retryable —
// the board does not un-prove a delivery.
func (s *SQLiteStore) RetryOutboxRow(ctx context.Context, req OutboxRetryRequest) (OutboxRow, error) {
	if err := req.Actor.Validate(); err != nil {
		return OutboxRow{}, invalidActor(err)
	}
	if req.Actor.Type == models.ActorAgent || req.Actor.Type == models.ActorSystem {
		return OutboxRow{}, apperr.WithDetails(apperr.CodeForbidden,
			"retrying a dead-lettered memory write is a human/orchestrator decision (C-AUTH-7)",
			map[string]any{"actor_type": string(req.Actor.Type), "operation": "outbox.retry"})
	}
	if strings.TrimSpace(req.WriteID) == "" {
		return OutboxRow{}, apperr.WithDetails(apperr.CodeInvalidRequest,
			"write id is required", map[string]any{"field": "id"})
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxRow{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	prior, found, err := readOutboxRowTx(ctx, tx, req.WriteID)
	if err != nil {
		return OutboxRow{}, retryOutboxErr(req.WriteID, err)
	}
	if !found {
		return OutboxRow{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("outbox row %s not found", req.WriteID), map[string]any{"id": req.WriteID})
	}

	res, err := tx.ExecContext(ctx, `
        UPDATE outbox
           SET status = 'pending', attempts = 0, next_attempt_at = NULL,
               last_error = NULL, lease_owner = NULL, lease_expires_at = NULL,
               updated_at = ?
         WHERE id = ? AND status = 'failed'`,
		nowMS, req.WriteID)
	if err != nil {
		return OutboxRow{}, retryOutboxErr(req.WriteID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return OutboxRow{}, retryOutboxErr(req.WriteID, err)
	}
	if affected != 1 {
		return OutboxRow{}, apperr.WithDetails(apperr.CodeOutboxNotRetryable,
			fmt.Sprintf("outbox row %s is %s, not failed", req.WriteID, prior.Status),
			map[string]any{"id": req.WriteID, "status": prior.Status})
	}

	// C-OUTBOX-13 says the reset is audited. §1.8 lists no dedicated type for it,
	// so this uses `outbox_retried` with the row's previous status — a contract
	// gap reported to the architect rather than papered over by reusing
	// memory_delivery_failed, which would assert a failure that no longer holds.
	//
	// The obligation outlives its ticket (C-SCHEMA-13), so the audit is written
	// through the guarded insert: a deleted ticket means no audit row, never a
	// failed reset (C-AUDIT-9).
	if err := insertAuditIfTicketExists(ctx, tx, prior.TicketID, models.EventOutboxRetried,
		req.Actor, map[string]any{
			"request_id":      req.RequestID,
			"write_id":        prior.ID,
			"previous_status": prior.Status,
		}, nowMS); err != nil {
		return OutboxRow{}, retryOutboxErr(req.WriteID, err)
	}

	updated, _, err := readOutboxRowTx(ctx, tx, req.WriteID)
	if err != nil {
		return OutboxRow{}, retryOutboxErr(req.WriteID, err)
	}
	if err := tx.Commit(); err != nil {
		return OutboxRow{}, mapDriverErr(err)
	}
	return updated, nil
}

// retryOutboxErr maps a failure inside the operator's retry to the error shape
// C-OUTBOX-13 defines for that endpoint. The capability and guard failures are
// decided above (403 forbidden, 404 not_found, 409 outbox_not_retryable); what is
// left is infrastructure, so a driver error is mapped first (a lock becomes
// database_busy, C-ERR-7) and anything still untyped becomes a typed `internal`
// rather than a raw driver error reaching the transport as an unclassified 500.
// The real cause is logged and never put in the response (C-ERR-6).
func retryOutboxErr(writeID string, err error) error {
	if err == nil {
		return nil
	}
	mapped := mapDriverErr(err)
	if _, ok := apperr.From(mapped); ok {
		return mapped
	}
	slog.Error("outbox retry failed", "write_id", writeID, "err", mapped)
	return apperr.WithDetails(apperr.CodeInternal,
		"the outbox retry could not be completed", map[string]any{"id": writeID})
}

// GetOutboxRow reads one row by id.
func (s *SQLiteStore) GetOutboxRow(ctx context.Context, id string) (OutboxRow, error) {
	row, found, err := readOutboxRowTx(ctx, s.readDB, id)
	if err != nil {
		return OutboxRow{}, err
	}
	if !found {
		return OutboxRow{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("outbox row %s not found", id), map[string]any{"id": id})
	}
	return row, nil
}

// ListOutboxRows is the store-level shape behind C-OUTBOX-23: newest first, with
// exact status and ticket filters. The transport slice renders it.
func (s *SQLiteStore) ListOutboxRows(ctx context.Context, f OutboxFilter) ([]OutboxRow, error) {
	if f.Status != "" && !ValidOutboxStatus(f.Status) {
		return nil, apperr.WithDetails(apperr.CodeInvalidRequest,
			fmt.Sprintf("unknown outbox status %q", f.Status),
			map[string]any{"field": "status", "allowed": OutboxStatuses()})
	}
	query := `SELECT ` + outboxColumns + ` FROM outbox WHERE 1 = 1`
	var args []any
	if f.Status != "" {
		query += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.TicketID != "" {
		query += ` AND ticket_id = ?`
		args = append(args, f.TicketID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
		if f.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, f.Offset)
		}
	}
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]OutboxRow, 0)
	for rows.Next() {
		row, err := scanOutboxRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OutboxSummary counts rows per status (C-OUTBOX-23's sibling endpoint). Every
// status appears, including the ones with no rows, so a caller never has to
// distinguish "absent" from "zero".
func (s *SQLiteStore) OutboxSummary(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(OutboxStatuses()))
	for _, st := range OutboxStatuses() {
		out[st] = 0
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM outbox GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			n      int64
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// readOutboxRowTx reads one row inside a transaction or a plain handle.
func readOutboxRowTx(ctx context.Context, q rowQuerier, id string) (OutboxRow, bool, error) {
	row, err := scanOutboxRow(q.QueryRowContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, err
	}
	return row, true, nil
}

// deliveryActor is the identity a delivery transition is audited under: the
// worker process, not the author of the memory write. The author travels in the
// row and in the `outbox_enqueued` payload; attributing a delivery to them would
// claim they performed it.
func deliveryActor(workerID string) models.Actor {
	if workerID == "" {
		workerID = "memory-worker"
	}
	return models.Actor{Type: models.ActorSystem, ID: workerID}
}

// credentialPatterns match the shapes C-SCHEMA-16 / C-AUDIT-6 forbid in a stored
// error: a bearer token, an authorization header, or a secret-bearing key=value.
// The bearer pattern runs first so `Authorization: Bearer x` loses the token as
// well as the header name.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|token|password|secret)\b\s*[:=]\s*\S+`),
}

// scrubLastError renders `code: message`, caps it at 500 characters and removes
// anything credential-shaped (C-SCHEMA-16). Classification never reads this
// string back (C-OUTBOX-11) — it exists for humans.
func scrubLastError(code, message string) string {
	text := message
	if code != "" {
		text = code + ": " + message
	}
	for _, re := range credentialPatterns {
		text = re.ReplaceAllString(text, "[redacted]")
	}
	if len(text) > outboxLastErrorMax {
		text = text[:outboxLastErrorMax]
	}
	return text
}

// deliveryMessage is the adapter's own words without its code, because
// scrubLastError prefixes the code itself and "code: code: message" reads as a
// bug in the log rather than a fact about the far end.
func deliveryMessage(de *memory.DeliveryError) string {
	if de.Err != nil {
		return de.Err.Error()
	}
	return de.Code
}

// ClassifyDeliveryError maps an adapter error to the outbox's classification,
// explicitly and never by string matching (C-OUTBOX-11):
//
//	*DeliveryError{Retryable: true}  -> retryable
//	*DeliveryError{Retryable: false} -> failed
//	adapter_timeout / ctx cancelled  -> uncertain
//	anything unclassified            -> uncertain
func ClassifyDeliveryError(err error) OutboxFailure {
	if err == nil {
		return OutboxFailure{Status: OutboxUncertain, Code: "unknown", Message: "no error"}
	}
	var de *memory.DeliveryError
	if errors.As(err, &de) {
		if de.Code == memory.CodeAdapterTimeout {
			// A timed-out request may have arrived: never a definite failure.
			return OutboxFailure{Status: OutboxUncertain, Code: de.Code, Message: deliveryMessage(de)}
		}
		if de.Retryable {
			return OutboxFailure{Status: OutboxRetryable, Code: de.Code, Message: deliveryMessage(de)}
		}
		return OutboxFailure{Status: OutboxFailed, Code: de.Code, Message: deliveryMessage(de)}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return OutboxFailure{Status: OutboxUncertain, Code: memory.CodeAdapterTimeout, Message: err.Error()}
	}
	return OutboxFailure{Status: OutboxUncertain, Code: "unclassified", Message: err.Error()}
}
