package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// MockBehaviour is the outcome the mock gives the next Write.
type MockBehaviour string

const (
	// BehaviourOK accepts the write and returns a stable external id.
	BehaviourOK MockBehaviour = "ok"
	// BehaviourRetryable fails with adapter_unavailable + Retryable=true.
	BehaviourRetryable MockBehaviour = "retryable"
	// BehaviourPermanent fails with adapter_rejected + Retryable=false.
	BehaviourPermanent MockBehaviour = "permanent"
	// BehaviourUnclassified fails with a plain Go error: the adapter did not
	// classify it, so the outbox must treat it as uncertain (C-OUTBOX-11).
	BehaviourUnclassified MockBehaviour = "unclassified"
	// BehaviourTimeout fails with adapter_timeout + Retryable=true. The outbox
	// must still record `uncertain`, because a request that may have arrived is
	// not a definite failure (C-OUTBOX-11 third clause).
	BehaviourTimeout MockBehaviour = "timeout"
	// BehaviourEmptyID returns a nil error and no external id, which cannot prove
	// success (C-OUTBOX-2) and is therefore uncertain.
	BehaviourEmptyID MockBehaviour = "empty_id"
)

// MockCall is one recorded adapter call. It is the calls.jsonl line shape: the
// evidence T-MEM-02/T-MEM-08 assert on.
type MockCall struct {
	Seq        int64    `json:"seq"`
	Op         string   `json:"op"`
	ID         string   `json:"id"`
	TicketID   string   `json:"ticket_id"`
	HandoffID  string   `json:"handoff_id"`
	Actor      string   `json:"actor"`
	ActorType  string   `json:"actor_type"`
	Type       string   `json:"type"`
	Content    string   `json:"content"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	CreatedAt  string   `json:"created_at"`
	ExternalID string   `json:"external_id"`
	Outcome    string   `json:"outcome"`
	ErrorCode  string   `json:"error_code,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// MockConfig configures a MockAdapter.
type MockConfig struct {
	// CallsPath, when set, is a JSONL file every call is appended to and fsynced
	// on. A crashed process still leaves its call behind, which is what the
	// at-least-once tests need to prove.
	CallsPath string
	// Behaviour is the outcome of Write. Empty means BehaviourOK.
	Behaviour MockBehaviour
	// ErrorMessage is the message carried by a failing behaviour. It exists so a
	// test can feed a credential in and assert the stored last_error is scrubbed
	// (C-SCHEMA-16).
	ErrorMessage string
	// FIFOPath, when set, is opened and written once per call ("call <id>\n"), so
	// a test learns the exact delivery instant without polling.
	FIFOPath string
	// Corpus backs Get and Search.
	Corpus []MemoryRecord
}

// MockAdapter is the Phase-5 mock of C-OUTBOX-21: it implements Write, Get and
// Search, records every call, and deduplicates by the stable write id so a
// repeated delivery returns the same external id — the far-end contract of
// C-OUTBOX-8 / C-IDEM-5.
type MockAdapter struct {
	mu        sync.Mutex
	cfg       MockConfig
	seq       int64
	calls     []MockCall
	byWriteID map[string]string       // write id -> external id (the dedup table)
	records   map[string]MemoryRecord // external id -> record
}

// NewMock builds a mock adapter. It is safe to use from several goroutines.
func NewMock(cfg MockConfig) *MockAdapter {
	if cfg.Behaviour == "" {
		cfg.Behaviour = BehaviourOK
	}
	return &MockAdapter{
		cfg:       cfg,
		byWriteID: make(map[string]string),
		records:   make(map[string]MemoryRecord),
	}
}

// SetBehaviour changes the outcome of subsequent Write calls, so one test can
// walk a row through several failure legs (T-MEM-03).
func (m *MockAdapter) SetBehaviour(b MockBehaviour, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Behaviour = b
	m.cfg.ErrorMessage = message
}

// ExternalIDFor is the deterministic external id of a write id: the same write
// id always yields the same external id, which is the dedup guarantee of
// C-IDEM-5 stated as a pure function.
func ExternalIDFor(writeID string) string { return "mem_" + writeID }

// Write implements Adapter.
func (m *MockAdapter) Write(ctx context.Context, w MemoryWrite) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	call := MockCall{
		Seq:        m.seq + 1,
		Op:         "write",
		ID:         w.ID,
		TicketID:   w.TicketID,
		HandoffID:  w.HandoffID,
		Actor:      w.Actor,
		ActorType:  w.ActorType,
		Type:       w.Type,
		Content:    w.Content,
		Confidence: w.Confidence,
		Tags:       append([]string{}, w.Tags...),
		CreatedAt:  w.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	var (
		externalID string
		err        error
	)
	switch m.cfg.Behaviour {
	case BehaviourOK:
		externalID = m.acceptLocked(w)
		call.ExternalID = externalID
		call.Outcome = "ok"
	case BehaviourEmptyID:
		call.Outcome = "empty_id"
	case BehaviourRetryable:
		err = &DeliveryError{Code: CodeAdapterUnavailable, Retryable: true, Err: m.errorLocked()}
		call.Outcome, call.ErrorCode, call.Error = "error", CodeAdapterUnavailable, err.Error()
	case BehaviourPermanent:
		err = &DeliveryError{Code: CodeAdapterRejected, Retryable: false, Err: m.errorLocked()}
		call.Outcome, call.ErrorCode, call.Error = "error", CodeAdapterRejected, err.Error()
	case BehaviourTimeout:
		// Retryable=true on purpose: C-OUTBOX-11's third clause must win over the
		// flag, because a timed-out request may have arrived.
		err = &DeliveryError{Code: CodeAdapterTimeout, Retryable: true, Err: m.errorLocked()}
		call.Outcome, call.ErrorCode, call.Error = "error", CodeAdapterTimeout, err.Error()
	default: // BehaviourUnclassified
		err = fmt.Errorf("%s", m.errorLocked().Error())
		call.Outcome, call.Error = "error", err.Error()
	}

	m.seq = call.Seq
	m.calls = append(m.calls, call)
	if werr := m.recordLocked(call); werr != nil {
		return "", werr
	}
	return externalID, err
}

// Get implements Adapter.
func (m *MockAdapter) Get(ctx context.Context, externalID string) (MemoryRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[externalID]
	if !ok {
		for _, c := range m.cfg.Corpus {
			if c.ExternalID == externalID {
				return c, nil
			}
		}
		return MemoryRecord{}, ErrNotFound
	}
	return rec, nil
}

// Search implements Searcher. Results are returned in CreatedAt order, newest
// last, limited to limit when limit > 0.
func (m *MockAdapter) Search(ctx context.Context, query string, limit int) ([]MemoryRecord, error) {
	m.mu.Lock()
	out := make([]MemoryRecord, 0, len(m.records)+len(m.cfg.Corpus))
	for _, rec := range m.records {
		out = append(out, rec)
	}
	out = append(out, m.cfg.Corpus...)
	m.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ExternalID < out[j].ExternalID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if query != "" {
		filtered := out[:0]
		for _, rec := range out {
			if strings.Contains(strings.ToLower(rec.Content), strings.ToLower(query)) {
				filtered = append(filtered, rec)
			}
		}
		out = filtered
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Calls returns the recorded calls in order.
func (m *MockAdapter) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// acceptLocked stores the record and returns the deduped external id.
func (m *MockAdapter) acceptLocked(w MemoryWrite) string {
	if existing, ok := m.byWriteID[w.ID]; ok {
		return existing
	}
	externalID := ExternalIDFor(w.ID)
	m.byWriteID[w.ID] = externalID
	m.records[externalID] = MemoryRecord{
		ExternalID: externalID,
		Type:       w.Type,
		Content:    w.Content,
		Confidence: w.Confidence,
		Tags:       append([]string{}, w.Tags...),
		CreatedAt:  w.CreatedAt.UTC(),
	}
	return externalID
}

// errorLocked builds the error the configured behaviour carries.
func (m *MockAdapter) errorLocked() error {
	msg := m.cfg.ErrorMessage
	if msg == "" {
		msg = "mock adapter failure"
	}
	return fmt.Errorf("%s", msg)
}

// recordLocked appends the call to the calls file and signals the FIFO.
func (m *MockAdapter) recordLocked(call MockCall) error {
	if m.cfg.CallsPath != "" {
		line, err := json.Marshal(call)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(m.cfg.CallsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			_ = f.Close()
			return err
		}
		// fsync: a SIGKILLed process must still leave its call behind, or the
		// at-least-once evidence would depend on the page cache.
		_ = f.Sync()
		if err := f.Close(); err != nil {
			return err
		}
	}
	if m.cfg.FIFOPath != "" {
		if f, err := os.OpenFile(m.cfg.FIFOPath, os.O_WRONLY, 0); err == nil {
			_, _ = f.WriteString("call " + call.ID + "\n")
			_ = f.Close()
		}
	}
	return nil
}

// MockWithoutSearch hides the Search method so a test can wire an adapter that
// is not a Searcher (C-OUTBOX-19, T-MEM-09). It embeds the mock but does not
// promote Search, because the field is unexported and no method is forwarded.
type MockWithoutSearch struct{ Mock *MockAdapter }

// Write implements Adapter.
func (n MockWithoutSearch) Write(ctx context.Context, w MemoryWrite) (string, error) {
	return n.Mock.Write(ctx, w)
}

// Get implements Adapter.
func (n MockWithoutSearch) Get(ctx context.Context, externalID string) (MemoryRecord, error) {
	return n.Mock.Get(ctx, externalID)
}

// NowUTC is a small helper so tests can build deterministic CreatedAt values.
func NowUTC(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
