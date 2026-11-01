// Package apperr is the one error type of the service layer (docs/contracts.md
// C-ERR-1) and the one place an error code maps to an HTTP status (§7.4).
//
// Transports render it; they never invent a code and never map a status
// themselves. The store returns it for every business failure, so REST and MCP
// cannot disagree about what went wrong.
package apperr

import (
	"errors"
	"fmt"
)

// Code is the closed error-code enum of §7.4. Adding a code is a contract
// change; a code that is not in the table is a defect.
type Code string

// The code table (§7.4), in the document's order.
const (
	CodeInvalidRequest          Code = "invalid_request"
	CodeUnknownField            Code = "unknown_field"
	CodeNotFound                Code = "not_found"
	CodeForbidden               Code = "forbidden"
	CodeUnauthorized            Code = "unauthorized" // reserved, never emitted in v1
	CodeAlreadyClaimed          Code = "already_claimed"
	CodeNotClaimed              Code = "not_claimed"
	CodeNotClaimHolder          Code = "not_claim_holder"
	CodeLeaseExpired            Code = "lease_expired"
	CodeLeaseGenerationMismatch Code = "lease_generation_mismatch"
	CodeVersionConflict         Code = "version_conflict"
	CodeStatusNotClaimable      Code = "status_not_claimable"
	CodeDependencyBlocked       Code = "dependency_blocked"
	CodeIdempotencyConflict     Code = "idempotency_conflict"
	CodeIdempotencyInProgress   Code = "idempotency_in_progress" // reserved
	CodeHandoffInvalid          Code = "handoff_invalid"
	CodeHandoffNotAcceptable    Code = "handoff_not_acceptable"
	CodeOutboxNotRetryable      Code = "outbox_not_retryable"
	CodeMemoryDeliveryDisabled  Code = "memory_delivery_disabled"
	CodeSearchUnsupported       Code = "search_unsupported"
	CodePayloadTooLarge         Code = "payload_too_large" // reserved
	CodeRateLimited             Code = "rate_limited"      // reserved
	CodeDatabaseBusy            Code = "database_busy"
	CodeInternal                Code = "internal"
)

// httpStatus is the normative code → REST status table of §7.4.
var httpStatus = map[Code]int{
	CodeInvalidRequest:          400,
	CodeUnknownField:            400,
	CodeNotFound:                404,
	CodeForbidden:               403,
	CodeUnauthorized:            401,
	CodeAlreadyClaimed:          409,
	CodeNotClaimed:              409,
	CodeNotClaimHolder:          409,
	CodeLeaseExpired:            409,
	CodeLeaseGenerationMismatch: 409,
	CodeVersionConflict:         409,
	CodeStatusNotClaimable:      409,
	CodeDependencyBlocked:       409,
	CodeIdempotencyConflict:     409,
	CodeIdempotencyInProgress:   409,
	CodeHandoffInvalid:          400,
	CodeHandoffNotAcceptable:    409,
	CodeOutboxNotRetryable:      409,
	CodeMemoryDeliveryDisabled:  409,
	CodeSearchUnsupported:       501,
	CodePayloadTooLarge:         413,
	CodeRateLimited:             429,
	CodeDatabaseBusy:            503,
	CodeInternal:                500,
}

// HTTPStatus returns the REST status for the code. An unknown code is mapped to
// 500 (C-ERR-2: the renderer maps it to internal while logging loudly) rather
// than to 200 or a panic.
func (c Code) HTTPStatus() int {
	if s, ok := httpStatus[c]; ok {
		return s
	}
	return 500
}

// Known reports whether the code is in the table.
func (c Code) Known() bool {
	_, ok := httpStatus[c]
	return ok
}

// Error is the single service-layer error (C-ERR-1).
//
// Message is the human sentence that has always been in the `error` field
// (C-ERR-8); Details carries the structured fields §7.4 requires for the code;
// RequestID joins the response to the server log (C-ERR-5).
type Error struct {
	Code      Code
	Message   string
	Details   map[string]any
	RequestID string
}

// Error implements error. The prefix is the contract's machine-matchable form
// `<code>: <message>` (C-ERR-11).
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// WithRequestID returns a copy carrying the request correlation id.
func (e *Error) WithRequestID(id string) *Error {
	cp := *e
	cp.RequestID = id
	return &cp
}

// WithDetail returns a copy with one structured detail added.
func (e *Error) WithDetail(key string, value any) *Error {
	cp := *e
	cp.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		cp.Details[k] = v
	}
	cp.Details[key] = value
	return &cp
}

// New builds an error with a code and a human message.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an error with a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetails builds an error carrying structured details.
func WithDetails(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// From extracts the *Error from err, or reports false when err is not a service
// error. An unexpected error is the caller's cue to render `internal` and log
// the real cause (C-ERR-6) — never to leak err.Error().
func From(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Is reports whether err is a service error with the given code.
func Is(err error, code Code) bool {
	e, ok := From(err)
	return ok && e.Code == code
}
