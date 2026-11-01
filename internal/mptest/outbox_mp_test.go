// Memory/outbox, multi-process (docs/test-plan.md §2.5 + T-CRASH-c): two real
// worker processes on one SQLite file (C-OUTBOX-5, C-OUTBOX-17), the stuck-
// worker recovery of T-MEM-06 with a genuine second worker, and the full
// outbox recovery matrix of T-CRASH-c including at-least-once dedup
// (C-OUTBOX-8 / C-IDEM-5).
//
// The server binary refuses MEMORY_DELIVERY=worker without an adapter
// (C-OUTBOX-18 — v1 ships no adapter), so the workers here are harness-agent
// children running the store's own RunOutboxWorker loop with the mock
// adapter: test-support code exercising product code, no product change.
package mptest

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enqueuedHandoff creates a claimed ticket with a handoff carrying n memory
// writes through one agent process (the normal producer), returning the outbox
// row ids in creation order.
func enqueuedHandoff(t *testing.T, agentBin, dir, dbPath, clockFile string, n int) string {
	s := OpenStore(t, dbPath, clockFile)
	_, tk := fixtureBoard(t, s, "outbox producer")
	ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:builder-1"))
	require.NoError(t, err)
	agent := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=handoff_create", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
		"LEDGERBOARD_TEST_KEY=hk-" + fmt.Sprint(fifoSeq.Add(1)),
		fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", ct.Claim.Generation),
		fmt.Sprintf("LEDGERBOARD_TEST_VERSION=%d", ct.Version),
		fmt.Sprintf("LEDGERBOARD_TEST_WRITES=%d", n),
	}...)...)
	res := agent.Line(t, 30*time.Second)
	_, _ = agent.Wait(t)
	require.True(t, okOf(res), "producer handoff: %v", res)
	return tk
}

// outboxStatuses reads the outbox row statuses in (created_at, id) order.
func outboxStatuses(t *testing.T, dbPath string) []string {
	pdb := ParentDB(t, dbPath)
	return pdb.strs(t, `SELECT status FROM outbox ORDER BY created_at, id`)
}

