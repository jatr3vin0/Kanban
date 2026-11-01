package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/memory"
)

// ErrNoMemoryAdapter is C-OUTBOX-18: `MEMORY_DELIVERY=worker` without an adapter
// is a startup error, because a worker that immediately fails every row would
// turn a configuration mistake into a pile of dead letters.
var ErrNoMemoryAdapter = errors.New(
	"MEMORY_DELIVERY=worker requires a memory adapter, and none is configured (C-OUTBOX-18)")

// OutboxWorkerConfig configures the delivery worker of §6.5.
type OutboxWorkerConfig struct {
	// WorkerID is written to outbox.lease_owner for the rows this worker holds.
	// Two workers on one database are safe (C-OUTBOX-5, C-OUTBOX-17) precisely
	// because this value is unique per process.
	WorkerID string
	// PollMS is LEDGERBOARD_WORKER_POLL_MS (C-CONV-10): how long the loop sleeps
	// when nothing is claimable. Zero means DefaultWorkerPollMS.
	PollMS int64
	// Adapter is the only thing the worker knows about the outside world
	// (C-OUTBOX-22). It is required.
	Adapter memory.Adapter
}

// normalize fills the defaults the contract names.
func (c OutboxWorkerConfig) normalize() OutboxWorkerConfig {
	if c.PollMS <= 0 {
		c.PollMS = DefaultWorkerPollMS
	}
	if c.WorkerID == "" {
		c.WorkerID = "worker-" + newUUID()[:8]
	}
	return c
}

// StartOutboxWorker validates the configuration and starts exactly one delivery
// loop for this process (C-OUTBOX-17), returning immediately.
//
// It is the only entry point a server calls, and it refuses to start without an
// adapter (C-OUTBOX-18). With MEMORY_DELIVERY=off nothing calls it at all: no
// goroutine starts, and rows accumulate visibly (C-OUTBOX-15).
func (s *SQLiteStore) StartOutboxWorker(ctx context.Context, cfg OutboxWorkerConfig) error {
	if cfg.Adapter == nil {
		return ErrNoMemoryAdapter
	}
	cfg = cfg.normalize()
	go func() {
		// The loop only returns when the shutdown context is done; a delivery
		// failure is recorded on its row, never fatal to the process. If it does
		// return for any other reason, all delivery for this process has stopped
		// (C-OUTBOX-17) — that must leave a trace, never be discarded.
		if err := s.runOutboxWorker(ctx, cfg); err != nil {
			slog.Error("outbox worker stopped", "worker_id", cfg.WorkerID, "err", err)
		}
	}()
	return nil
}

// RunOutboxWorker runs the delivery loop in the calling goroutine until ctx is
// done. StartOutboxWorker is the production shape; this is the same loop a test
// can own.
func (s *SQLiteStore) RunOutboxWorker(ctx context.Context, cfg OutboxWorkerConfig) error {
	if cfg.Adapter == nil {
		return ErrNoMemoryAdapter
	}
	return s.runOutboxWorker(ctx, cfg.normalize())
}

// runOutboxWorker is C-OUTBOX-17's loop: claim one row at a time, deliver it,
// and when nothing is claimable sleep PollMS on a select over the shutdown
// context — so shutdown never waits out a poll interval.
//
// A row-level error is never fatal to the loop. One stuck row, one transient
// database error or one locked file must not stop delivery for the whole board,
// and a loop that dies with nothing in the log and nothing in the outbox to say
// so is the failure mode this product exists to make impossible. The error is
// logged and the loop continues after the normal poll sleep; it returns nil only
// when ctx is done. The per-row outcome decision stays where it is: the row's own
// `WHERE ... AND lease_owner = ?` makes a stale worker harmless (C-OUTBOX-7), so
// the loop only has to survive.
func (s *SQLiteStore) runOutboxWorker(ctx context.Context, cfg OutboxWorkerConfig) error {
	poll := time.Duration(cfg.PollMS) * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		processed, err := s.ProcessOutboxOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("outbox worker iteration failed", "worker_id", cfg.WorkerID, "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(poll):
			}
			continue
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

// ProcessOutboxOnce is one worker iteration: recover stuck rows (C-OUTBOX-6),
// claim at most one due row (C-OUTBOX-5), deliver it, and record the outcome. It
// reports whether a row was claimed.
//
// It is exported so a test can drive the loop deterministically — one claim, one
// delivery, one assertion — instead of racing a goroutine.
func (s *SQLiteStore) ProcessOutboxOnce(ctx context.Context, cfg OutboxWorkerConfig) (bool, error) {
	if cfg.Adapter == nil {
		return false, ErrNoMemoryAdapter
	}
	cfg = cfg.normalize()

	// C-OUTBOX-6: an expired processing lease becomes `uncertain` before anything
	// is claimed, in its own transaction, so the state is observable (C-OUTBOX-3).
	if _, err := s.ReclaimExpiredOutboxRows(ctx); err != nil {
		return false, err
	}

	row, found, err := s.ClaimOutboxRow(ctx, cfg.WorkerID)
	if err != nil || !found {
		return false, err
	}
	if err := s.deliverOutboxRow(ctx, row, cfg); err != nil {
		return true, err
	}
	return true, nil
}

// deliverOutboxRow performs one delivery attempt: adapter call, then ack, and on
// failure a classified transition. Every outcome is decided by the row's own
// guards, so a worker whose lease lapsed mid-attempt changes nothing (C-OUTBOX-7).
func (s *SQLiteStore) deliverOutboxRow(ctx context.Context, row OutboxRow, cfg OutboxWorkerConfig) error {
	externalID, err := cfg.Adapter.Write(ctx, row.MemoryWrite())

	// Armed, this kills the process the instant the adapter has returned and
	// before any local acknowledgement commits — the success-then-crash window of
	// C-OUTBOX-9 that makes delivery at-least-once (C-OUTBOX-8).
	crashpoint.Hit(crashpoint.MemoryPostAdapterWrite)

	if err == nil && externalID == "" {
		// C-OUTBOX-2: no external id proves nothing about the far end, and a nil
		// error does not prove it either. "Unknown" is the honest label.
		_, ferr := s.FailOutboxRow(ctx, row, cfg.WorkerID, OutboxFailure{
			Status:  OutboxUncertain,
			Code:    "adapter_empty_external_id",
			Message: "the adapter returned no external id, so the write cannot be proved (C-OUTBOX-2)",
		})
		return ferr
	}
	if err != nil {
		_, ferr := s.FailOutboxRow(ctx, row, cfg.WorkerID, ClassifyDeliveryError(err))
		return ferr
	}

	acked, err := s.AckOutboxRow(ctx, row, cfg.WorkerID, externalID)
	if err != nil {
		return fmt.Errorf("ack outbox row %s: %w", row.ID, err)
	}
	// acked == false is C-OUTBOX-7: the lease was stolen or lapsed. The row is not
	// ours, no external id was written, and nobody is told it succeeded.
	_ = acked
	return nil
}
