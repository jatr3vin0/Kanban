package mptest

import (
	"encoding/json"

	"github.com/rgracey/kanban-mcp/internal/store"
)

// OutboxLeaseMS is the 60 s processing lease of §6.3 (C-OUTBOX-5).
func OutboxLeaseMS() int64 { return store.OutboxAttemptLeaseMS }

func jsonUnmarshal(line string, v any) error { return json.Unmarshal([]byte(line), v) }
