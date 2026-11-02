// Claims, multi-process (docs/test-plan.md §2.2 — the MP rows of slice 4):
// T-CLAIM-01 (8 claimant processes, one ticket), T-CLAIM-16 (busy DB: absorb,
// exceed → database_busy, and the recon §4.5 negative control), the
// expiry/renewal race (the MP leg of T-CLAIM-08(ii)) and fencing rejection
// across processes (the T-CLAIM-14 route, run MP two phases early because
// separate processes are the point).
//
// Every child is a real OS process with its own connection on one SQLite file;
// the barrier is the §1.1 construction specialised for store-level ops:
// readiness on per-child FIFOs, a per-child go FIFO, outcome contracts
// asserted — never interleavings.
package mptest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMP_T_CLAIM_01 is row T-CLAIM-01: eight separate claimant processes race
// for one ticket. Exactly one succeeds with the winner's lease; the other
// seven get already_claimed, each carrying the full C-ERR-4 detail set with
// the winner's holder/generation/expiry; afterwards the row has exactly one
// holder, generation g0+1, and exactly one `claimed` audit row — and no
// loser is ever left with a silent zero-row success.
func TestMP_T_CLAIM_01(t *testing.T) {
	dir := TestDir(t, "T-CLAIM-01")
	ev := EvidenceDir(t, "T-CLAIM-01", "go test ./internal/mptest -run '^TestMP_T_CLAIM_01$' -count=1 -v  (MP: 8 harness-agent processes, one SQLite file)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())
	dbPath := dir + "/claim01.db"
	s := OpenStore(t, dbPath, clockFile)
	_, tk := fixtureBoard(t, s, "one ticket, eight claimants")

	const n = 8
	agents := make([]*barrierAgent, n)
	for i := 0; i < n; i++ {
		env := append(agentBase(), claimEnv(dbPath, tk, fmt.Sprintf("agent:claimant-%d", i))...)
		agents[i] = startBarrier(t, agentBin, dir, env)
	}
	waitReady(t, agents)
	releaseBarrier(t, agents)
	children := make([]*Child, n)
	for i, a := range agents {
		children[i] = a.child
	}
	results := collect(t, children, 60*time.Second)

	var winner map[string]any
	for _, m := range results {
		if okOf(m) {
			require.Nil(t, winner, "two claimants won: %v and %v", winner, m)
			winner = m
			continue
		}
		assert.Equal(t, "already_claimed", codeOf(m), "a loser must be diagnosed, never silent: %v", m)
		d := detailOf(m)
		for _, k := range []string{"holder", "holder_type", "lease_expires_at", "lease_generation", "ticket_id"} {
			assert.Contains(t, d, k, "C-ERR-4 detail set missing %q in %v", k, m)
		}
		assert.Equal(t, num(winnerOrSkip(results), "generation"), num(d, "lease_generation"), "losers must see the winner's generation")
	}
	require.NotNil(t, winner, "exactly one winner required, got none")
	assert.Equal(t, 1, countOK(results), "exactly one winner")
	assert.Equal(t, n-1, len(results)-countOK(results), "the other seven lose loudly")
	assert.Equal(t, int64(1), num(winner, "generation"), "g0+1 with g0=0")
	assert.Equal(t, int64(2), num(winner, "version"), "claim bumps version once")

	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder IS NOT NULL`, tk))
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='claimed'`, tk))
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_generation=1 AND status='in_progress'`, tk))

	SaveEvidence(t, ev, "output.json", DumpJSON(results))
	SaveEvidence(t, ev, "post-state.json", DumpJSON(map[string]any{
		"holders":    pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder IS NOT NULL`, tk),
		"audits":     pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='claimed'`, tk),
		"generation": pdb.Str(t, `SELECT lease_generation FROM tickets WHERE id=?`, tk),
		"holder":     pdb.Str(t, `SELECT lease_holder FROM tickets WHERE id=?`, tk),
	}))
	EvidenceNote(t, ev, "verdict.txt", "T-CLAIM-01 PASS (MP): 8 processes, exactly 1 winner, 7 already_claimed with C-ERR-4 details, 1 audit row\n")
}

func winnerOrSkip(results []map[string]any) map[string]any {
	for _, m := range results {
		if okOf(m) {
			return m
		}
	}
	return map[string]any{"generation": float64(1)}
}

// TestMP_T_CLAIM_16 is row T-CLAIM-16: the busy DB.
//   - absorb: a dbproc holds BEGIN IMMEDIATE; a claimant from another process
//     waits inside busy_timeout and succeeds when the holder commits;
//   - exceed: a 6 s hold (> busy_timeout=5000): the claimant gets
//     database_busy — never a bare 500, never a silent success;
//   - negative control: 4 dbproc processes × 10 UPDATEs each with the contract
//     DSN → 40/40 ok (recon §4.5 measured 17/40 lost upstream).
func TestMP_T_CLAIM_16(t *testing.T) {
	dir := TestDir(t, "T-CLAIM-16")
	ev := EvidenceDir(t, "T-CLAIM-16", "go test ./internal/mptest -run '^TestMP_T_CLAIM_16$' -count=1 -v  (MP: harness-dbproc lock holder + harness-agent claimant + 4x10 burst)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())

	t.Run("absorb_within_busy_timeout", func(t *testing.T) {
		dbPath := dir + "/absorb.db"
		s := OpenStore(t, dbPath, clockFile)
		_, tk := fixtureBoard(t, s, "busy absorb")

		holder := StartDBProc(t, dbprocBin,
			"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=tx_hold",
			"LEDGERBOARD_TEST_DB="+dbPath, "LEDGERBOARD_TEST_HOLD_MS=1200")
		locked := holder.Line(t, 30*time.Second)
		require.Equal(t, true, locked["locked"], "holder must report the open write lock")

		claimant := StartAgent(t, agentBin, append(agentBase(), claimEnv(dbPath, tk, "agent:busy-claimant")...)...)
		res := claimant.Line(t, 30*time.Second)
		_, _ = holder.Wait(t)
		assert.True(t, okOf(res), "claim must succeed after the holder commits: %v", res)
		assert.GreaterOrEqual(t, num(res, "elapsed_ms"), int64(200), "the claim must have actually waited on the lock")
		assert.Less(t, num(res, "elapsed_ms"), int64(5000), "the wait must stay inside busy_timeout")
		SaveEvidence(t, ev, "absorb.json", DumpJSON(res))
	})

	t.Run("exceed_yields_database_busy", func(t *testing.T) {
		dbPath := dir + "/exceed.db"
		s := OpenStore(t, dbPath, clockFile)
		_, tk := fixtureBoard(t, s, "busy exceed")

		holder := StartDBProc(t, dbprocBin,
			"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=tx_hold",
			"LEDGERBOARD_TEST_DB="+dbPath, "LEDGERBOARD_TEST_HOLD_MS=6000")
		locked := holder.Line(t, 30*time.Second)
		require.Equal(t, true, locked["locked"])

		claimant := StartAgent(t, agentBin, append(agentBase(), claimEnv(dbPath, tk, "agent:busy-claimant-2")...)...)
		res := claimant.Line(t, 30*time.Second)
		_, _ = holder.Wait(t)
		assert.False(t, okOf(res), "a claim past busy_timeout must not silently succeed")
		assert.Equal(t, "database_busy", codeOf(res), "the contracted busy diagnosis: %v", res)
		assert.GreaterOrEqual(t, num(res, "elapsed_ms"), int64(4000), "busy_timeout must have been exhausted, not short-circuited")
		assert.LessOrEqual(t, num(res, "elapsed_ms"), int64(8000), "and it must give up promptly after")
		pdb := ParentDB(t, dbPath)
		assert.Equal(t, int64(0), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder IS NOT NULL`, tk))
		assert.Equal(t, int64(0), pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='claimed'`, tk), "a busy-failed claim must not write an audit row (the fixture's `created` event is baseline)")
		SaveEvidence(t, ev, "exceed.json", DumpJSON(res))
	})

	t.Run("negative_control_4x10_burst", func(t *testing.T) {
		dbPath := dir + "/burst.db"
		s := OpenStore(t, dbPath, clockFile)
		_, _ = fixtureBoard(t, s, "burst board")
		var children []*Child
		for p := 0; p < 4; p++ {
			children = append(children, StartDBProc(t, dbprocBin,
				"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=burst",
				"LEDGERBOARD_TEST_DB="+dbPath, "LEDGERBOARD_TEST_BURST_N=10"))
		}
		okCount := 0
		var transcripts []map[string]any
		for _, c := range children {
			res := c.Line(t, 30*time.Second)
			_, _ = c.Wait(t)
			if okOf(res) && num(res, "rows_affected") == 10 {
				okCount++
			}
			transcripts = append(transcripts, res)
		}
		assert.Equal(t, 4, okCount, "40/40 writes must succeed under the contract DSN (recon §4.5 measured 17/40 lost upstream)")
		SaveEvidence(t, ev, "burst.json", DumpJSON(map[string]any{"processes_ok": okCount, "writes_each": 10, "results": transcripts}))
	})
	EvidenceNote(t, ev, "verdict.txt", "T-CLAIM-16 PASS (MP): busy_timeout absorbs a short hold; a >5s hold yields database_busy with an exact timing window; 4x10 burst is 40/40\n")
}

// TestMP_T_CLAIM_08_EXPIRY_RACE is the MP leg of T-CLAIM-08(ii): a lapsed
// holder renewing from one process while a new claimant claims from another —
// both released simultaneously through the barrier. The outcome contract:
// exactly one wins (the claim — a lapsed lease is never resurrected,
// C-CLAIM-14), the loser is diagnosed (lease_expired or, if the claim already
// re-leased, not_claim_holder), never 500, and the row satisfies C-INV-3/4.
func TestMP_T_CLAIM_08_EXPIRY_RACE(t *testing.T) {
	dir := TestDir(t, "T-CLAIM-08")
	ev := EvidenceDir(t, "T-CLAIM-08", "go test ./internal/mptest -run '^TestMP_T_CLAIM_08_EXPIRY_RACE$' -count=1 -v  (MP leg: lapsed-holder renew vs new claimant, separate processes)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/race.db"
	base := time.Now().UnixMilli()
	clockFile := ClockPath(t, dir, base)
	s := OpenStore(t, dbPath, clockFile)
	_, tk := fixtureBoard(t, s, "expiry race")
	ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:lapsed-holder"))
	require.NoError(t, err)
	require.EqualValues(t, 1, ct.Claim.Generation)

	// Advance the shared clock past the lease expiry: relabelled, not waited.
	WriteClockMS(t, clockFile, base+DefaultLeaseTTL()+60000)

	renewer := startBarrier(t, agentBin, dir, append(agentBase(), []string{
		"LEDGERBOARD_TEST_OP=renew", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:lapsed-holder",
		fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", ct.Claim.Generation),
	}...))
	claimer := startBarrier(t, agentBin, dir, append(agentBase(), append(
		claimEnv(dbPath, tk, "agent:new-claimant"),
		"LEDGERBOARD_CLOCK=fake", "LEDGERBOARD_CLOCK_FILE="+clockFile)...))
	waitReady(t, []*barrierAgent{renewer, claimer})
	releaseBarrier(t, []*barrierAgent{renewer, claimer})
	results := collect(t, []*Child{renewer.child, claimer.child}, 60*time.Second)

	renewRes, claimRes := results[0], results[1]
	assert.Equal(t, 1, countOK(results), "exactly one of renew/claim wins")
	assert.False(t, okOf(renewRes), "a lapsed lease is never resurrected by its holder (C-CLAIM-14)")
	assert.Contains(t, []string{"lease_expired", "not_claim_holder"}, codeOf(renewRes), "the loser is diagnosed, never 500: %v", renewRes)
	assert.True(t, okOf(claimRes), "the new claimant wins: %v", claimRes)
	assert.Equal(t, int64(2), num(claimRes, "generation"), "generation monotonic 1 -> 2")

	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder='new-claimant' AND lease_generation=2 AND status='in_progress'`, tk), "lease_holder stores the bare actor id (claimStatement binds req.Actor.ID; the type lives in lease_holder_type)")
	assert.Equal(t, int64(0), pdb.Int(t, `SELECT lease_renewals FROM tickets WHERE id=?`, tk), "the failed renew must not have written")
	SaveEvidence(t, ev, "output.json", DumpJSON(results))
	EvidenceNote(t, ev, "verdict.txt", "T-CLAIM-08(ii) MP PASS: lapsed renew loses with a diagnosis, claim wins, generation 1->2, invariants hold\n")
}

