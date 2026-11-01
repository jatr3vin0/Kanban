// Command harness-agent is the test-only agent process of docs/test-plan.md
// §1.1 (build requirement H4), specialised for Phase 2's store-level
// multi-process rows: the claim/handoff/outbox domain is exposed at the store
// service boundary in Phase 2, so this agent speaks the store API — every
// invocation is still a real OS process with its own database connection on the
// shared file, which is the property the MP rows exist to prove (C-INV-1,
// C-OUTBOX-5, C-OUTBOX-17 cannot be reproduced by goroutines sharing a handle).
//
// The HTTP variant of the same op table lands with the Phase 3 transport slice;
// the JSON result contract below is deliberately identical to the one §1.1
// specifies so those rows only swap the transport.
//
// One op per process, selected by LEDGERBOARD_TEST_OP. The result is one JSON
// line on stdout (and, when LEDGERBOARD_TEST_RESULT is set, in that file):
// contracted business failures are reported in the result with ok=false and are
// NOT process failures; only infrastructure errors exit non-zero (code 3).
//
// Seams: the fake clock and crashpoints are armed exactly the way a server
// binary would be — LEDGERBOARD_CLOCK / LEDGERBOARD_CLOCK_FILE and
// LEDGERBOARD_CRASH_POINTS / LEDGERBOARD_CRASH_FIFO (C-CONV-11). Nothing here
// can arm them from a request path.
//
// This binary is test support: it is never imported by product code and never
// shipped (WORKFLOW.md; test-plan §1.1 "never shipped").
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/clocksource"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/db"
	"github.com/rgracey/kanban-mcp/internal/memory"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/rgracey/kanban-mcp/internal/store"
)

// Environment contract between the parent test and this process.
const (
	envRole      = "LEDGERBOARD_TEST_AGENT" // must be "1" or the child refuses
	envOp        = "LEDGERBOARD_TEST_OP"
	envDB        = "LEDGERBOARD_TEST_DB"
	envResult    = "LEDGERBOARD_TEST_RESULT"
	envActor     = "LEDGERBOARD_TEST_ACTOR" // "type:id"
	envTicket    = "LEDGERBOARD_TEST_TICKET"
	envBoard     = "LEDGERBOARD_TEST_BOARD"
	envKey       = "LEDGERBOARD_TEST_KEY"
	envGen       = "LEDGERBOARD_TEST_GEN"
	envVersion   = "LEDGERBOARD_TEST_VERSION"
	envWrites    = "LEDGERBOARD_TEST_WRITES"
	envWorkerID  = "LEDGERBOARD_TEST_WORKER_ID"
	envCalls     = "LEDGERBOARD_TEST_CALLS" // mock adapter calls.jsonl
	envGoFIFO    = "LEDGERBOARD_TEST_GO_FIFO"
	envReadyFIFO = "LEDGERBOARD_TEST_READY_FIFO"
)

