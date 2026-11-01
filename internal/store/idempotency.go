package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/models"
)

// The two contract constants of C-IDEM-16. Both are contract values, not tuning
// knobs: the tests assert them and changing either is a contract revision.
const (
	// IdempotencyRetentionMS is the 72-hour retention window (C-IDEM-16):
	// expires_at = created_at + 259_200_000.
	IdempotencyRetentionMS int64 = 259_200_000
	// IdempotencyPurgeIntervalMS is the in-process rate limit on the
	// opportunistic purge (C-IDEM-16). It lives on the store as one value and is
	// deliberately not shared state: two processes on one database may both
	// purge, which is safe because no correctness depends on it (C-IDEM-19).
	IdempotencyPurgeIntervalMS int64 = 600_000
)

// Operation is the fixed operation name of §4.1 (C-IDEM-3). The service layer
// assigns it; a client can never make two different operations share a scope by
// lying about it.
type Operation string

const (
	OperationTicketCreate       Operation = "ticket.create"
	OperationTicketBulkCreate   Operation = "ticket.bulk_create"
	OperationTicketClaim        Operation = "ticket.claim"
	OperationHandoffCreate      Operation = "handoff.create"
	OperationHandoffAccept      Operation = "handoff.accept"
	OperationMemoryWriteSubmit  Operation = "memory.write.submit"
	OperationMemoryWriteDeliver Operation = "memory.write.deliver"
)

// operations is the closed §4.1 table, with the "Key required?" column.
var operations = map[Operation]bool{
	OperationTicketCreate:       false, // optional
	OperationTicketBulkCreate:   false, // optional
	OperationTicketClaim:        true,  // required
	OperationHandoffCreate:      true,  // required
	OperationHandoffAccept:      true,  // required
	OperationMemoryWriteSubmit:  true,  // required
	OperationMemoryWriteDeliver: true,  // the outbox id IS the key (C-IDEM-5)
}

// Valid reports whether o is one of the fixed operation names (C-IDEM-3).
func (o Operation) Valid() bool {
	_, ok := operations[o]
	return ok
}

// RequiresKey implements the §4.1 "Key required?" column (C-IDEM-4). An unknown
// operation reports true so that a missing key on a typo is an error rather than
// a silently un-deduplicated mutation.
func (o Operation) RequiresKey() bool {
	required, ok := operations[o]
	if !ok {
		return true
	}
	return required
}

// idempotencyKeyPattern is C-IDEM-2's grammar: 1–128 characters from
// [A-Za-z0-9._:-].
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// requestHashPattern is the lowercase hex SHA-256 C-IDEM-10 produces.
var requestHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IdempotencyScope is the (actor, operation, key) triple of C-IDEM-1 plus the
// request hash of C-IDEM-10.
//
// Actor is server-derived (C-AUTH-1) and is rendered `type:id`, the same form
// every other identity column uses (C-AUTH-2). The key is never derived from the
// payload server-side and never exposed (C-IDEM-2, C-IDEM-17).
type IdempotencyScope struct {
	Actor       models.Actor
	Operation   Operation
	Key         string
	RequestHash string
}

// Validate enforces C-IDEM-2 (key grammar), C-IDEM-3 (operation) and the hash
// shape, at the service boundary so REST, MCP and CLI cannot disagree.
//
// An empty key is *not* an error here: whether it is required depends on the
// operation (C-IDEM-4) and is decided by validateForOperation, which knows the
// operation the caller actually declared.
func (sc IdempotencyScope) Validate() error {
	if err := sc.Actor.Validate(); err != nil {
		return invalidActor(err)
	}
	if !sc.Operation.Valid() {
		return apperr.WithDetails(apperr.CodeInvalidRequest,
			fmt.Sprintf("unknown idempotency operation %q", sc.Operation),
			map[string]any{"field": "operation"})
	}
	if sc.Key == "" {
		return nil
	}
	if !idempotencyKeyPattern.MatchString(sc.Key) {
		return apperr.WithDetails(apperr.CodeInvalidRequest,
			"idempotency_key must be 1-128 characters from [A-Za-z0-9._:-]",
			map[string]any{"field": "idempotency_key"})
	}
	if !requestHashPattern.MatchString(sc.RequestHash) {
		return apperr.WithDetails(apperr.CodeInvalidRequest,
			"request_hash must be the lowercase hex SHA-256 of the canonical request (C-IDEM-10)",
			map[string]any{"field": "request_hash"})
	}
	return nil
}

