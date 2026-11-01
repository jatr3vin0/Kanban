package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// ActorType is the closed set of identity kinds (docs/contracts.md C-AUTH-1).
type ActorType string

const (
	ActorHuman        ActorType = "human"
	ActorAgent        ActorType = "agent"
	ActorOrchestrator ActorType = "orchestrator"
	ActorSystem       ActorType = "system"
)

// Valid reports whether t is one of the four actor types.
func (t ActorType) Valid() bool {
	switch t {
	case ActorHuman, ActorAgent, ActorOrchestrator, ActorSystem:
		return true
	}
	return false
}

// actorIDPattern is C-AUTH-2's actor id grammar.
var actorIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// Actor is a server-derived identity (C-AUTH-1, C-AUTH-2): a type plus an id,
// rendered `type:id` in lease_holder, ticket_events.actor and
// handoffs.from_actor.
//
// It is never built from a request body. The transport derives it from trusted
// local configuration or a loopback-trusted header and hands it to the service
// layer; anything a client sends as `actor` or `actor_type` is ignored
// (C-FENCE-12).
type Actor struct {
	Type ActorType
	ID   string
}

// String renders the actor as `type:id`.
func (a Actor) String() string {
	return string(a.Type) + ":" + a.ID
}

// Validate rejects an unknown type or a malformed id. It is called at the
// service boundary so no transport can introduce an identity the store cannot
// reason about (C-CONV-5).
func (a Actor) Validate() error {
	if !a.Type.Valid() {
		return fmt.Errorf("invalid actor type %q: must be human, agent, orchestrator, or system", a.Type)
	}
	if !actorIDPattern.MatchString(a.ID) {
		return fmt.Errorf("invalid actor id %q: must be 1-200 characters from [A-Za-z0-9._:-]", a.ID)
	}
	return nil
}

// ClaimState is the derived ownership state of a ticket (contracts.md §2.1).
// It is never stored: it is computed from the lease columns and now_ms, because
// a stored state machine would be a second source of truth that can disagree
// with the lease fields.
type ClaimState string

const (
	ClaimStateUnclaimed ClaimState = "unclaimed"
	ClaimStateActive    ClaimState = "active"
	ClaimStateLapsed    ClaimState = "lapsed"
)

// Claim is the ownership block returned with every mutating ticket response
// (C-CONV-7). Times are unix milliseconds in the struct; MarshalJSON renders
// them per C-CONV-8 (RFC3339 UTC, three fractional digits).
type Claim struct {
	State        ClaimState `json:"state"`
	Holder       *string    `json:"holder"`
	HolderType   *ActorType `json:"holder_type"`
	AcquiredAtMS *int64     `json:"-"`
	ExpiresAtMS  *int64     `json:"-"`
	Generation   int64      `json:"generation"`
	Renewals     int64      `json:"renewals"`
}

// claimJSON is the wire shape of Claim (C-CONV-8: INTEGER ms columns are
// serialized as RFC3339 UTC with exactly three fractional digits).
type claimJSON struct {
	State      ClaimState `json:"state"`
	Holder     *string    `json:"holder"`
	HolderType *ActorType `json:"holder_type"`
	AcquiredAt *string    `json:"acquired_at"`
	ExpiresAt  *string    `json:"expires_at"`
	Generation int64      `json:"generation"`
	Renewals   int64      `json:"renewals"`
}

// MarshalJSON implements C-CONV-8.
func (c Claim) MarshalJSON() ([]byte, error) {
	out := claimJSON{
		State:      c.State,
		Holder:     c.Holder,
		HolderType: c.HolderType,
		Generation: c.Generation,
		Renewals:   c.Renewals,
	}
	if c.AcquiredAtMS != nil {
		s := FormatMS(*c.AcquiredAtMS)
		out.AcquiredAt = &s
	}
	if c.ExpiresAtMS != nil {
		s := FormatMS(*c.ExpiresAtMS)
		out.ExpiresAt = &s
	}
	return jsonMarshal(out)
}

// FormatMS renders a unix-millisecond instant as RFC3339 UTC with exactly three
// fractional digits, e.g. 2026-10-30T23:08:22.180Z (C-CONV-8).
func FormatMS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonMarshal is encoding/json.Marshal, kept behind a one-line helper so this
// file's import list stays about identity and ownership rather than encoding.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