// result is the JSON line every op reports. Field names match the §1.1 op-table
// contract (status/error_code/details/values the parent asserts on).
type result struct {
	Op          string         `json:"op"`
	OK          bool           `json:"ok"`
	ErrCode     string         `json:"error_code,omitempty"`
	ErrMessage  string         `json:"err_message,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
	TicketID    string         `json:"ticket_id,omitempty"`
	Holder      string         `json:"holder,omitempty"`
	Generation  int64          `json:"generation,omitempty"`
	Version     int64          `json:"version,omitempty"`
	ExpiresAtMS int64          `json:"lease_expires_at_ms,omitempty"`
	Replayed    bool           `json:"replayed,omitempty"`
	HandoffID   string         `json:"handoff_id,omitempty"`
	RowID       string         `json:"row_id,omitempty"`
	RowStatus   string         `json:"row_status,omitempty"`
	Attempts    int64          `json:"attempts,omitempty"`
	ExternalID  string         `json:"external_id,omitempty"`
	Claimed     bool           `json:"claimed,omitempty"`
	Reclaimed   int64          `json:"reclaimed,omitempty"`
	Delivered   int64          `json:"delivered,omitempty"`
	ElapsedMS   int64          `json:"elapsed_ms,omitempty"`
	OutboxID    string         `json:"outbox_id,omitempty"`
	OutboxIDs   []string       `json:"outbox_ids,omitempty"`
}

func actorFromEnv(name string) models.Actor {
	parts := strings.SplitN(name, ":", 2)
	t := models.ActorAgent
	id := name
	if len(parts) == 2 {
		switch parts[0] {
		case "human":
			t = models.ActorHuman
		case "system":
			t = models.ActorSystem
		case "orchestrator":
			t = models.ActorOrchestrator
		}
		id = parts[1]
	}
	return models.Actor{Type: t, ID: id}
}

func getenv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func requireEnv(name string) (string, error) {
	v := getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func main() {
	if err := run(); err != nil {
		// Infrastructure failure: the parent must treat this as a broken
		// harness, never as a contracted outcome.
		fmt.Fprintf(os.Stderr, "harness-agent: %v\n", err)
		os.Exit(3)
	}
}

// waitGo blocks on the per-child go FIFO (test-plan §1.5): synchronisation is a
// message on a FIFO, never a sleep. The parent opens the FIFO for writing, so
// this open returns the moment the child's writer side appears.
func waitGo(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 64)
	_, _ = f.Read(buf)
	return nil
}

// signalReady announces readiness on the per-child ready FIFO: the parent's
// blocking read of it is the readiness message of §1.1(1).
func signalReady(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("ready\n")
	f.Close()
	return err
}

func run() error {
	if getenv(envRole) != "1" {
		return fmt.Errorf("%s must be 1; refusing to act as a stray process", envRole)
	}
	op := getenv(envOp)
	dbPath, err := requireEnv(envDB)
	if err != nil {
		return err
	}

	// The same seams a server binary arms, armed the same way.
	crashpoint.Init()
	clock, err := clocksource.FromEnv()
	if err != nil {
		return err
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	s := store.NewSQLiteStore(conn, store.WithClock(clock))

	// Readiness first, then the go barrier, then the op: the collision is
	// guaranteed by every process already being inside its op before the
	// first one can complete (§1.1 barrier), and by SQLite's own write lock.
	if getenv(envReadyFIFO) != "" {
		if err := signalReady(getenv(envReadyFIFO)); err != nil {
			return err
		}
	}
	if getenv(envGoFIFO) != "" {
		if err := waitGo(getenv(envGoFIFO)); err != nil {
			return err
		}
	}

	start := time.Now()
	res := result{Op: op}
	switch op {
	case "noop":
		res.OK = true
	case "claim":
		res, err = opClaim(s, res)
	case "claim_idem":
		res, err = opClaimIdem(s, res)
	case "renew":
		res, err = opRenew(s, res)
	case "release":
		res, err = opRelease(s, res)
	case "handoff_create":
		res, err = opHandoffCreate(s, res)
	case "handoff_create_signal":
		// T-HAND-09: create, publish the result, then block forever. The
		// kill is issued by the parent (a separate process) after it reads
		// the signal — the A9 construction, not a self-kill.
		res, err = opHandoffCreate(s, res)
		if err != nil || !res.OK {
			break
		}
		_ = writeResult(res)
		select {}
	case "outbox_once":
		res, err = opOutboxOnce(s, res)
	case "outbox_reclaim":
		n, rerr := s.ReclaimExpiredOutboxRows(context.Background())
		res.Reclaimed = n
		res.OK = rerr == nil
		err = rerr
	case "outbox_claim":
		row, found, rerr := s.ClaimOutboxRow(context.Background(), workerID())
		if rerr != nil {
			err = rerr
			break
		}
		res.Claimed = found
		res.OutboxID = row.ID
		res.Attempts = row.Attempts
		res.OK = true
	case "outbox_loop":
		res, err = opOutboxLoop(s, res)
	default:
		return fmt.Errorf("unknown op %q", op)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()

	if err != nil {
		// A business error is a result, not a harness failure.
		if ae, ok := apperr.From(err); ok {
			res.OK = false
			res.ErrCode = string(ae.Code)
			res.ErrMessage = ae.Message
			res.Details = ae.Details
			return writeResult(res)
		}
		return fmt.Errorf("op %q: non-service error: %w", op, err)
	}
	return writeResult(res)
}

func writeResult(res result) error {
	line, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fmt.Println(string(line))
	if p := getenv(envResult); p != "" {
		return os.WriteFile(p, append(line, '\n'), 0o600)
	}
	return nil
}

func requireTicket() (string, error) { return requireEnv(envTicket) }

func actor() (models.Actor, error) {
	name, err := requireEnv(envActor)
	if err != nil {
		return models.Actor{}, err
	}
	return actorFromEnv(name), nil
}

func workerID() string {
	if v := getenv(envWorkerID); v != "" {
		return v
	}
	return "harness-worker"
}

func opClaim(s *store.SQLiteStore, res result) (result, error) {
	tk, err := requireTicket()
	if err != nil {
		return res, err
	}
	a, err := actor()
	if err != nil {
		return res, err
	}
	res.TicketID = tk
	ct, err := s.Claim(context.Background(), store.ClaimRequest{
		TicketID: tk, Actor: a, RequestID: "req-" + opName("claim"),
	})
	if err != nil {
		return res, err
	}
	fillClaimed(&res, ct, a)
	return res, nil
}

func opClaimIdem(s *store.SQLiteStore, res result) (result, error) {
	tk, err := requireTicket()
	if err != nil {
		return res, err
	}
	a, err := actor()
	if err != nil {
		return res, err
	}
	key, err := requireEnv(envKey)
	if err != nil {
		return res, err
	}
	hash, err := store.RequestHash(map[string]string{"ticket_id": tk})
	if err != nil {
		return res, err
	}
	res.TicketID = tk
	ct, replayed, err := s.ClaimIdempotent(context.Background(), store.ClaimRequest{
		TicketID: tk, Actor: a, RequestID: "req-claim-idem",
	}, store.IdempotencyScope{Actor: a, Operation: store.OperationTicketClaim, Key: key, RequestHash: hash})
	if err != nil {
		return res, err
	}
	res.Replayed = replayed
	fillClaimed(&res, ct, a)
	return res, nil
}

func fillClaimed(res *result, ct store.ClaimedTicket, a models.Actor) {
	res.OK = true
	res.Generation = ct.Claim.Generation
	res.Version = ct.Version
	if ct.Claim.Holder != nil {
		res.Holder = *ct.Claim.Holder
	}
	if ct.Claim.ExpiresAtMS != nil {
		res.ExpiresAtMS = *ct.Claim.ExpiresAtMS
	}
	_ = a
}

func opRenew(s *store.SQLiteStore, res result) (result, error) {
	tk, err := requireTicket()
	if err != nil {
		return res, err
	}
	a, err := actor()
	if err != nil {
		return res, err
	}
	gen, err := strconv.ParseInt(getenv(envGen), 10, 64)
	if err != nil {
		return res, fmt.Errorf("%s: %w", envGen, err)
	}
	ct, err := s.RenewClaim(context.Background(), store.RenewRequest{
		TicketID: tk, Actor: a, Generation: gen, RequestID: "req-renew",
	})
	if err != nil {
		return res, err
	}
	res.OK = true
	res.TicketID = tk
	res.Generation = ct.Claim.Generation
	res.Version = ct.Version
	if ct.Claim.ExpiresAtMS != nil {
		res.ExpiresAtMS = *ct.Claim.ExpiresAtMS
	}
	return res, nil
}

func opRelease(s *store.SQLiteStore, res result) (result, error) {
	tk, err := requireTicket()
	if err != nil {
		return res, err
	}
	a, err := actor()
	if err != nil {
		return res, err
	}
	gen, err := strconv.ParseInt(getenv(envGen), 10, 64)
	if err != nil {
		return res, fmt.Errorf("%s: %w", envGen, err)
	}
	ct, err := s.ReleaseClaim(context.Background(), store.ReleaseRequest{
		TicketID: tk, Actor: a, Generation: gen, RequestID: "req-release",
	})
	if err != nil {
		return res, err
	}
	res.OK = true
	res.TicketID = tk
	res.Generation = ct.Claim.Generation
	res.Version = ct.Version
	return res, nil
}

// opHandoffCreate creates one handoff carrying LEDGERBOARD_TEST_WRITES memory
// writes for a ticket that must already be claimed (generation/version come
// from the environment, exactly as a caller that read its lease would send).
func opHandoffCreate(s *store.SQLiteStore, res result) (result, error) {
	tk, err := requireTicket()
	if err != nil {
		return res, err
	}
	a, err := actor()
	if err != nil {
		return res, err
	}
	key, err := requireEnv(envKey)
	if err != nil {
		return res, err
	}
	gen, err := strconv.ParseInt(getenv(envGen), 10, 64)
	if err != nil {
		return res, fmt.Errorf("%s: %w", envGen, err)
	}
	ver, err := strconv.ParseInt(getenv(envVersion), 10, 64)
	if err != nil {
		return res, fmt.Errorf("%s: %w", envVersion, err)
	}
	n := 0
	if v := getenv(envWrites); v != "" {
		n, err = strconv.Atoi(v)
		if err != nil {
			return res, fmt.Errorf("%s: %w", envWrites, err)
		}
	}
	writes := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		writes = append(writes, map[string]any{
			"type": "fact", "content": fmt.Sprintf("write %d from %s", i, a.ID), "confidence": 0.75,
		})
	}
	body := map[string]any{
		"schema_version":   1,
		"generation":       gen,
		"expected_version": ver,
		"summary":          "handoff created by harness-agent",
		"memory_writes":    writes,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return res, err
	}
	hash, err := store.RequestHash(json.RawMessage(raw))
	if err != nil {
		return res, err
	}
	hr, err := s.CreateHandoff(context.Background(), store.CreateHandoffRequest{
		TicketID: tk, Actor: a, RequestID: "req-handoff", Payload: raw,
		Scope: store.IdempotencyScope{Actor: a, Operation: store.OperationHandoffCreate, Key: key, RequestHash: hash},
	})
	if err != nil {
		return res, err
	}
	res.OK = true
	res.TicketID = tk
	res.HandoffID = hr.Document.ID
	res.Generation = hr.Document.Generation
	return res, nil
}

func mockAdapter() *memory.MockAdapter {
	return memory.NewMock(memory.MockConfig{CallsPath: getenv(envCalls)})
}

func opOutboxOnce(s *store.SQLiteStore, res result) (result, error) {
	found, err := s.ProcessOutboxOnce(context.Background(), store.OutboxWorkerConfig{
		WorkerID: workerID(), PollMS: 5, Adapter: mockAdapter(),
	})
	if err != nil {
		return res, err
	}
	res.Claimed = found
	res.OK = true
	return res, nil
}

// opOutboxLoop runs the delivery loop until one full iteration finds nothing
// claimable, then exits — a worker process that drains its backlog and stops,
// so the parent's Wait is a termination message, not a timeout.
func opOutboxLoop(s *store.SQLiteStore, res result) (result, error) {
	adapter := mockAdapter()
	for {
		found, err := s.ProcessOutboxOnce(context.Background(), store.OutboxWorkerConfig{
			WorkerID: workerID(), PollMS: 5, Adapter: adapter,
		})
		if err != nil {
			return res, err
		}
		if found {
			res.Delivered++
			continue
		}
		break
	}
	res.OK = true
	return res, nil
}

func opName(s string) string { return s }

// keepimports silences unused-import churn for the files the MP rows share.
var (
	_ = sql.ErrNoRows
	_ = syscall.SIGKILL
	_ = time.Now
)