// validateForOperation applies C-IDEM-4's required/optional split: a required
// operation with no key is a 400 naming the field; an optional one with no key
// executes with no idempotency record at all.
func (sc IdempotencyScope) validateForOperation() error {
	if err := sc.Validate(); err != nil {
		return err
	}
	if sc.Key == "" && sc.Operation.RequiresKey() {
		return apperr.WithDetails(apperr.CodeInvalidRequest,
			fmt.Sprintf("operation %q requires an idempotency key", sc.Operation),
			map[string]any{"field": "idempotency_key"})
	}
	return nil
}

// StoredResponse is what a mutation produced and what a replay returns:
// `response_body` is stored verbatim so a replay is byte-identical (C-IDEM-6,
// C-SCHEMA-18), and `entity_id` duplicates the created entity id for
// `Location` reconstruction.
type StoredResponse struct {
	// Status is the HTTP status the first call returned. Only 2xx is recorded
	// (C-IDEM-8), so a non-2xx value here is a programming error.
	Status int
	// Body is the exact JSON text returned to the first caller.
	Body []byte
	// EntityID is the created or mutated entity's id ("" when the operation has
	// no single entity).
	EntityID string
}

// IdempotentResult is one RunIdempotent outcome. Replayed reports that the
// stored response was returned instead of executing the mutation again
// (C-IDEM-6); the transport renders it as `Idempotency-Replayed: true`.
type IdempotentResult struct {
	Response StoredResponse
	Replayed bool
}

// MutationFunc is the mutation RunIdempotent executes inside its transaction.
// It must do all of its work — guard, mutation, audit, outbox — on the tx it is
// given and must not commit or roll back: that is what makes the key row and the
// mutation commit together (C-IDEM-12).
type MutationFunc func(ctx context.Context, tx *sql.Tx) (StoredResponse, error)

// RunIdempotent executes fn under the idempotency scope of C-IDEM-1 in one
// BEGIN IMMEDIATE transaction, with the key row written in that same transaction
// (C-IDEM-12, C-CONV-3).
//
// The order inside the transaction is:
//
//  1. attempt the INSERT of the new key row (C-IDEM-19). A primary-key conflict
//     is diagnosed by re-reading that row — for diagnosis only, never to decide
//     whether to write (C-CONV-4) — and resolves to exactly one of: replay
//     (C-IDEM-6), conflict (C-IDEM-7), or "expired behaves as absent", which
//     deletes the dead row and inserts again (C-IDEM-19);
//  2. run fn;
//  3. fill in response_status/response_body/entity_id and commit.
//
// A failure in step 2 rolls the whole transaction back, so no key row survives a
// 4xx/5xx and the same key re-executes next time (C-IDEM-8). There is therefore
// no state in which a committed mutation lacks its key row (C-IDEM-13), and
// `in_flight` is never observable after a commit (C-IDEM-14).
func (s *SQLiteStore) RunIdempotent(ctx context.Context, scope IdempotencyScope, fn MutationFunc) (IdempotentResult, error) {
	if err := scope.validateForOperation(); err != nil {
		return IdempotentResult{}, err
	}

	// C-IDEM-4: an optional operation with no key executes with no idempotency
	// record at all — there is nothing to deduplicate against.
	if scope.Key == "" {
		return s.runWithoutRecord(ctx, fn)
	}
	nowMS := s.clock.NowMS()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdempotentResult{}, mapDriverErr(err)
	}
	defer tx.Rollback()

	// The opportunistic purge deliberately does not run here: C-IDEM-16 forbids a
	// purge inside a request that is itself being deduplicated.
	replayed, stored, err := s.reserveIdempotencySlot(ctx, tx, scope, nowMS)
	if err != nil {
		return IdempotentResult{}, err
	}
	if replayed {
		return IdempotentResult{Response: stored, Replayed: true}, nil
	}

	resp, err := fn(ctx, tx)
	if err != nil {
		// Rollback: a failed request is never recorded (C-IDEM-8).
		return IdempotentResult{}, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		// Only 2xx is recordable (C-IDEM-8); recording a non-2xx would freeze a
		// failure into the key's 72-hour window.
		return IdempotentResult{}, apperr.Newf(apperr.CodeInternal,
			"mutation produced status %d under an idempotency key: only 2xx is recorded (C-IDEM-8)", resp.Status)
	}

	if _, err := tx.ExecContext(ctx, `
        UPDATE idempotency_keys
           SET status = 'completed', response_status = ?, response_body = ?, entity_id = ?
         WHERE actor = ? AND operation = ? AND key = ?`,
		resp.Status, string(resp.Body), nullableString(resp.EntityID),
		scope.Actor.String(), string(scope.Operation), scope.Key); err != nil {
		return IdempotentResult{}, mapDriverErr(err)
	}

	if err := tx.Commit(); err != nil {
		return IdempotentResult{}, mapDriverErr(err)
	}
	return IdempotentResult{Response: resp}, nil
}

