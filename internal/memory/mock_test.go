package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testWrite(id string) MemoryWrite {
	return MemoryWrite{
		ID:         id,
		TicketID:   "ticket-1",
		HandoffID:  "handoff-1",
		Actor:      "agent:builder-1",
		ActorType:  "agent",
		Type:       "finding",
		Content:    "modernc.org/sqlite supports UPDATE ... RETURNING",
		Confidence: 0.9,
		Tags:       []string{"sqlite", "driver"},
		CreatedAt:  time.UnixMilli(1761865702180).UTC(),
	}
}

// TestMockWriteIsIdempotentOnID is the far-end half of C-OUTBOX-8 / C-IDEM-5: a
// repeated Write with the same id returns the same external id and stores one
// record, which is what makes at-least-once delivery safe.
func TestMockWriteIsIdempotentOnID(t *testing.T) {
	dir := t.TempDir()
	m := NewMock(MockConfig{CallsPath: filepath.Join(dir, "calls.jsonl")})
	ctx := context.Background()

	first, err := m.Write(ctx, testWrite("write-1"))
	require.NoError(t, err)
	second, err := m.Write(ctx, testWrite("write-1"))
	require.NoError(t, err)

	assert.NotEmpty(t, first)
	assert.Equal(t, first, second, "a repeated write id must return the same external id")
	assert.Equal(t, ExternalIDFor("write-1"), first)

	rec, err := m.Get(ctx, first)
	require.NoError(t, err)
	assert.Equal(t, testWrite("write-1").Content, rec.Content)
	assert.InDelta(t, 0.9, rec.Confidence, 1e-9)
	assert.Equal(t, []string{"sqlite", "driver"}, rec.Tags)

	// Two calls were recorded even though one record exists: at-least-once is
	// visible in the calls file, not hidden by the dedup.
	assert.Len(t, m.Calls(), 2)
	raw, err := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
	require.NoError(t, err)
	assert.Len(t, strings.Split(strings.TrimSpace(string(raw)), "\n"), 2)
}

// TestMockRecordsExactlyTheMemoryWrite is C-OUTBOX-22: the calls file holds the
// MemoryWrite and nothing else — no ticket row, no database handle, no request
// context.
func TestMockRecordsExactlyTheMemoryWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "calls.jsonl")
	m := NewMock(MockConfig{CallsPath: path})
	w := testWrite("write-2")
	_, err := m.Write(context.Background(), w)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line))

	// The call envelope: seq/op/outcome plus the ten MemoryWrite fields, and
	// nothing that could only have come from the board's storage.
	want := map[string]bool{
		"seq": true, "op": true, "outcome": true,
		"id": true, "ticket_id": true, "handoff_id": true, "actor": true,
		"actor_type": true, "type": true, "content": true, "confidence": true,
		"tags": true, "created_at": true, "external_id": true,
	}
	for key := range line {
		assert.True(t, want[key], "unexpected field %q in the adapter call (C-OUTBOX-22)", key)
	}
	for key := range want {
		assert.Contains(t, line, key)
	}
	assert.Equal(t, w.ID, line["id"])
	assert.Equal(t, "2025-10-30T23:08:22.180Z", line["created_at"])
}

// TestMockBehaviourClassification checks that each configured behaviour produces
// the error the outbox classifies on (C-OUTBOX-11), never a string to match.
func TestMockBehaviourClassification(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		behaviour MockBehaviour
		wantCode  string
		retryable bool
		plain     bool
	}{
		{name: "retryable", behaviour: BehaviourRetryable, wantCode: CodeAdapterUnavailable, retryable: true},
		{name: "permanent", behaviour: BehaviourPermanent, wantCode: CodeAdapterRejected, retryable: false},
		{name: "timeout", behaviour: BehaviourTimeout, wantCode: CodeAdapterTimeout, retryable: true},
		{name: "unclassified", behaviour: BehaviourUnclassified, plain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMock(MockConfig{Behaviour: tc.behaviour, ErrorMessage: "far end said no"})
			_, err := m.Write(ctx, testWrite("w-"+tc.name))
			require.Error(t, err)
			if tc.plain {
				var de *DeliveryError
				assert.False(t, errors.As(err, &de), "an unclassified error must not be a DeliveryError")
				return
			}
			var de *DeliveryError
			require.True(t, errors.As(err, &de))
			assert.Equal(t, tc.wantCode, de.Code)
			assert.Equal(t, tc.retryable, de.Retryable)
			assert.Contains(t, de.Error(), "far end said no")
		})
	}

	t.Run("empty id", func(t *testing.T) {
		m := NewMock(MockConfig{Behaviour: BehaviourEmptyID})
		id, err := m.Write(ctx, testWrite("w-empty"))
		require.NoError(t, err)
		assert.Empty(t, id, "an empty external id cannot prove success (C-OUTBOX-2)")
	})
}

// TestMockSearcherDetection is C-OUTBOX-19: Search is optional and detected by a
// type assertion at wiring time, never per call.
func TestMockSearcherDetection(t *testing.T) {
	full := NewMock(MockConfig{})
	assert.True(t, IsSearcher(full), "the full mock implements Searcher")

	bare := MockWithoutSearch{Mock: full}
	assert.False(t, IsSearcher(bare), "an adapter without Search must not be reported as a Searcher")

	// Search returns the corpus in CreatedAt order and honours the limit.
	corpus := []MemoryRecord{
		{ExternalID: "b", Content: "second", CreatedAt: time.UnixMilli(2000).UTC()},
		{ExternalID: "a", Content: "first", CreatedAt: time.UnixMilli(1000).UTC()},
	}
	searcher := NewMock(MockConfig{Corpus: corpus})
	got, err := searcher.Search(context.Background(), "", 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ExternalID)
	limited, err := searcher.Search(context.Background(), "", 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	// A missing external id is ErrNotFound, not an empty record.
	_, err = full.Get(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrNotFound)
}