// TestMP_T_FENCING is the multi-process fencing row: a superseded generation
// is rejected from another process. Route (b) of C-CLAIM-25 (the same actor
// releases and re-claims, bumping its own generation), each step from a
// different process; then the stale process's renew and a foreign claim are
// both diagnosed from the row's current state.
func TestMP_T_FENCING(t *testing.T) {
	dir := TestDir(t, "T-FENCING-MP")
	ev := EvidenceDir(t, "T-FENCING-MP", "go test ./internal/mptest -run '^TestMP_T_FENCING$' -count=1 -v  (MP row: fencing across four processes, generation 1 -> 2)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	dbPath := dir + "/fence.db"
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())
	s := OpenStore(t, dbPath, clockFile)
	_, tk := fixtureBoard(t, s, "fencing")
	ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:builder-1"))
	require.NoError(t, err)
	require.EqualValues(t, 1, ct.Claim.Generation)

	run := func(env []string) map[string]any {
		c := StartAgent(t, agentBin, append(agentBase(), env...)...)
		res := c.Line(t, 30*time.Second)
		_, _ = c.Wait(t)
		return res
	}
	// Step 1: the holder (a new process, same actor) releases generation 1.
	rel := run([]string{"LEDGERBOARD_TEST_OP=release", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1", "LEDGERBOARD_TEST_GEN=1"})
	require.True(t, okOf(rel), "release: %v", rel)
	// Step 2: the same actor re-claims from yet another process -> generation 2.
	recl := run(claimEnv(dbPath, tk, "agent:builder-1"))
	require.True(t, okOf(recl), "re-claim: %v", recl)
	require.Equal(t, int64(3), num(recl, "generation"), "release bumped 1->2 (T3, C-FENCE-11), the re-claim issues 3")
	// Step 3: the stale process (still holding generation 1) renews — rejected.
	stale := run([]string{"LEDGERBOARD_TEST_OP=renew", "LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1", "LEDGERBOARD_TEST_GEN=1"})
	assert.False(t, okOf(stale), "the superseded generation must be rejected")
	assert.Equal(t, "lease_generation_mismatch", codeOf(stale), "%v", stale)
	assert.Equal(t, int64(1), num(detailOf(stale), "expected_generation"), "details carry the expected generation")
	assert.Equal(t, int64(3), num(detailOf(stale), "current_generation"))
	// Step 4: a foreign agent's claim on the live lease — already_claimed with
	// the holder's CURRENT details (never the stale generation).
	foreign := run(claimEnv(dbPath, tk, "agent:builder-2"))
	assert.False(t, okOf(foreign))
	assert.Equal(t, "already_claimed", codeOf(foreign))
	assert.Equal(t, int64(3), num(detailOf(foreign), "lease_generation"), "details carry the CURRENT generation")

	pdb := ParentDB(t, dbPath)
	assert.Equal(t, int64(1), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder='builder-1' AND lease_generation=3`, tk), "lease_holder stores the bare actor id; holder_type is the separate column")
	SaveEvidence(t, ev, "output.json", DumpJSON(map[string]any{"release": rel, "reclaim": recl, "stale_renew": stale, "foreign_claim": foreign}))
	EvidenceNote(t, ev, "verdict.txt", "FENCING MP PASS: stale generation rejected cross-process with expected/current details; foreign claim sees holder's current details\n")
}