// TestMP_T_MEM_01 is row T-MEM-01's second-process safety leg: two worker
// processes on one file drain one backlog — every row delivered exactly once,
// no double delivery, attempts consistent at 1, every row exactly one
// succeeded status and one memory_delivered audit (C-OUTBOX-5, C-OUTBOX-17,
// C-OUTBOX-24).
func TestMP_T_MEM_01(t *testing.T) {
	dir := TestDir(t, "T-MEM-01")
	ev := EvidenceDir(t, "T-MEM-01", "go test ./internal/mptest -run '^TestMP_T_MEM_01$' -count=1 -v  (MP: two worker processes, one file, 6 rows)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/mem01.db"
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())
	calls := dir + "/calls.jsonl"

	enqueuedHandoff(t, agentBin, dir, dbPath, clockFile, 6)
	require.Len(t, outboxStatuses(t, dbPath), 6, "6 outbox rows must be enqueued")

	w1 := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-a", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	w2 := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-b", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	r1 := w1.Line(t, 60*time.Second)
	r2 := w2.Line(t, 60*time.Second)
	_, _ = w1.Wait(t)
	_, _ = w2.Wait(t)
	assert.True(t, okOf(r1) && okOf(r2), "both workers exit cleanly after draining: %v %v", r1, r2)
	assert.Equal(t, int64(6), num(r1, "delivered")+num(r2, "delivered"), "the pair delivers exactly the 6 rows")

	// Exactly-once: one succeeded row per write, attempts 1, one audit each.
	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(6), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status='succeeded' AND attempts=1 AND external_id IS NOT NULL AND delivered_at IS NOT NULL`))
	assert.Equal(t, int64(6), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='memory_delivered'`))
	assert.Equal(t, int64(0), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status IN ('pending','processing','retryable','uncertain','failed')`))

	// No double delivery at the far end: 6 distinct write ids, each once.
	callLines := readCalls(t, calls)
	require.Len(t, callLines, 6)
	ids := map[string]int{}
	for _, line := range callLines {
		id, _ := line["id"].(string)
		ids[id]++
	}
	for id, n := range ids {
		assert.Equal(t, 1, n, "write id %s delivered more than once", id)
	}
	SaveEvidence(t, ev, "output.json", DumpJSON(map[string]any{"worker_a": r1, "worker_b": r2, "calls": callLines,
		"lease_owners": pdb.strs(t, `SELECT DISTINCT lease_owner FROM outbox`)}))
	EvidenceNote(t, ev, "verdict.txt", "T-MEM-01 PASS (MP): 2 worker processes drained 6 rows, exactly-once, attempts=1, 6 audits, no double delivery\n")
}

// readCalls parses the mock adapter's calls.jsonl.
func readCalls(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, jsonUnmarshal(line, &m), "line: %s", line)
		out = append(out, m)
	}
	return out
}

// TestMP_T_MEM_01_CLAIM_RACE is the direct C-OUTBOX-5 leg: one due row, two
// worker processes released together — exactly one claims it, the other finds
// nothing.
func TestMP_T_MEM_01_CLAIM_RACE(t *testing.T) {
	dir := TestDir(t, "T-MEM-01-CLAIMRACE")
	ev := EvidenceDir(t, "T-MEM-01-CLAIMRACE", "go test ./internal/mptest -run '^TestMP_T_MEM_01_CLAIM_RACE$' -count=1 -v  (MP: one due row, two claimant processes)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/race.db"
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())
	calls := dir + "/calls.jsonl"
	enqueuedHandoff(t, agentBin, dir, dbPath, clockFile, 1)

	a := startBarrier(t, agentBin, dir, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_claim", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=w-a", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...))
	b := startBarrier(t, agentBin, dir, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_claim", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=w-b", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...))
	waitReady(t, []*barrierAgent{a, b})
	releaseBarrier(t, []*barrierAgent{a, b})
	results := collect(t, []*Child{a.child, b.child}, 30*time.Second)

	claimed := 0
	rowID := ""
	for _, m := range results {
		if okOf(m) && m["claimed"].(bool) {
			claimed++
			rowID, _ = m["outbox_id"].(string)
		}
	}
	assert.Equal(t, 1, claimed, "exactly one worker holds the row (C-OUTBOX-5)")
	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status='processing' AND lease_owner IS NOT NULL`))
	assert.Equal(t, "processing", pdb.Str(t, `SELECT status FROM outbox WHERE id=?`, rowID))
	SaveEvidence(t, ev, "output.json", DumpJSON(results))
	EvidenceNote(t, ev, "verdict.txt", "C-OUTBOX-5 MP PASS: 1 due row, 2 processes, exactly 1 claim, other reports nothing claimable\n")
}

