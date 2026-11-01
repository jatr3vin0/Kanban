// Package memory is the outbox worker's only view of the outside world: the
// adapter interface of docs/contracts.md §6.6 (C-OUTBOX-19..22) plus a mock
// adapter for tests.
//
// The real operator-memory adapter (a client for the operator's Python MCP
// server) is blocked externally — see recon.md §9/§12 and the business plan's
// "Blocked externally". Nothing in the outbox or in this package depends on it
// existing: the interface is the seam, and the mock proves the delivery path
// end to end.
package memory

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DeliveryError codes (§6.6). They are the *only* thing the outbox classifies
// on — never message text (C-OUTBOX-11, recon §10).
const (
	CodeAdapterUnavailable   = "adapter_unavailable"
	CodeAdapterTimeout       = "adapter_timeout"
	CodeAdapterRejected      = "adapter_rejected"
	CodeAdapterNotConfigured = "adapter_not_configured"
)

// MemoryWrite is one durable assertion the board wants the operator's memory to
// hold (contracts.md §6.6).
//
// It is the whole of what an adapter may see (C-OUTBOX-22): no SQLite handle, no
// ticket row, no request context. A future out-of-process adapter is therefore a
// drop-in.
type MemoryWrite struct {
	ID         string // stable write id == outbox.id; the adapter idempotency key
	TicketID   string
	HandoffID  string
	Actor      string // "agent:builder-1"
	ActorType  string
	Type       string // fact|decision|requirement|finding|preference|continuation|lesson
	Content    string
	Confidence float64
	Tags       []string
	CreatedAt  time.Time // UTC
}

// MemoryRecord is one record the operator's memory holds (§6.6).
type MemoryRecord struct {
	ExternalID string
	Type       string
	Content    string
	Confidence float64
	Tags       []string
	CreatedAt  time.Time
}

// Adapter is the only thing the outbox worker knows about the outside world.
type Adapter interface {
	// Write must be idempotent on w.ID: a repeated Write with the same ID must
	// return the same ExternalID, not create a second record.
	Write(ctx context.Context, w MemoryWrite) (externalID string, err error)
	Get(ctx context.Context, externalID string) (MemoryRecord, error)
}

// Searcher is OPTIONAL. Implementations that cannot search simply do not
// implement it (C-OUTBOX-19); the transport layer detects it once at wiring time
// by type assertion and answers 501 search_unsupported when it is missing
// (C-OUTBOX-20) — never an empty list.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]MemoryRecord, error)
}

// DeliveryError is the adapter's explicit classification of a failed write
// (C-OUTBOX-11). Retryable is the adapter's judgement about the *far end*; the
// outbox maps it to `retryable` / `failed`, except that a timeout is always
// `uncertain` because a request that may have arrived cannot be called a
// definite failure.
type DeliveryError struct {
	Code      string // adapter_unavailable | adapter_timeout | adapter_rejected | adapter_not_configured
	Retryable bool
	Err       error
}

// Error implements error.
func (e *DeliveryError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *DeliveryError) Unwrap() error { return e.Err }

// NewDeliveryError builds a DeliveryError without an underlying cause.
func NewDeliveryError(code string, retryable bool, format string, args ...any) *DeliveryError {
	return &DeliveryError{Code: code, Retryable: retryable, Err: fmt.Errorf(format, args...)}
}

// IsSearcher reports whether a is wired with the optional Search operation. The
// detection happens once, at wiring time (C-OUTBOX-19), not per call.
func IsSearcher(a Adapter) bool {
	_, ok := a.(Searcher)
	return ok
}

// ErrNotFound is what Get returns for an external id the far end does not hold.
var ErrNotFound = errors.New("memory record not found")
