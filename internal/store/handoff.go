package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/models"
)

// The three handoff statuses of §5.2 / C-SCHEMA-9. There is no fourth: the only
// writes after creation are the acceptance transition and supersession
// (C-HANDOFF-10).
const (
	HandoffStatusOffered    = "offered"
	HandoffStatusAccepted   = "accepted"
	HandoffStatusSuperseded = "superseded"
)

// HandoffSchemaVersion is the only schema_version v1 accepts (§5.2). A future
// version is a new contract, never a guess (C-HANDOFF-1).
const HandoffSchemaVersion = 1

// HandoffMaxCanonicalBytes is C-HANDOFF-2's total-size backstop.
const HandoffMaxCanonicalBytes = 65536

// memoryTypes is the §5.3 enum (C-HANDOFF-5, C-SCHEMA-15).
var memoryTypes = map[string]bool{
	"fact": true, "decision": true, "requirement": true, "finding": true,
	"preference": true, "continuation": true, "lesson": true,
}

// MemoryTypes returns the §5.3 enum in a stable order, for error details
// (`details.allowed`, C-HANDOFF-5).
func MemoryTypes() []string {
	return []string{"fact", "decision", "requirement", "finding", "preference", "continuation", "lesson"}
}

// artifactKinds is the §5.2 `artifacts[].kind` enum.
var artifactKinds = map[string]bool{
	"commit": true, "pr": true, "file": true, "url": true, "log": true, "other": true,
}

var (
	toActorPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)
	tagPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	// urlUserinfoPattern matches a URL whose authority carries `user:pass@`
	// userinfo — credentials in an artifact ref (C-HANDOFF-2's artifacts row).
	urlUserinfoPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://[^/?#\s]*@`)
)

// HandoffArtifact is one §5.1 `artifacts[]` entry.
type HandoffArtifact struct {
	Kind        string `json:"kind"`
	Ref         string `json:"ref"`
	Description string `json:"description,omitempty"`
}

