package config

import (
	"strings"
	"testing"

	"github.com/rgracey/kanban-mcp/internal/store"
)

// TestLoad_MemoryDeliveryDefaultsOff is C-OUTBOX-14's default and the spec's
// delivery-off default: nothing is delivered until an operator asks for it.
func TestLoad_MemoryDeliveryDefaultsOff(t *testing.T) {
	clearEnv(t)
	t.Setenv("MEMORY_DELIVERY", "")

	cfg, err := load([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MemoryDelivery != store.MemoryDeliveryOff {
		t.Fatalf("MemoryDelivery: want %q, got %q", store.MemoryDeliveryOff, cfg.MemoryDelivery)
	}
	if cfg.WorkerPollMS != store.DefaultWorkerPollMS {
		t.Fatalf("WorkerPollMS: want %d, got %d", store.DefaultWorkerPollMS, cfg.WorkerPollMS)
	}
}

// TestLoad_MemoryDeliveryWorker covers the one accepted non-default value and the
// poll knob.
func TestLoad_MemoryDeliveryWorker(t *testing.T) {
	clearEnv(t)
	t.Setenv("MEMORY_DELIVERY", "worker")
	t.Setenv("LEDGERBOARD_WORKER_POLL_MS", "25")

	cfg, err := load([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MemoryDelivery != store.MemoryDeliveryWorker {
		t.Fatalf("MemoryDelivery: want %q, got %q", store.MemoryDeliveryWorker, cfg.MemoryDelivery)
	}
	if cfg.WorkerPollMS != 25 {
		t.Fatalf("WorkerPollMS: want 25, got %d", cfg.WorkerPollMS)
	}
}

// TestLoad_MemoryDeliveryInvalid is C-OUTBOX-14's "startup error naming the
// variable, never a silent fallback".
func TestLoad_MemoryDeliveryInvalid(t *testing.T) {
	for _, value := range []string{"banana", "on", "OFF ", "true"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MEMORY_DELIVERY", value)

			_, err := load([]string{})
			if err == nil {
				t.Fatalf("MEMORY_DELIVERY=%q was accepted", value)
			}
			if !strings.Contains(err.Error(), "MEMORY_DELIVERY") {
				t.Fatalf("the error must name the variable, got %v", err)
			}
		})
	}
}

// TestLoad_WorkerPollInvalid is C-CONV-10 applied to LEDGERBOARD_WORKER_POLL_MS.
func TestLoad_WorkerPollInvalid(t *testing.T) {
	for _, value := range []string{"0", "-5", "soon"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("LEDGERBOARD_WORKER_POLL_MS", value)

			_, err := load([]string{})
			if err == nil {
				t.Fatalf("LEDGERBOARD_WORKER_POLL_MS=%q was accepted", value)
			}
			if !strings.Contains(err.Error(), "LEDGERBOARD_WORKER_POLL_MS") {
				t.Fatalf("the error must name the variable, got %v", err)
			}
		})
	}
}
