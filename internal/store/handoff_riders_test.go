package store

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/rgracey/kanban-mcp/internal/apperr"
	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawHandoffRequest assembles a creation request whose payload is the raw JSON a
// client sent, which is the only way to express a value a Go struct cannot carry
// (a bare NaN token, a float schema_version).
func rawHandoffRequest(t *testing.T, ticketID string, a models.Actor, key, body string) CreateHandoffRequest {
	t.Helper()
	return CreateHandoffRequest{
		TicketID:  ticketID,
		Actor:     a,
		RequestID: "req-raw",
		Payload:   json.RawMessage(body),
		Scope: IdempotencyScope{
			Actor: a,
			Key:   key,
			// C-IDEM-10: the transport supplies the hash; for a payload that is not
			// valid JSON the test cannot canonicalise it, so it hashes the key instead
			// and only the hash format is exercised.
			RequestHash: hashOf(t, key),
		},
	}
}

// TestHandoffNonFiniteConfidenceNamesTheField is QA's slice-2 rider (a): a raw
// NaN / Infinity / -Infinity confidence is not JSON at all, and the error must
// name the confidence field path rather than the whole document.
func TestHandoffNonFiniteConfidenceNamesTheField(t *testing.T) {
	cases := []struct {
		name  string
		token string
		index int
	}{
		{name: "NaN", token: "NaN", index: 0},
		{name: "Infinity", token: "Infinity", index: 0},
		{name: "-Infinity", token: "-Infinity", index: 0},
		{name: "NaN in the second write", token: "NaN", index: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := setupClaimStore(t)
			_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
			a := agent("builder-1")
			claimed := claimTicket(t, s, ticketID, a)

			writes := fmt.Sprintf(
				`[{"type":"fact","content":"ok","confidence":0.5},{"type":"fact","content":"bad","confidence":%s}]`,
				tc.token)
			if tc.index == 0 {
				writes = fmt.Sprintf(
					`[{"type":"fact","content":"bad","confidence":%s},{"type":"fact","content":"ok","confidence":0.5}]`,
					tc.token)
			}
			body := fmt.Sprintf(
				`{"schema_version":1,"generation":%d,"expected_version":%d,"summary":"s","memory_writes":%s}`,
				claimed.Claim.Generation, claimed.Version, writes)

			_, err := s.CreateHandoff(t.Context(), rawHandoffRequest(t, ticketID, a, "k-"+tc.name, body))
			e := requireCode(t, err, apperr.CodeHandoffInvalid)
			assert.Equal(t, fmt.Sprintf("memory_writes[%d].confidence", tc.index), e.Details["field"])

			// Nothing was written: no handoff, no outbox row, no key row.
			assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM handoffs`))
			assert.EqualValues(t, 0, countRows(t, s, `SELECT COUNT(*) FROM outbox`))
		})
	}
}

// TestHandoffSchemaVersionTypeNamesTheField is QA's slice-2 rider (b): a
// schema_version type error must name schema_version, not document. It also pins
// the accepted spelling 1.0, which is a JSON number equal to the only version v1
// accepts.
func TestHandoffSchemaVersionTypeNamesTheField(t *testing.T) {
	bad := []string{`1.5`, `"1"`, `null`, `0`, `2`, `true`, `NaN`, `[]`}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			s, _, _ := setupClaimStore(t)
			_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
			a := agent("builder-1")
			claimed := claimTicket(t, s, ticketID, a)

			body := fmt.Sprintf(
				`{"schema_version":%s,"generation":%d,"expected_version":%d,"summary":"s"}`,
				raw, claimed.Claim.Generation, claimed.Version)
			_, err := s.CreateHandoff(t.Context(), rawHandoffRequest(t, ticketID, a, "k-sv", body))
			e := requireCode(t, err, apperr.CodeHandoffInvalid)
			assert.Equal(t, "schema_version", e.Details["field"],
				"a schema_version type error must name the field, not the document")
		})
	}

	t.Run("1.0 is accepted as the integer 1", func(t *testing.T) {
		s, _, _ := setupClaimStore(t)
		_, ticketID := newBoardAndTicket(t, s, models.StatusTodo, models.PriorityMedium)
		a := agent("builder-1")
		claimed := claimTicket(t, s, ticketID, a)

		body := fmt.Sprintf(
			`{"schema_version":1.0,"generation":%d,"expected_version":%d,"summary":"s"}`,
			claimed.Claim.Generation, claimed.Version)
		res, err := s.CreateHandoff(t.Context(), rawHandoffRequest(t, ticketID, a, "k-sv-ok", body))
		require.NoError(t, err)
		assert.Equal(t, HandoffSchemaVersion, res.Document.SchemaVersion)
	})
}