// HandoffMemoryWrite is one §5.1 `memory_writes[]` entry: a durable assertion the
// author wants the operator's memory to hold, enqueued to the outbox inside the
// handoff transaction (C-HANDOFF-14).
type HandoffMemoryWrite struct {
	Type       string   `json:"type"`
	Content    string   `json:"content"`
	Confidence *float64 `json:"confidence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// ConfidenceOr returns the §5.3 default of 1.0 for an absent confidence
// (C-HANDOFF-6): a write an agent chose to make deliberately is certain unless it
// says otherwise.
func (m HandoffMemoryWrite) ConfidenceOr() float64 {
	if m.Confidence == nil {
		return 1.0
	}
	return *m.Confidence
}

// Handoff is the v1 document of §5.1, rebuilt from the typed columns
// (C-SCHEMA-10).
//
// Checksum is deliberately not serialized: §5.1's document has no checksum
// field, and the checksum is a storage-side drift detector recomputed from the
// typed columns (C-HANDOFF-9). A transport that wants to expose it reads the Go
// field.
type Handoff struct {
	SchemaVersion int                  `json:"schema_version"`
	ID            string               `json:"id"`
	TicketID      string               `json:"ticket_id"`
	BoardID       string               `json:"board_id"`
	FromActor     string               `json:"from_actor"`
	FromActorType string               `json:"from_actor_type"`
	ToActor       *string              `json:"to_actor"`
	Generation    int64                `json:"generation"`
	Status        string               `json:"status"`
	Supersedes    *string              `json:"supersedes"`
	Summary       string               `json:"summary"`
	Done          []string             `json:"done"`
	Next          []string             `json:"next"`
	Blockers      []string             `json:"blockers"`
	Artifacts     []HandoffArtifact    `json:"artifacts"`
	MemoryWrites  []HandoffMemoryWrite `json:"memory_writes"`
	MemoryRefs    []string             `json:"memory_refs"`
	CreatedAt     string               `json:"created_at"`
	AcceptedBy    *string              `json:"accepted_by"`
	AcceptedAt    *string              `json:"accepted_at"`

	// Checksum is the lowercase hex SHA-256 of the canonical form of the
	// creation-time field set (C-HANDOFF-9). It is additive to §5.1's document
	// (C-COMPAT-5): C-HANDOFF-9 promises a client can detect drift in any
	// creation-time field, which it cannot do without the stored value.
	Checksum string `json:"checksum"`
}

// handoffPayload is the decoded creation body: the §5.1 document fields a client
// supplies, plus the two request-level fields the concurrency matrix requires
// (`expected_version` and `generation`).
//
// Server-assigned fields (`id`, `ticket_id`, `board_id`, `from_actor`,
// `from_actor_type`, `status`, `created_at`, `accepted_*`) are ignored when a
// client sends them (C-FENCE-12, §5.2) — and warned about, not silently dropped.
type handoffPayload struct {
	SchemaVersion int                  `json:"schema_version"`
	ToActor       *string              `json:"to_actor"`
	Supersedes    *string              `json:"supersedes"`
	Summary       string               `json:"summary"`
	Done          []string             `json:"done"`
	Next          []string             `json:"next"`
	Blockers      []string             `json:"blockers"`
	Artifacts     []HandoffArtifact    `json:"artifacts"`
	MemoryWrites  []HandoffMemoryWrite `json:"memory_writes"`
	MemoryRefs    []string             `json:"memory_refs"`

	ExpectedVersion *int64 `json:"expected_version"`
	Generation      *int64 `json:"generation"`
}

// handoffPayloadKeys is every field name the creation body understands. Anything
// else is an unknown field (C-HANDOFF-4: warn by default, reject under strict
// mode — C-COMPAT-5).
var handoffPayloadKeys = map[string]bool{
	"schema_version": true, "to_actor": true, "supersedes": true, "summary": true,
	"done": true, "next": true, "blockers": true, "artifacts": true,
	"memory_writes": true, "memory_refs": true,
	"expected_version": true, "generation": true,
}

// CreateHandoffRequest is one handoff creation (§5.5).
//
// Payload is the client's document body verbatim, so REST and MCP get identical
// validation at this boundary (C-HANDOFF-1) and the JSON type rules of §5.2 are
// enforced on the JSON, not on a Go struct that could not have carried the bad
// value in the first place.
type CreateHandoffRequest struct {
	TicketID  string
	Actor     models.Actor
	RequestID string
	Scope     IdempotencyScope
	Payload   json.RawMessage
	// Strict makes unknown fields a 400 unknown_field instead of a warning
	// (C-HANDOFF-4, C-COMPAT-5). The default is warn.
	Strict bool
}

// HandoffResult is the outcome of a handoff mutation: the document, whether it
// was a replay (C-IDEM-6), and the warnings an unknown field produced
// (C-HANDOFF-4).
type HandoffResult struct {
	Document Handoff
	Replayed bool
	Warnings []string
}

// AcceptHandoffRequest is one acceptance of an offered handoff (T7).
//
// Accept needs no lease: an offered handoff addressed to a specific actor is the
// authority to take over, and one addressed to nobody is claimable by whoever
// takes it (C-HANDOFF-17).
type AcceptHandoffRequest struct {
	HandoffID string
	Actor     models.Actor
	RequestID string
	Scope     IdempotencyScope
}

// CreateHandoff validates the §5.1 document and, in one transaction, writes the
// handoff row, its outbox rows, the supersession transition and every audit row
// (C-HANDOFF-14, C-AUDIT-4), together with the idempotency key row (C-IDEM-12).
//
// Creation does not change ownership: the author keeps the lease and its
// generation until it releases or another actor accepts (C-HANDOFF-15).
func (s *SQLiteStore) CreateHandoff(ctx context.Context, req CreateHandoffRequest) (HandoffResult, error) {
	if err := s.requireHandoffActor(req.Actor, "handoff.create"); err != nil {
		return HandoffResult{}, err
	}
	if strings.TrimSpace(req.TicketID) == "" {
		return HandoffResult{}, apperr.WithDetails(apperr.CodeInvalidRequest,
			"ticket_id is required", map[string]any{"field": "ticket_id"})
	}

	payload, warnings, err := decodeHandoffPayload(req.Payload, req.Strict)
	if err != nil {
		return HandoffResult{}, err
	}
	if err := validateHandoffPayload(payload); err != nil {
		return HandoffResult{}, err
	}
	if err := validateHandoffAuthority(req.Actor, payload); err != nil {
		return HandoffResult{}, err
	}

	scope := req.Scope
	scope.Operation = OperationHandoffCreate
	if scope.Actor.String() != req.Actor.String() {
		return HandoffResult{}, apperr.WithDetails(apperr.CodeInternal,
			"idempotency scope actor disagrees with the request actor", map[string]any{"field": "actor"})
	}
	nowMS := s.clock.NowMS()

	var created Handoff
	res, err := s.RunIdempotent(ctx, scope, func(ctx context.Context, tx *sql.Tx) (StoredResponse, error) {
		doc, err := s.createHandoffInTx(ctx, tx, req, payload, nowMS)
		if err != nil {
			return StoredResponse{}, err
		}
		created = doc
		body, err := json.Marshal(doc)
		if err != nil {
			return StoredResponse{}, err
		}
		return StoredResponse{Status: 201, Body: body, EntityID: doc.ID}, nil
	})
	if err != nil {
		return HandoffResult{}, err
	}
	if res.Replayed {
		var replayed Handoff
		if err := json.Unmarshal(res.Response.Body, &replayed); err != nil {
			return HandoffResult{}, apperr.New(apperr.CodeInternal,
				"recorded handoff response could not be decoded (C-SCHEMA-18)")
		}
		return HandoffResult{Document: replayed, Replayed: true, Warnings: warnings}, nil
	}
	return HandoffResult{Document: created, Warnings: warnings}, nil
}

// createHandoffInTx is the whole creation, minus the transaction boundary.
func (s *SQLiteStore) createHandoffInTx(ctx context.Context, tx *sql.Tx, req CreateHandoffRequest,
	payload *handoffPayload, nowMS int64) (Handoff, error) {

	id := newUUID()
	createdAt := models.FormatMS(nowMS)

	doneJSON, err := jsonOrEmptyArray(payload.Done)
	if err != nil {
		return Handoff{}, err
	}
	nextJSON, err := jsonOrEmptyArray(payload.Next)
	if err != nil {
		return Handoff{}, err
	}
	blockersJSON, err := jsonOrEmptyArray(payload.Blockers)
	if err != nil {
		return Handoff{}, err
	}
	artifactsJSON, err := jsonOrEmptyArray(payload.Artifacts)
	if err != nil {
		return Handoff{}, err
	}
	writesJSON, err := jsonOrEmptyArray(payload.MemoryWrites)
	if err != nil {
		return Handoff{}, err
	}
	refsJSON, err := jsonOrEmptyArray(payload.MemoryRefs)
	if err != nil {
		return Handoff{}, err
	}

	// The board id, the generation the author holds and the pre-transition values
	// the audit payloads need are read once, inside the write transaction
	// (C-CONV-4 as amended by A1). They feed the derived columns and the checksum;
	// the conditional INSERT below still decides whether anything is written.
	prior, found, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return Handoff{}, err
	}
	boardID := ""
	var generation int64
	if found {
		boardID = prior.BoardID
		generation = prior.Claim.Generation
	}

	checksum, err := handoffChecksum(handoffChecksumFields{
		SchemaVersion: payload.SchemaVersion,
		ID:            id,
		TicketID:      req.TicketID,
		BoardID:       boardID,
		FromActor:     req.Actor.String(),
		FromActorType: string(req.Actor.Type),
		ToActor:       payload.ToActor,
		Generation:    generation,
		Supersedes:    payload.Supersedes,
		Summary:       payload.Summary,
		Done:          payload.Done,
		Next:          payload.Next,
		Blockers:      payload.Blockers,
		Artifacts:     payload.Artifacts,
		MemoryWrites:  payload.MemoryWrites,
		MemoryRefs:    payload.MemoryRefs,
		CreatedAt:     createdAt,
	})
	if err != nil {
		return Handoff{}, err
	}

	// The guard, the derived board_id and the generation the author held are all
	// decided by this one statement (C-CONV-4, C-INV-5): there is no read-then-write
	// between checking that the author may write and writing.
	query, args := s.handoffInsertStatement(req, payload, id, createdAt, checksum,
		doneJSON, nextJSON, blockersJSON, artifactsJSON, writesJSON, refsJSON, nowMS)
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return Handoff{}, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Handoff{}, err
	}
	if affected == 0 {
		return Handoff{}, s.diagnoseHandoffCreate(ctx, tx, req, payload, nowMS)
	}
	if affected > 1 {
		return Handoff{}, apperr.New(apperr.CodeInternal, "handoff insert wrote more than one row")
	}

	// Armed, this kills the process after the handoff row and before its outbox
	// rows and audit rows: nothing may survive (C-HANDOFF-14, C-IDEM-12).
	crashpoint.Hit(crashpoint.HandoffAfterInsert)

	doc, _, err := s.readHandoffTx(ctx, tx, id)
	if err != nil {
		return Handoff{}, err
	}

	// The outbox rows are written directly here — delivery is a later slice
	// (C-OUTBOX-15: with delivery off the rows are still created and visible).
	writeIDs := make([]string, 0, len(doc.MemoryWrites))
	for _, mw := range doc.MemoryWrites {
		writeID := newUUID()
		tags := mw.Tags
		if tags == nil {
			tags = []string{}
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return Handoff{}, err
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO outbox (id, ticket_id, handoff_id, board_id, actor, actor_type,
                                memory_type, content, confidence, tags_json, status, attempts,
                                next_attempt_at, created_at, updated_at, request_id, idempotency_key)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, NULL, ?, ?, ?, ?)`,
			writeID, doc.TicketID, doc.ID, doc.BoardID, doc.FromActor, doc.FromActorType,
			mw.Type, mw.Content, mw.ConfidenceOr(), string(tagsJSON),
			nowMS, nowMS, req.RequestID, writeID); err != nil {
			return Handoff{}, mapDriverErr(err)
		}
		writeIDs = append(writeIDs, writeID)
	}

	if payload.Supersedes != nil {
		if _, err := tx.ExecContext(ctx, `
            UPDATE handoffs SET status = 'superseded' WHERE id = ? AND status <> 'superseded'`,
			*payload.Supersedes); err != nil {
			return Handoff{}, mapDriverErr(err)
		}
		if err := insertAudit(ctx, tx, doc.TicketID, models.EventHandoffSuperseded, req.Actor, map[string]any{
			"request_id":    req.RequestID,
			"handoff_id":    *payload.Supersedes,
			"superseded_by": doc.ID,
		}, nowMS); err != nil {
			return Handoff{}, err
		}
	}

	if err := insertAudit(ctx, tx, doc.TicketID, models.EventHandoffCreated, req.Actor, map[string]any{
		"request_id":     req.RequestID,
		"handoff_id":     doc.ID,
		"generation":     doc.Generation,
		"schema_version": doc.SchemaVersion,
		"to_actor":       derefString(doc.ToActor),
		"supersedes":     derefString(doc.Supersedes),
	}, nowMS); err != nil {
		return Handoff{}, err
	}

	if len(writeIDs) > 0 {
		if err := insertAudit(ctx, tx, doc.TicketID, models.EventOutboxEnqueued, req.Actor, map[string]any{
			"request_id": req.RequestID,
			"handoff_id": doc.ID,
			"count":      len(writeIDs),
			"write_ids":  writeIDs,
		}, nowMS); err != nil {
			return Handoff{}, err
		}
	}

	// Armed, this kills the process before COMMIT: no handoff, no outbox row, no
	// audit row and no key row may survive (C-HANDOFF-14, C-IDEM-12).
	crashpoint.Hit(crashpoint.HandoffPreCommit)
	return doc, nil
}