// runWithoutRecord executes an optional operation's mutation in one transaction
// with no key row (C-IDEM-4).
func (s *SQLiteStore) runWithoutRecord(ctx context.Context, fn MutationFunc) (IdempotentResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdempotentResult{}, mapDriverErr(err)
	}
	defer tx.Rollback()
	resp, err := fn(ctx, tx)
	if err != nil {
		return IdempotentResult{}, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return IdempotentResult{}, apperr.Newf(apperr.CodeInternal,
			"mutation produced status %d without an idempotency key", resp.Status)
	}
	if err := tx.Commit(); err != nil {
		return IdempotentResult{}, mapDriverErr(err)
	}
	return IdempotentResult{Response: resp}, nil
}

// reserveIdempotencySlot implements C-IDEM-19's insert-then-diagnose dance. It
// reports whether the caller must replay the stored response instead of running
// the mutation.
func (s *SQLiteStore) reserveIdempotencySlot(ctx context.Context, tx *sql.Tx, scope IdempotencyScope, nowMS int64) (bool, StoredResponse, error) {
	inserted, err := s.insertIdempotencySlot(ctx, tx, scope, nowMS)
	if err != nil {
		return false, StoredResponse{}, err
	}
	if inserted {
		return false, StoredResponse{}, nil
	}

	// The insert hit the primary key (C-SCHEMA-17). Re-read the row for diagnosis
	// only; the writes below are unconditional (C-CONV-4, C-IDEM-19).
	var (
		storedHash   string
		status       string
		expiresAt    int64
		responseCode sql.NullInt64
		responseBody sql.NullString
		entityID     sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
        SELECT request_hash, status, expires_at, response_status, response_body, entity_id
          FROM idempotency_keys
         WHERE actor = ? AND operation = ? AND key = ?`,
		scope.Actor.String(), string(scope.Operation), scope.Key).
		Scan(&storedHash, &status, &expiresAt, &responseCode, &responseBody, &entityID)
	if err != nil {
		return false, StoredResponse{}, mapDriverErr(err)
	}

	if expiresAt <= nowMS {
		// C-IDEM-19: an expired key row behaves as absent, purged or not. Delete
		// it and insert again, inside this transaction.
		if err := s.deleteIdempotencySlot(ctx, tx, scope); err != nil {
			return false, StoredResponse{}, err
		}
		inserted, err := s.insertIdempotencySlot(ctx, tx, scope, nowMS)
		if err != nil {
			return false, StoredResponse{}, err
		}
		if !inserted {
			// Unreachable under the write lock this transaction holds.
			return false, StoredResponse{}, apperr.New(apperr.CodeInternal,
				"expired idempotency row could not be replaced")
		}
		return false, StoredResponse{}, nil
	}

	if storedHash != scope.RequestHash {
		// C-IDEM-7: details carry the *stored* hash, never the new one.
		return false, StoredResponse{}, apperr.WithDetails(apperr.CodeIdempotencyConflict,
			fmt.Sprintf("idempotency key %q was used with a different request payload", scope.Key),
			map[string]any{
				"operation":    string(scope.Operation),
				"key":          scope.Key,
				"request_hash": storedHash,
			})
	}

	if status != "completed" {
		// C-IDEM-14: in_flight exists only inside an uncommitted transaction, so a
		// row seen in that state is stale (a crashed process, a future
		// multi-process deployment). C-IDEM-8's rule applies — the request is
		// retryable, so the stale row is replaced and the mutation executes.
		if err := s.deleteIdempotencySlot(ctx, tx, scope); err != nil {
			return false, StoredResponse{}, err
		}
		inserted, err := s.insertIdempotencySlot(ctx, tx, scope, nowMS)
		if err != nil {
			return false, StoredResponse{}, err
		}
		if !inserted {
			return false, StoredResponse{}, apperr.New(apperr.CodeInternal,
				"stale in_flight idempotency row could not be replaced")
		}
		return false, StoredResponse{}, nil
	}

	if !responseCode.Valid {
		return false, StoredResponse{}, apperr.New(apperr.CodeInternal,
			"completed idempotency row has no recorded response (C-IDEM-12)")
	}
	return true, StoredResponse{
		Status:   int(responseCode.Int64),
		Body:     []byte(responseBody.String),
		EntityID: entityID.String,
	}, nil
}

// insertIdempotencySlot inserts the in_flight row. It reports false (with a nil
// error) when the primary key already exists — the conflict signal of
// C-SCHEMA-17, never a prior SELECT.
func (s *SQLiteStore) insertIdempotencySlot(ctx context.Context, tx *sql.Tx, scope IdempotencyScope, nowMS int64) (bool, error) {
	_, err := tx.ExecContext(ctx, `
        INSERT INTO idempotency_keys
               (actor, operation, key, request_hash, status, created_at, expires_at)
        VALUES (?, ?, ?, ?, 'in_flight', ?, ?)`,
		scope.Actor.String(), string(scope.Operation), scope.Key, scope.RequestHash,
		nowMS, nowMS+IdempotencyRetentionMS)
	if err == nil {
		return true, nil
	}
	if isUniqueViolation(err) {
		return false, nil
	}
	return false, mapDriverErr(err)
}

// deleteIdempotencySlot removes one key row inside the caller's transaction.
func (s *SQLiteStore) deleteIdempotencySlot(ctx context.Context, tx *sql.Tx, scope IdempotencyScope) error {
	_, err := tx.ExecContext(ctx, `
        DELETE FROM idempotency_keys WHERE actor = ? AND operation = ? AND key = ?`,
		scope.Actor.String(), string(scope.Operation), scope.Key)
	return mapDriverErr(err)
}

// PurgeIdempotencyKeysOnStartup is C-IDEM-16(a): the purge that runs once, when
// the server process starts. It also seeds the in-process rate limit so the
// opportunistic path does not immediately repeat the work.
func (s *SQLiteStore) PurgeIdempotencyKeysOnStartup(ctx context.Context) (int64, error) {
	nowMS := s.clock.NowMS()
	n, err := s.purgeExpiredIdempotencyKeys(ctx, nowMS)
	if err != nil {
		return 0, err
	}
	s.lastPurgeMS.Store(nowMS)
	return n, nil
}

// PurgeIdempotencyKeys is the explicit purge: it ignores the rate limit and
// deletes every expired key row in its own write transaction. Housekeeping only —
// no correctness depends on it (C-IDEM-19).
func (s *SQLiteStore) PurgeIdempotencyKeys(ctx context.Context) (int64, error) {
	return s.purgeExpiredIdempotencyKeys(ctx, s.clock.NowMS())
}

// purgeExpiredIdempotencyKeys deletes `expires_at < :now_ms` in one statement
// inside its own BEGIN IMMEDIATE transaction (C-IDEM-16).
func (s *SQLiteStore) purgeExpiredIdempotencyKeys(ctx context.Context, nowMS int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapDriverErr(err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < ?`, nowMS)
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

// MaybePurgeIdempotencyTx is C-IDEM-16(b): the opportunistic purge, called from a
// write transaction that is *not* itself being deduplicated. The 10-minute rate
// limit is one in-process value on the store (`last_purge_ms`), so each process
// keeps its own schedule and no table or column is involved.
//
// It returns how many rows it deleted (0 when the rate limit skipped it).
func (s *SQLiteStore) MaybePurgeIdempotencyTx(ctx context.Context, tx *sql.Tx, nowMS int64) (int64, error) {
	last := s.lastPurgeMS.Load()
	if last != 0 && nowMS-last < IdempotencyPurgeIntervalMS {
		return 0, nil
	}
	// One winner per process per window: a concurrent caller that lost the swap
	// skips rather than purging twice.
	if !s.lastPurgeMS.CompareAndSwap(last, nowMS) {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < ?`, nowMS)
	if err != nil {
		return 0, mapDriverErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RequestHash returns the lowercase hex SHA-256 of the canonical JSON of v
// (C-IDEM-10). Callers pass the *decoded* request they are about to act on, so
// fields the server ignores are excluded by construction and field order and
// whitespace in the wire body cannot change the hash.
//
// encoding/json emits object keys sorted by byte value with no insignificant
// whitespace, which is exactly the canonical form the rule names; numbers are
// rendered in Go's shortest round-trip form.
func RequestHash(v any) (string, error) {
	canonical, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("canonical request JSON: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// isUniqueViolation reports whether err is SQLite's PRIMARY KEY / UNIQUE
// constraint failure. modernc.org/sqlite exposes the extended result code, so the
// check is by code (19 = SQLITE_CONSTRAINT, 1555 = SQLITE_CONSTRAINT_PRIMARYKEY)
// with a message fallback for the plain driver error shape.
func isUniqueViolation(err error) bool {
	var coder interface{ Code() int }
	if errors.As(err, &coder) {
		switch coder.Code() {
		case 19, 1555, 2067: // CONSTRAINT, CONSTRAINT_PRIMARYKEY, CONSTRAINT_UNIQUE
			return true
		}
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "constraint failed")
}

// nullableString maps "" to SQL NULL, so entity_id is NULL for operations with no
// single entity rather than an empty string that looks like an id.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
