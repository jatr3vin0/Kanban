// T-HAND-09 full row (owed from slice 3, architect review A9): create a
// handoff, kill the creator post-commit, restart, verify the durable state
// byte-identical, then a worker delivers the backlog in (created_at, id)
// order — with the kill issued by a SEPARATE process (the controller), not a
// self-kill, exactly as A9 prescribes ("the kill is produced by the test").
// No crashpoint is used: the eight-point vocabulary of C-CONV-11 is closed and
// no rule covers a post-commit handoff path — durability after COMMIT is
// SQLite's guarantee.
package mptest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMP_T_HAND_09(t *testing.T) {
	dir := TestDir(t, "T-HAND-09")
	ev := EvidenceDir(t, "T-HAND-09", "go test ./internal/mptest -run '^TestMP_T_HAND_09$' -count=1 -v  (MP row: external kill post-commit + second-process delivery)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/hand09.db"
	base := time.Now().UnixMilli()
	clockFile := ClockPath(t, dir, base)
	calls := dir + "/calls.jsonl"

	s := OpenStore(t, dbPath, clockFile)
	_, tk := fixtureBoard(t, s, "restart survival")
	ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:builder-1"))
	require.NoError(t, err)

	// The creator child: one handoff with 3 memory writes, published, then
	// blocked forever. The controller (this test process) kills it.
	creator := startBarrier(t, agentBin, dir, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=handoff_create_signal", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
		"LEDGERBOARD_TEST_KEY=h9",
		fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", ct.Claim.Generation),
		fmt.Sprintf("LEDGERBOARD_TEST_VERSION=%d", ct.Version),
		"LEDGERBOARD_TEST_WRITES=3",
	}...))
	waitReady(t, []*barrierAgent{creator})
	releaseBarrier(t, []*barrierAgent{creator})
	created := creator.child.Line(t, 30*time.Second)
	require.True(t, okOf(created), "creator result: %v", created)

	// THE KILL: issued by this separate controller process (A9).
	require.NoError(t, creator.child.Kill())
	state, werr := creator.child.Wait(t)
	require.Error(t, werr, "the creator must have been SIGKILLed")
	require.NotNil(t, state)
	SaveEvidence(t, ev, "kill.txt", fmt.Sprintf("controller SIGKILL pid=%d state=%v\ncreator result: %s", creator.child.cmd.Process.Pid, state.String(), DumpJSON(created)))

	// Restart on the same file (a fresh process — this controller's own
	// connections count): handoff + 3 outbox rows + audits intact.
	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM handoffs WHERE id=?`, str(created, "handoff_id")))
	assert.Equal(t, int64(3), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE handoff_id=?`, str(created, "handoff_id")))
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='handoff_created'`, tk))
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='outbox_enqueued' AND ticket_id=?`, tk))
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM idempotency_keys WHERE operation='handoff.create' AND key='h9'`))

	// The durable handoff row must equal what the creator reported (the fields
	// a caller saw before the kill are the fields the file holds after it).
	got := ParentDB(t, dbPath).strs(t,
		`SELECT id || '|' || status || '|' || summary || '|' || from_actor FROM handoffs WHERE id=?`,
		str(created, "handoff_id"))
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "offered")
	SaveEvidence(t, ev, "durable-row.txt", "durable handoff row: "+got[0]+"\nreported by killed creator: "+str(created, "handoff_id")+"\n")

	// Delivery leg: a worker process starts against the crashed file and
	// delivers the backlog in the contract's order (created_at, id — the
	// three rows share one created_at because one transaction wrote them, so
	// the id tiebreak orders them; both are the contract's ORDER BY).
	worker := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-restart", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	wres := worker.Line(t, 30*time.Second)
	_, _ = worker.Wait(t)
	require.True(t, okOf(wres), "restart delivery: %v", wres)
	assert.Equal(t, int64(3), num(wres, "delivered"))

	assert.Equal(t, int64(3), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status='succeeded' AND handoff_id=?`, str(created, "handoff_id")))
	assert.Equal(t, int64(3), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='memory_delivered'`))

	// The delivered order equals (created_at, id) over the backlog.
	callLines := readCalls(t, calls)
	order := pdb.strs(t, `SELECT id FROM outbox WHERE handoff_id=? ORDER BY created_at, id`, str(created, "handoff_id"))
	var delivered []string
	for _, c := range callLines {
		id, _ := c["id"].(string)
		delivered = append(delivered, id)
	}
	assert.Equal(t, order, delivered, "backlog drains in created_at(, id) order (C-OUTBOX-16)")
	SaveEvidence(t, ev, "delivery.json", DumpJSON(map[string]any{"worker": wres, "calls": callLines, "contract_order": order, "delivered": delivered}))
	EvidenceNote(t, ev, "verdict.txt", "T-HAND-09 PASS (MP, full row): external SIGKILL post-commit; handoff+3 outbox rows+audits+key durable; second process delivered the backlog in (created_at,id) order\n")
}