// TestMP_T_MEM_06 is row T-MEM-06 with a REAL second worker: worker A claims a
// row and is SIGKILLed mid-attempt (crashpoint memory.post_adapter_write); the
// row stays processing; the fake clock runs past its 60 s lease; worker B (a
// second process) reclaims — the row goes `uncertain` FIRST and is observed in
// that state with no external id — then delivers, and the mock's dedup keeps
// the far end consistent (C-OUTBOX-5/6/17, C-OUTBOX-2/9).
func TestMP_T_MEM_06(t *testing.T) {
	dir := TestDir(t, "T-MEM-06")
	ev := EvidenceDir(t, "T-MEM-06", "go test ./internal/mptest -run '^TestMP_T_MEM_06$' -count=1 -v  (MP: worker A killed mid-attempt, worker B reclaims uncertain-then-delivers)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/mem06.db"
	base := time.Now().UnixMilli()
	clockFile := ClockPath(t, dir, base)
	calls := dir + "/calls.jsonl"

	enqueuedHandoff(t, agentBin, dir, dbPath, clockFile, 1)

	// Worker A: one delivery attempt, killed after the adapter returned and
	// before the local ack commits.
	a, fifo := crashAgent(t, agentBin, dir, "memory.post_adapter_write", append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_once", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-a", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...))
	marker := ReadFIFO(t, fifo, 30*time.Second)
	require.Contains(t, marker, "crash memory.post_adapter_write")
	_, _ = a.child.Wait(t)

	pdb := ParentDB(t, dbPath)
	assert.Equal(t, "processing", pdb.Str(t, `SELECT status FROM outbox`), "the row is stuck in processing")
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT attempts FROM outbox`), "attempts incremented at claim")
	assert.Equal(t, int64(1), len(readCalls(t, calls)), "the far end already took the write (at-least-once)")

	// Fake clock past the 60 s processing lease: relabelled, not waited.
	WriteClockMS(t, clockFile, base+OutboxLeaseMS()+60000)

	// Worker B, step 1: reclaim only — the row must become uncertain FIRST,
	// visibly, with no external id and no delivery claim (C-OUTBOX-6).
	reclaimer := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_reclaim", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-b",
	}...)...)
	rec := reclaimer.Line(t, 30*time.Second)
	_, _ = reclaimer.Wait(t)
	require.True(t, okOf(rec), "reclaim: %v", rec)
	assert.Equal(t, int64(1), num(rec, "reclaimed"))
	assert.Equal(t, "uncertain", pdb.Str(t, `SELECT status FROM outbox`), "uncertain must be observable before any retry")
	assert.Equal(t, int64(0), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE external_id IS NOT NULL`), "no success was ever claimed before it was provable")

	// Worker B, step 2: one delivery — the same write id goes out again, the
	// far end dedups by stable id, the committed ack is the only succeeded.
	b := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=worker-b", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	bres := b.Line(t, 30*time.Second)
	_, _ = b.Wait(t)
	require.True(t, okOf(bres), "worker B delivery: %v", bres)

	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status='succeeded' AND external_id IS NOT NULL`))
	assert.Equal(t, int64(2), pdb.Int(t, `SELECT attempts FROM outbox`), "attempts counts the killed first attempt")
	callLines := readCalls(t, calls)
	require.Len(t, callLines, 2, "the same write id went out twice (at-least-once)")
	id1, _ := callLines[0]["id"].(string)
	id2, _ := callLines[1]["id"].(string)
	assert.Equal(t, id1, id2, "the retry reuses the SAME stable write id (C-IDEM-5)")
	assert.Equal(t, callLines[0]["external_id"], callLines[1]["external_id"], "the far end dedups to one external id")
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='memory_delivered'`), "exactly one delivered audit")
	SaveEvidence(t, ev, "output.json", DumpJSON(map[string]any{"crash_marker": marker, "reclaim": rec, "worker_b": bres, "calls": callLines,
		"lease_owners": pdb.strs(t, `SELECT DISTINCT lease_owner FROM outbox`)}))
	EvidenceNote(t, ev, "verdict.txt", "T-MEM-06 PASS (MP): killed worker A mid-attempt -> processing -> clock past 60s -> worker B reclaims to uncertain FIRST (no external id) -> delivers with the same write id; far end deduped\n")
}