// handoffInsertStatement is the conditional INSERT of C-CONV-4: every guard lives
// in the WHERE clause, the derived columns come from the ticket row in the same
// statement, and RowsAffected() is the decision.
//
// Guard arms per the concurrency matrix:
//
//	agent                 holder + generation + live lease + expected_version (C-FENCE-2, C-FENCE-5)
//	orchestrator, holding holder + generation + live lease + expected_version
//	orchestrator, no lease C-HANDOFF-13 / C-AUTH-8 carve-out: expected_version only
//	human                 no lease and no generation; expected_version only if supplied (C-FENCE-6)
func (s *SQLiteStore) handoffInsertStatement(req CreateHandoffRequest, payload *handoffPayload,
	id, createdAt, checksum, doneJSON, nextJSON, blockersJSON, artifactsJSON, writesJSON, refsJSON string,
	nowMS int64) (string, []any) {

	query := `
        INSERT INTO handoffs
               (id, ticket_id, board_id, schema_version, from_actor, from_actor_type, to_actor,
                generation, summary, done_json, next_json, blockers_json, artifacts_json,
                memory_writes_json, memory_refs_json, status, supersedes, created_at,
                request_id, checksum)
        SELECT ?, t.id, t.board_id, ?, ?, ?, ?, t.lease_generation, ?, ?, ?, ?, ?, ?, ?,
               'offered', ?, ?, ?, ?
          FROM tickets t
         WHERE t.id = ?`
	args := []any{
		id, HandoffSchemaVersion, req.Actor.String(), string(req.Actor.Type),
		nullableStringPtr(payload.ToActor), payload.Summary,
		doneJSON, nextJSON, blockersJSON, artifactsJSON, writesJSON, refsJSON,
		nullableStringPtr(payload.Supersedes), nowMS, req.RequestID, checksum,
		req.TicketID,
	}

	if payload.Generation != nil || req.Actor.Type == models.ActorAgent {
		// A claim-protected creation proves holding with both halves of the token
		// (C-FENCE-2). An agent that sent no generation never reaches this
		// statement: validateHandoffActor rejects it with 400 (C-FENCE-5).
		query += ` AND t.lease_holder = ? AND t.lease_generation = ? AND t.lease_expires_at > ?`
		args = append(args, req.Actor.ID, derefInt64Ptr(payload.Generation), nowMS)
	}
	if payload.ExpectedVersion != nil {
		query += ` AND t.version = ?`
		args = append(args, *payload.ExpectedVersion)
	}
	if payload.Supersedes != nil {
		// C-HANDOFF-12 (same ticket) and the cycle guard of C-HANDOFF-11 in the
		// statement itself: a superseded row is already the tail of a chain.
		query += ` AND EXISTS (SELECT 1 FROM handoffs s
                                WHERE s.id = ? AND s.ticket_id = t.id AND s.status <> 'superseded')`
		args = append(args, *payload.Supersedes)
	}
	return query, args
}

// diagnoseHandoffCreate explains a 0-row insert from the row state read under the
// same write lock, in the C-CLAIM-22 order: absence first (404 before 409).
func (s *SQLiteStore) diagnoseHandoffCreate(ctx context.Context, tx *sql.Tx,
	req CreateHandoffRequest, payload *handoffPayload, nowMS int64) error {

	ticket, found, err := readClaimedTicket(ctx, tx, req.TicketID, nowMS)
	if err != nil {
		return err
	}
	if !found {
		return apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("ticket %s not found", req.TicketID),
			map[string]any{"ticket_id": req.TicketID})
	}

	if payload.Supersedes != nil {
		var targetTicket string
		var status string
		err := tx.QueryRowContext(ctx,
			`SELECT ticket_id, status FROM handoffs WHERE id = ?`, *payload.Supersedes).
			Scan(&targetTicket, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return handoffInvalid("supersedes", fmt.Sprintf("handoff %s does not exist", *payload.Supersedes))
		case err != nil:
			return mapDriverErr(err)
		case targetTicket != req.TicketID:
			return handoffInvalid("supersedes", "a handoff may only supersede another handoff on the same ticket (C-HANDOFF-12)")
		case status == HandoffStatusSuperseded:
			return handoffInvalid("supersedes", "that handoff is already superseded; superseding it again would fork the chain (C-HANDOFF-11)")
		}
	}

	if payload.ExpectedVersion != nil && *payload.ExpectedVersion != ticket.Version {
		return apperr.WithDetails(apperr.CodeVersionConflict,
			fmt.Sprintf("ticket %s is at version %d, not %d", req.TicketID, ticket.Version, *payload.ExpectedVersion),
			map[string]any{
				"ticket_id":        req.TicketID,
				"expected_version": *payload.ExpectedVersion,
				"current_version":  ticket.Version,
			})
	}

	if req.Actor.Type == models.ActorAgent || payload.Generation != nil {
		// C-GUARD-3's diagnosis order: a live lease held by someone else is
		// already_claimed; only then is "you are not the holder" the answer.
		holder := ticket.Claim.Holder
		live := holder != nil && ticket.Claim.ExpiresAtMS != nil && *ticket.Claim.ExpiresAtMS > nowMS
		if live && *holder != req.Actor.ID {
			return apperr.WithDetails(apperr.CodeAlreadyClaimed,
				fmt.Sprintf("ticket %s is held by %s", req.TicketID, *holder),
				conflictDetails(ticket, req.TicketID, "held"))
		}
		if holder == nil || *holder != req.Actor.ID {
			return apperr.WithDetails(apperr.CodeNotClaimHolder,
				fmt.Sprintf("ticket %s is not held by %s", req.TicketID, req.Actor.String()),
				map[string]any{
					"ticket_id":   req.TicketID,
					"holder":      derefString(ticket.Claim.Holder),
					"holder_type": derefActorType(ticket.Claim.HolderType),
				})
		}
		if payload.Generation == nil || *payload.Generation != ticket.Claim.Generation {
			return apperr.WithDetails(apperr.CodeLeaseGenerationMismatch,
				fmt.Sprintf("generation %s is not the current generation of ticket %s",
					formatGenerationPtr(payload.Generation), req.TicketID),
				map[string]any{
					"ticket_id":           req.TicketID,
					"expected_generation": derefInt64Ptr(payload.Generation),
					"current_generation":  ticket.Claim.Generation,
				})
		}
		if !live {
			return apperr.WithDetails(apperr.CodeLeaseExpired,
				fmt.Sprintf("the lease on ticket %s has lapsed", req.TicketID),
				map[string]any{
					"ticket_id":        req.TicketID,
					"lease_expires_at": derefMS(ticket.Claim.ExpiresAtMS),
					"lease_generation": ticket.Claim.Generation,
				})
		}
	}

	return apperr.New(apperr.CodeInternal,
		"handoff create matched no rows and the diagnosis found no cause (C-CLAIM-23)")
}

// AcceptHandoff performs T7: the acceptance of an offered handoff, which is the
// ownership transfer (C-HANDOFF-16).
//
// One transaction: the handoff's `offered` -> `accepted` transition (guarded by
// `to_actor IS NULL OR to_actor = :acceptor`), the ticket's ownership transfer
// with a fresh generation, the `handoff_accepted` audit row and the idempotency
// key row. Accepting needs no lease (C-HANDOFF-17): the handoff is the authority.
func (s *SQLiteStore) AcceptHandoff(ctx context.Context, req AcceptHandoffRequest) (HandoffResult, error) {
	if err := s.requireHandoffActor(req.Actor, "handoff.accept"); err != nil {
		return HandoffResult{}, err
	}
	if strings.TrimSpace(req.HandoffID) == "" {
		return HandoffResult{}, apperr.WithDetails(apperr.CodeInvalidRequest,
			"handoff_id is required", map[string]any{"field": "handoff_id"})
	}

	scope := req.Scope
	scope.Operation = OperationHandoffAccept
	if scope.Actor.String() != req.Actor.String() {
		return HandoffResult{}, apperr.WithDetails(apperr.CodeInternal,
			"idempotency scope actor disagrees with the request actor", map[string]any{"field": "actor"})
	}
	nowMS := s.clock.NowMS()

	var accepted Handoff
	res, err := s.RunIdempotent(ctx, scope, func(ctx context.Context, tx *sql.Tx) (StoredResponse, error) {
		doc, err := s.acceptHandoffInTx(ctx, tx, req, nowMS)
		if err != nil {
			return StoredResponse{}, err
		}
		accepted = doc
		body, err := json.Marshal(doc)
		if err != nil {
			return StoredResponse{}, err
		}
		return StoredResponse{Status: 200, Body: body, EntityID: doc.ID}, nil
	})
	if err != nil {
		return HandoffResult{}, err
	}
	if res.Replayed {
		var replayed Handoff
		if err := json.Unmarshal(res.Response.Body, &replayed); err != nil {
			return HandoffResult{}, apperr.New(apperr.CodeInternal,
				"recorded handoff response could not be decoded (C-SCHEMA-18)")
		}
		return HandoffResult{Document: replayed, Replayed: true}, nil
	}
	return HandoffResult{Document: accepted}, nil
}