// TestMP_T_CRASH_C is the full outbox recovery matrix (row T-CRASH-c): a kill
// at each of the two memory-boundary crashpoints (memory.post_adapter_write,
// memory.pre_ack_commit), then the contracted recovery: lease lapse (fake
// clock) -> reclaim marks uncertain -> retry with the same write id -> far end
// dedups -> exactly one committed succeeded. Includes the at-least-once
// dedup leg of T-MEM-08 (C-OUTBOX-2/6/8/9, C-IDEM-5).
func TestMP_T_CRASH_C(t *testing.T) {
	dir := TestDir(t, "T-CRASH-c")
	ev := EvidenceDir(t, "T-CRASH-c", "go test ./internal/mptest -run '^TestMP_T_CRASH_C$' -count=1 -v  (MP: recovery matrix over both memory crashpoints)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	matrix := map[string]any{}
	for _, point := range []string{"memory.post_adapter_write", "memory.pre_ack_commit"} {
		dbPath := dir + "/" + sanitize(point) + ".db"
		base := time.Now().UnixMilli()
		clockFile := ClockPath(t, dir, base)
		calls := dir + "/" + sanitize(point) + "-calls.jsonl"
		enqueuedHandoff(t, agentBin, dir, dbPath, clockFile, 1)

		a, fifo := crashAgent(t, agentBin, dir, point, append(agentBase(), []string{
			"LEDGERBOARD_TEST_OP=outbox_once", "LEDGERBOARD_TEST_DB=" + dbPath,
			"LEDGERBOARD_TEST_WORKER_ID=worker-a", "LEDGERBOARD_TEST_CALLS=" + calls,
		}...))
		marker := ReadFIFO(t, fifo, 30*time.Second)
		require.Contains(t, marker, "crash "+point)
		_, _ = a.child.Wait(t)

		pdb := ParentDB(t, dbPath)
		require.Equal(t, "processing", pdb.Str(t, `SELECT status FROM outbox`))
		require.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE external_id IS NULL`),
			"%s: the local ack never committed, no external id was written", point)

		WriteClockMS(t, clockFile, base+OutboxLeaseMS()+60000)

		reclaimer := StartAgent(t, agentBin, append(agentBase(), []string{
			"LEDGERBOARD_TEST_OP=outbox_reclaim", "LEDGERBOARD_TEST_DB=" + dbPath,
			"LEDGERBOARD_TEST_WORKER_ID=worker-b",
		}...)...)
		rec := reclaimer.Line(t, 30*time.Second)
		_, _ = reclaimer.Wait(t)
		require.Equal(t, "uncertain", pdb.Str(t, `SELECT status FROM outbox`), "%s: reclaim must label uncertain first", point)

		b := StartAgent(t, agentBin, append(agentBase(), []string{
			"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
			"LEDGERBOARD_TEST_WORKER_ID=worker-b", "LEDGERBOARD_TEST_CALLS=" + calls,
		}...)...)
		bres := b.Line(t, 30*time.Second)
		_, _ = b.Wait(t)
		require.True(t, okOf(bres), "%s: recovery delivery: %v", point, bres)

		callLines := readCalls(t, calls)
		require.Len(t, callLines, 2, "%s: two attempts at the far end (at-least-once)", point)
		assert.Equal(t, callLines[0]["id"], callLines[1]["id"], "%s: same stable write id", point)
		assert.Equal(t, callLines[0]["external_id"], callLines[1]["external_id"], "%s: far-end dedup", point)

		assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM outbox WHERE status='succeeded'`), "%s: exactly one succeeded row", point)
		assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='memory_delivered'`), "%s: one audit", point)
		matrix[point] = map[string]any{"crash": marker, "reclaim": rec, "deliver": bres, "calls": callLines,
			"attempts": pdb.Int(t, `SELECT attempts FROM outbox`)}
	}
	SaveEvidence(t, ev, "matrix.json", DumpJSON(matrix))
	EvidenceNote(t, ev, "verdict.txt", "T-CRASH-c PASS (MP): both memory-boundary kills recover through uncertain + same-write-id retry; the far end dedups; one succeeded row, one audit (T-MEM-08 at-least-once included)\n")
}

// TestMP_T_MEM_06_STALE guards C-OUTBOX-7's MP shape: a worker that lost its
// lease (because the row was reclaimed) cannot write anything on ack — the
// lease guard makes the stale worker harmless. Simulated with two processes:
// A claims, the clock passes the lease, B reclaims+delivers, then A (if it
// were still alive) acking must affect 0 rows — observed here as: after B's
// delivery the row shows worker-b's ownership and one audit only.
func TestMP_T_MEM_06_STALE(t *testing.T) {
	dir := TestDir(t, "T-MEM-06-STALE")
	ev := EvidenceDir(t, "T-MEM-06-STALE", "go test ./internal/mptest -run '^TestMP_T_MEM_06_STALE$' -count=1 -v  (MP: stale worker ack is harmless, C-OUTBOX-7)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/stale.db"
	base := time.Now().UnixMilli()
	clockFile := ClockPath(t, dir, base)
	calls := dir + "/calls.jsonl"
	enqueuedHandoff(t, agentBin, dir, dbPath, clockFile, 1)

	a := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_claim", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=stale-worker", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	ares := a.Line(t, 30*time.Second)
	_, _ = a.Wait(t)
	require.True(t, okOf(ares) && ares["claimed"].(bool), "A claims: %v", ares)

	WriteClockMS(t, clockFile, base+OutboxLeaseMS()+60000)

	b := StartAgent(t, agentBin, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=outbox_loop", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_WORKER_ID=fresh-worker", "LEDGERBOARD_TEST_CALLS=" + calls,
	}...)...)
	bres := b.Line(t, 30*time.Second)
	_, _ = b.Wait(t)
	require.True(t, okOf(bres), "B recovers and delivers: %v", bres)

	pdb := ParentDB(t, dbPath)
	assert.Equal(t, "succeeded", pdb.Str(t, `SELECT status FROM outbox`))
	assert.Equal(t, "fresh-worker", pdb.Str(t, `SELECT lease_owner FROM outbox`), "the winning ack is the lease holder at ack time")
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='memory_delivered'`))
	SaveEvidence(t, ev, "output.json", DumpJSON(map[string]any{"claim": ares, "deliver": bres}))
	EvidenceNote(t, ev, "verdict.txt", "C-OUTBOX-7 MP PASS: only the lease-holding worker's ack lands; the row's WHERE lease_owner guard decides\n")
}