// acceptHandoffInTx is the acceptance, minus the transaction boundary.
func (s *SQLiteStore) acceptHandoffInTx(ctx context.Context, tx *sql.Tx, req AcceptHandoffRequest, nowMS int64) (Handoff, error) {
	// The pre-transition values the audit payload needs, read once inside the
	// write transaction (C-CONV-4 as amended by A1). They decide nothing: the two
	// conditional UPDATEs below are the decision.
	prior, found, err := s.readHandoffTx(ctx, tx, req.HandoffID)
	if err != nil {
		return Handoff{}, err
	}
	if !found {
		return Handoff{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("handoff %s not found", req.HandoffID),
			map[string]any{"handoff_id": req.HandoffID})
	}
	priorTicket, ticketFound, err := readClaimedTicket(ctx, tx, prior.TicketID, nowMS)
	if err != nil {
		return Handoff{}, err
	}
	if !ticketFound {
		return Handoff{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("ticket %s not found", prior.TicketID),
			map[string]any{"ticket_id": prior.TicketID})
	}

	// C-HANDOFF-16's guard, verbatim: `status = 'offered' AND (to_actor IS NULL OR
	// to_actor = :acceptor)`. 0 rows is handoff_not_acceptable (C-HANDOFF-17).
	res, err := tx.ExecContext(ctx, `
        UPDATE handoffs
           SET status = 'accepted', accepted_by = ?, accepted_at = ?
         WHERE id = ?
           AND status = 'offered'
           AND (to_actor IS NULL OR to_actor = ?)`,
		req.Actor.String(), nowMS, req.HandoffID, req.Actor.String())
	if err != nil {
		return Handoff{}, mapDriverErr(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Handoff{}, err
	}
	if affected == 0 {
		return Handoff{}, apperr.WithDetails(apperr.CodeHandoffNotAcceptable,
			fmt.Sprintf("handoff %s is not offered to %s", req.HandoffID, req.Actor.String()),
			map[string]any{
				"handoff_id": req.HandoffID,
				"status":     prior.Status,
				"to_actor":   derefString(prior.ToActor),
			})
	}

	// The ownership transfer of C-HANDOFF-16, in one statement: every lease field,
	// the generation bump, the version bump, the status and handoff_id.
	var generation, version int64
	err = tx.QueryRowContext(ctx, `
        UPDATE tickets
           SET lease_holder = ?, lease_holder_type = ?,
               lease_acquired_at = ?, lease_expires_at = ?,
               lease_generation = lease_generation + 1, lease_renewals = 0,
               status = 'in_progress', version = version + 1,
               handoff_id = ?, updated_at = ?
         WHERE id = ?
        RETURNING lease_generation, version`,
		req.Actor.ID, string(req.Actor.Type), nowMS, nowMS+s.ttlMS,
		req.HandoffID, timeText(nowMS), prior.TicketID).
		Scan(&generation, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, apperr.New(apperr.CodeInternal,
			"handoff accept could not update its ticket (C-CLAIM-23)")
	}
	if err != nil {
		return Handoff{}, mapDriverErr(err)
	}

	if err := insertAudit(ctx, tx, prior.TicketID, models.EventHandoffAccepted, req.Actor, map[string]any{
		"request_id":          req.RequestID,
		"handoff_id":          req.HandoffID,
		"generation":          generation,
		"previous_generation": priorTicket.Claim.Generation,
		"from_holder":         renderHolder(priorTicket.Claim.Holder, priorTicket.Claim.HolderType),
		"to_holder":           req.Actor.String(),
	}, nowMS); err != nil {
		return Handoff{}, err
	}

	doc, _, err := s.readHandoffTx(ctx, tx, req.HandoffID)
	if err != nil {
		return Handoff{}, err
	}
	return doc, nil
}

// GetHandoff reads one handoff document by id (the "by id" retrieval shape of
// §5.6).
func (s *SQLiteStore) GetHandoff(ctx context.Context, id string) (Handoff, error) {
	doc, found, err := s.readHandoffTx(ctx, s.readDB, id)
	if err != nil {
		return Handoff{}, err
	}
	if !found {
		return Handoff{}, apperr.WithDetails(apperr.CodeNotFound,
			fmt.Sprintf("handoff %s not found", id), map[string]any{"handoff_id": id})
	}
	return doc, nil
}

// rowQuerier is the subset of *sql.Tx and *sql.DB the handoff reader needs.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readHandoffTx rebuilds the §5.1 document from the typed columns (C-SCHEMA-10)
// and recomputes the checksum from them, never from a stored copy (C-HANDOFF-9).
func (s *SQLiteStore) readHandoffTx(ctx context.Context, q rowQuerier, id string) (Handoff, bool, error) {
	var (
		doc                                 Handoff
		toActor, supersedes, acceptedBy     sql.NullString
		acceptedAt                          sql.NullInt64
		createdAtMS                         int64
		doneJSON, nextJSON, blockersJSON    string
		artifactsJSON, writesJSON, refsJSON string
	)
	err := q.QueryRowContext(ctx, `
        SELECT id, ticket_id, board_id, schema_version, from_actor, from_actor_type,
               to_actor, generation, summary, done_json, next_json, blockers_json,
               artifacts_json, memory_writes_json, memory_refs_json, status, supersedes,
               accepted_by, accepted_at, created_at, checksum
          FROM handoffs WHERE id = ?`, id).
		Scan(&doc.ID, &doc.TicketID, &doc.BoardID, &doc.SchemaVersion, &doc.FromActor,
			&doc.FromActorType, &toActor, &doc.Generation, &doc.Summary,
			&doneJSON, &nextJSON, &blockersJSON, &artifactsJSON, &writesJSON, &refsJSON,
			&doc.Status, &supersedes, &acceptedBy, &acceptedAt, &createdAtMS, &doc.Checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}

	if toActor.Valid {
		v := toActor.String
		doc.ToActor = &v
	}
	if supersedes.Valid {
		v := supersedes.String
		doc.Supersedes = &v
	}
	if acceptedBy.Valid {
		v := acceptedBy.String
		doc.AcceptedBy = &v
	}
	if acceptedAt.Valid {
		v := models.FormatMS(acceptedAt.Int64)
		doc.AcceptedAt = &v
	}
	doc.CreatedAt = models.FormatMS(createdAtMS)
	if doc.Done, err = decodeJSONArray[[]string](doneJSON); err != nil {
		return Handoff{}, false, err
	}
	if doc.Next, err = decodeJSONArray[[]string](nextJSON); err != nil {
		return Handoff{}, false, err
	}
	if doc.Blockers, err = decodeJSONArray[[]string](blockersJSON); err != nil {
		return Handoff{}, false, err
	}
	if doc.Artifacts, err = decodeJSONArray[[]HandoffArtifact](artifactsJSON); err != nil {
		return Handoff{}, false, err
	}
	if doc.MemoryWrites, err = decodeJSONArray[[]HandoffMemoryWrite](writesJSON); err != nil {
		return Handoff{}, false, err
	}
	if doc.MemoryRefs, err = decodeJSONArray[[]string](refsJSON); err != nil {
		return Handoff{}, false, err
	}
	return doc, true, nil
}

// ---- validation (§5.2, C-HANDOFF-1..7) -------------------------------------

// decodeHandoffPayload decodes the creation body at the service boundary, so
// REST, MCP and any future CLI get identical validation (C-HANDOFF-1).
//
// The JSON is type-checked before it reaches the typed struct, because a Go
// struct cannot express "confidence was the string \"0.9\"": decoding straight
// into *float64 would fail with a generic message instead of the
// `details.field` §5.2 requires.
func decodeHandoffPayload(raw json.RawMessage, strict bool) (*handoffPayload, []string, error) {
	if len(raw) == 0 {
		return nil, nil, handoffInvalid("document", "the handoff document is required")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, handoffInvalid("document", "the handoff document must be a JSON object")
	}

	var unknown []string
	for key := range fields {
		if !handoffPayloadKeys[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if strict && len(unknown) > 0 {
		return nil, nil, apperr.WithDetails(apperr.CodeUnknownField,
			fmt.Sprintf("unknown field %q", unknown[0]),
			map[string]any{"fields": unknown})
	}
	var warnings []string
	for _, key := range unknown {
		warnings = append(warnings, fmt.Sprintf("unknown field %q ignored", key))
	}

	if err := checkJSONTypes(fields); err != nil {
		return nil, nil, err
	}

	var payload handoffPayload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&payload); err != nil {
		return nil, nil, handoffInvalid("document", err.Error())
	}
	return &payload, warnings, nil
}

// checkJSONTypes enforces the §5.2 *types*, which a typed decode would otherwise
// silently accept or reject without the field name.
func checkJSONTypes(fields map[string]json.RawMessage) error {
	if raw, ok := fields["memory_writes"]; ok {
		var writes []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &writes); err != nil {
			return handoffInvalid("memory_writes", "must be an array of objects")
		}
		for i, w := range writes {
			if conf, ok := w["confidence"]; ok {
				if err := checkJSONNumber(conf); err != nil {
					// C-HANDOFF-6: a string, a bool or null where a number belongs
					// is a handoff_invalid naming the exact field.
					return handoffInvalid(fmt.Sprintf("memory_writes[%d].confidence", i),
						"must be a JSON number in [0.0, 1.0]")
				}
			}
		}
	}
	if raw, ok := fields["schema_version"]; ok {
		if err := checkJSONNumber(raw); err != nil {
			return handoffInvalid("schema_version", "must be the integer 1")
		}
	}
	return nil
}

// checkJSONNumber reports whether raw is a JSON number. A quoted number ("0.9"),
// a bool and null are all rejected: C-HANDOFF-6 requires a JSON number, and
// encoding/json would otherwise accept a quoted numeric string for json.Number.
func checkJSONNumber(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return fmt.Errorf("empty")
	}
	switch trimmed[0] {
	case '{', '[', '"', 't', 'f', 'n':
		return fmt.Errorf("not a JSON number")
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err != nil {
		return err
	}
	return nil
}

// validateHandoffPayload is the §5.2 field table, one rule per row. Every failure
// is `400 handoff_invalid` with `details.field` (C-HANDOFF-1).
func validateHandoffPayload(p *handoffPayload) error {
	if p.SchemaVersion != HandoffSchemaVersion {
		return handoffInvalid("schema_version",
			fmt.Sprintf("must be exactly %d, got %d", HandoffSchemaVersion, p.SchemaVersion))
	}

	if p.ToActor != nil {
		to := strings.TrimSpace(*p.ToActor)
		if to == "" {
			p.ToActor = nil
		} else {
			if !toActorPattern.MatchString(to) {
				return handoffInvalid("to_actor", "must be 1-200 characters from [A-Za-z0-9._:-]")
			}
			p.ToActor = &to
		}
	}

	if p.Supersedes != nil {
		sup := strings.TrimSpace(*p.Supersedes)
		if sup == "" {
			p.Supersedes = nil
		} else {
			p.Supersedes = &sup
		}
	}

	p.Summary = strings.TrimSpace(p.Summary)
	if p.Summary == "" {
		return handoffInvalid("summary", "must be 1-4000 characters and not blank")
	}
	if len([]rune(p.Summary)) > 4000 {
		return handoffInvalid("summary", "must be at most 4000 characters")
	}

	var err error
	if p.Done, err = normalizeStringList(p.Done, 50, "done"); err != nil {
		return err
	}
	if p.Next, err = normalizeStringList(p.Next, 50, "next"); err != nil {
		return err
	}
	if p.Blockers, err = normalizeStringList(p.Blockers, 20, "blockers"); err != nil {
		return err
	}
	if p.MemoryRefs, err = normalizeStringList(p.MemoryRefs, 50, "memory_refs"); err != nil {
		return err
	}
	for i, ref := range p.MemoryRefs {
		if len(ref) > 200 {
			return handoffInvalid(fmt.Sprintf("memory_refs[%d]", i), "must be at most 200 characters")
		}
	}

	if len(p.Artifacts) > 50 {
		return handoffInvalid("artifacts", "must contain at most 50 items")
	}
	for i := range p.Artifacts {
		if err := validateArtifact(i, &p.Artifacts[i]); err != nil {
			return err
		}
	}

	if len(p.MemoryWrites) > 50 {
		return handoffInvalid("memory_writes", "must contain at most 50 items")
	}
	for i := range p.MemoryWrites {
		if err := validateMemoryWrite(i, &p.MemoryWrites[i]); err != nil {
			return err
		}
	}

	// C-HANDOFF-2's backstop: the canonical JSON of the document, after the
	// per-field rules above, must fit in 64 KiB.
	canonical, err := json.Marshal(p)
	if err != nil {
		return handoffInvalid("document", err.Error())
	}
	if len(canonical) > HandoffMaxCanonicalBytes {
		return handoffInvalid("document",
			fmt.Sprintf("the canonical document is %d bytes; the limit is %d", len(canonical), HandoffMaxCanonicalBytes))
	}
	return nil
}

// normalizeStringList applies the shared array rules of §5.2: a cap on items,
// trimming, and dropping empty strings rather than rejecting them.
func normalizeStringList(items []string, max int, field string) ([]string, error) {
	if len(items) > max {
		return nil, handoffInvalid(field, fmt.Sprintf("must contain at most %d items", max))
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		if len([]rune(s)) > 500 {
			return nil, handoffInvalid(field, "each item must be at most 500 characters")
		}
		out = append(out, s)
	}
	return out, nil
}

func validateArtifact(i int, a *HandoffArtifact) error {
	field := fmt.Sprintf("artifacts[%d]", i)
	if !artifactKinds[a.Kind] {
		return handoffInvalid(field+".kind", "must be one of commit, pr, file, url, log, other")
	}
	a.Ref = strings.TrimSpace(a.Ref)
	if a.Ref == "" {
		return handoffInvalid(field+".ref", "must be 1-1000 characters and not blank")
	}
	if len([]rune(a.Ref)) > 1000 {
		return handoffInvalid(field+".ref", "must be at most 1000 characters")
	}
	if urlUserinfoPattern.MatchString(a.Ref) {
		return handoffInvalid(field+".ref", "must not carry credentials (user:pass@) in a URL")
	}
	if len([]rune(a.Description)) > 500 {
		return handoffInvalid(field+".description", "must be at most 500 characters")
	}
	return nil
}

func validateMemoryWrite(i int, mw *HandoffMemoryWrite) error {
	field := fmt.Sprintf("memory_writes[%d]", i)
	if !memoryTypes[mw.Type] {
		return apperr.WithDetails(apperr.CodeHandoffInvalid,
			fmt.Sprintf("%s.type %q is not a memory type", field, mw.Type),
			map[string]any{
				"field":   field + ".type",
				"reason":  "unknown memory type",
				"allowed": MemoryTypes(),
			})
	}
	mw.Content = strings.TrimSpace(mw.Content)
	if mw.Content == "" {
		return handoffInvalid(field+".content", "must be 1-2000 characters and not blank")
	}
	if len([]rune(mw.Content)) > 2000 {
		return handoffInvalid(field+".content", "must be at most 2000 characters")
	}
	if mw.Confidence != nil {
		c := *mw.Confidence
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > 1 {
			return handoffInvalid(field+".confidence", "must be a number in [0.0, 1.0]")
		}
	}
	if len(mw.Tags) > 16 {
		return handoffInvalid(field+".tags", "must contain at most 16 tags")
	}
	seen := make(map[string]bool, len(mw.Tags))
	tags := make([]string, 0, len(mw.Tags))
	for _, tag := range mw.Tags {
		t := strings.TrimSpace(tag)
		if !tagPattern.MatchString(t) {
			return handoffInvalid(field+".tags", "each tag must be 1-64 characters from [A-Za-z0-9._:-]")
		}
		if seen[t] {
			continue // duplicates collapse, first-seen order preserved (C-HANDOFF-7)
		}
		seen[t] = true
		tags = append(tags, t)
	}
	mw.Tags = tags
	return nil
}

// requireHandoffActor enforces §8.2's capability cells at the service boundary:
// `system` may never create or accept a handoff (C-AUTH-6), and an agent must
// present its generation (C-FENCE-5: missing is 400, not 409).
func (s *SQLiteStore) requireHandoffActor(actor models.Actor, operation string) error {
	if err := actor.Validate(); err != nil {
		return invalidActor(err)
	}
	if actor.Type == models.ActorSystem {
		return apperr.WithDetails(apperr.CodeForbidden,
			"system may not create or accept handoffs",
			map[string]any{"actor_type": string(actor.Type), "operation": operation})
	}
	return nil
}

// validateHandoffAuthority enforces the *request-level* requirements of the
// concurrency matrix (`docs/concurrency.md` §2, normative): an agent creating a
// handoff must present the generation it holds (C-FENCE-2) and its
// expected_version, and a missing one is 400 invalid_request naming the field —
// a malformed request, not a losing race (C-FENCE-5).
//
// An orchestrator's and a human's expected_version is optional and enforced when
// supplied: concurrency.md requires it "for agent", and C-HANDOFF-13/C-AUTH-8
// carve the orchestrator out of the lease requirement, not out of version
// hygiene, so it is accepted either way and never silently ignored.
func validateHandoffAuthority(actor models.Actor, p *handoffPayload) error {
	if actor.Type == models.ActorAgent {
		if p.Generation == nil {
			return apperr.WithDetails(apperr.CodeInvalidRequest,
				"an agent creating a handoff must present the generation it holds (C-FENCE-5)",
				map[string]any{"field": "generation"})
		}
		if p.ExpectedVersion == nil {
			return apperr.WithDetails(apperr.CodeInvalidRequest,
				"an agent creating a handoff must present expected_version (C-GUARD-4)",
				map[string]any{"field": "expected_version"})
		}
	}
	return nil
}

// handoffInvalid builds the `400 handoff_invalid` of §7.4 with its required
// details: the field and a short reason.
func handoffInvalid(field, reason string) error {
	return apperr.WithDetails(apperr.CodeHandoffInvalid,
		fmt.Sprintf("handoff_invalid: %s %s", field, reason),
		map[string]any{"field": field, "reason": reason})
}

// ---- checksum (C-HANDOFF-9) ------------------------------------------------

// handoffChecksumFields is the closed set of seventeen creation-time fields. It
// is stated by mutability, not authorship: everything fixed at creation is
// inside, and the three fields the two permitted transitions write (status,
// accepted_by, accepted_at) are outside — a checksum that moved on a legitimate
// accept would report drift on every accept.
type handoffChecksumFields struct {
	SchemaVersion int
	ID            string
	TicketID      string
	BoardID       string
	FromActor     string
	FromActorType string
	ToActor       *string
	Generation    int64
	Supersedes    *string
	Summary       string
	Done          []string
	Next          []string
	Blockers      []string
	Artifacts     []HandoffArtifact
	MemoryWrites  []HandoffMemoryWrite
	MemoryRefs    []string
	CreatedAt     string
}

// handoffChecksum returns the lowercase hex SHA-256 of the canonical form of the
// seventeen creation-time fields (C-HANDOFF-9): object keys sorted by byte value,
// no insignificant whitespace, arrays in submitted order, null-valued optional
// fields omitted, created_at with three fractional digits.
//
// encoding/json sorts map keys by byte value and emits no insignificant
// whitespace, so marshalling a map with the nulls dropped *is* the canonical form.
func handoffChecksum(f handoffChecksumFields) (string, error) {
	doc := map[string]any{
		"schema_version":  f.SchemaVersion,
		"id":              f.ID,
		"ticket_id":       f.TicketID,
		"board_id":        f.BoardID,
		"from_actor":      f.FromActor,
		"from_actor_type": f.FromActorType,
		"generation":      f.Generation,
		"summary":         f.Summary,
		"done":            orEmpty(f.Done),
		"next":            orEmpty(f.Next),
		"blockers":        orEmpty(f.Blockers),
		"artifacts":       orEmptyArtifacts(f.Artifacts),
		"memory_writes":   orEmptyWrites(f.MemoryWrites),
		"memory_refs":     orEmpty(f.MemoryRefs),
		"created_at":      f.CreatedAt,
	}
	if f.ToActor != nil {
		doc["to_actor"] = *f.ToActor
	}
	if f.Supersedes != nil {
		doc["supersedes"] = *f.Supersedes
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("canonical handoff JSON: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyChecksum recomputes the checksum from the typed columns and reports
// whether it still matches the stored value (C-SCHEMA-10, C-HANDOFF-9). Drift in
// any creation-time field is detectable at any point in the handoff's life.
func (h Handoff) VerifyChecksum() (bool, error) {
	expected, err := handoffChecksum(handoffChecksumFields{
		SchemaVersion: h.SchemaVersion,
		ID:            h.ID,
		TicketID:      h.TicketID,
		BoardID:       h.BoardID,
		FromActor:     h.FromActor,
		FromActorType: h.FromActorType,
		ToActor:       h.ToActor,
		Generation:    h.Generation,
		Supersedes:    h.Supersedes,
		Summary:       h.Summary,
		Done:          h.Done,
		Next:          h.Next,
		Blockers:      h.Blockers,
		Artifacts:     h.Artifacts,
		MemoryWrites:  h.MemoryWrites,
		MemoryRefs:    h.MemoryRefs,
		CreatedAt:     h.CreatedAt,
	})
	if err != nil {
		return false, err
	}
	return expected == h.Checksum, nil
}

// ---- small helpers ---------------------------------------------------------

func jsonOrEmptyArray[T any](v []T) (string, error) {
	if v == nil {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeJSONArray[T any](raw string) (T, error) {
	var out T
	if raw == "" {
		raw = "[]"
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, fmt.Errorf("decode stored handoff array: %w", err)
	}
	return out, nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyArtifacts(v []HandoffArtifact) []HandoffArtifact {
	if v == nil {
		return []HandoffArtifact{}
	}
	return v
}

func orEmptyWrites(v []HandoffMemoryWrite) []HandoffMemoryWrite {
	if v == nil {
		return []HandoffMemoryWrite{}
	}
	return v
}

// renderHolder renders a lease holder as `type:id` (C-AUTH-2), which is the form
// every audit payload uses. A holder whose type is unknown falls back to the bare
// id rather than inventing a type.
func renderHolder(holder *string, holderType *models.ActorType) any {
	if holder == nil {
		return nil
	}
	if holderType == nil {
		return *holder
	}
	return string(*holderType) + ":" + *holder
}

func nullableStringPtr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefInt64Ptr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func formatGenerationPtr(v *int64) string {
	if v == nil {
		return "(none)"
	}
	return fmt.Sprintf("%d", *v)
}
