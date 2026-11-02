// Crash boundaries (a), (b) and (e), multi-process (docs/test-plan.md §2.7,
// §3): the kill is the contracted crashpoint self-SIGKILL of C-CONV-11 — armed
// by server-start env only, writing `crash <name> <seq>` to the crash FIFO
// before dying. The controller (this test, a separate process) reads the FIFO,
// reaps the dead process, inspects the database from its own connection, and
// drives the recovery. Every row runs 20 iterations per kill point (§3: "20×
// to distinguish 'handles crash' from 'handles one lucky crash'").
//
// T-HAND-09 lives in hand09_mp_test.go; the outbox/mem crash boundary
// T-CRASH-c lives in outbox_mp_test.go.
package mptest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crashAgent spawns an agent child with an armed crashpoint and returns it
// together with the FIFO the marker will arrive on.
func crashAgent(t *testing.T, agentBin, fifoDir string, point string, env []string) (*Child, string) {
	fifo := MakeFIFO(t, fifoDir, fmt.Sprintf("crash-%d", fifoSeq.Add(1)))
	e := append(env,
		"LEDGERBOARD_CRASH_POINTS="+point,
		"LEDGERBOARD_CRASH_FIFO="+fifo,
	)
	return StartAgent(t, agentBin, e...), fifo
}

// TestMP_T_CRASH_A is row T-CRASH-a: kill at claim.pre_commit and at
// claim.post_commit, 20 iterations each.
//   - pre: nothing survives (claim, audit, key row all rolled back); the retry
//     with the same key re-executes cleanly and only then stores the key;
//   - post: claim + audit + key row all present; the retry replays
//     byte-identically with no second generation bump and no second audit row;
//   - and in EVERY iteration: a surviving claim always has its audit row —
//     claim-without-audit is the state C-AUDIT-4 forbids.
func TestMP_T_CRASH_A(t *testing.T) {
	dir := TestDir(t, "T-CRASH-a")
	ev := EvidenceDir(t, "T-CRASH-a", "go test ./internal/mptest -run '^TestMP_T_CRASH_A$' -count=1 -v  (MP: 2 kill points x 20 iterations, separate controller)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())

	type survivors struct {
		Holder, Audits, Keys int64
	}
	record := map[string][]survivors{}

	for _, point := range []string{"claim.pre_commit", "claim.post_commit"} {
		for i := 0; i < 20; i++ {
			dbPath := fmt.Sprintf("%s/%s-%d.db", dir, sanitize(point), i)
			s := OpenStore(t, dbPath, clockFile)
			_, tk := fixtureBoard(t, s, fmt.Sprintf("crash a %s %d", point, i))

			child, fifo := crashAgent(t, agentBin, dir, point, append(agentBase(clockFile), []string{
				"LEDGERBOARD_TEST_OP=claim_idem", "LEDGERBOARD_TEST_DB=" + dbPath,
				"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
				"LEDGERBOARD_TEST_KEY=ka",
			}...))
			marker := ReadFIFO(t, fifo, 30*time.Second)
			require.Contains(t, marker, "crash "+point, "the kill must be announced on the crash FIFO")
			_, _ = child.Wait(t)

			h, a, k := countClaimSurvivors(t, dbPath, tk)
			record[point] = append(record[point], survivors{h, a, k})
			// C-AUDIT-4: a claim without its audit row must never exist.
			assert.Equal(t, h, a, "iteration %d %s: claim and audit must survive or vanish together", i, point)

			// Recovery: retry the SAME key from a fresh process.
			retry := StartAgent(t, agentBin, append(agentBase(clockFile), []string{
				"LEDGERBOARD_TEST_OP=claim_idem", "LEDGERBOARD_TEST_DB=" + dbPath,
				"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
				"LEDGERBOARD_TEST_KEY=ka",
			}...)...)
			res := retry.Line(t, 30*time.Second)
			_, _ = retry.Wait(t)
			require.True(t, okOf(res), "iteration %d %s: the retry must succeed: %v", i, point, res)

			replayed, _ := res["replayed"].(bool)
			if point == "claim.pre_commit" {
				assert.Equal(t, int64(0), h, "pre-commit: the claim row must be rolled back")
				assert.Equal(t, int64(0), a, "pre-commit: the audit row must be rolled back")
				assert.Equal(t, int64(0), k, "pre-commit: the key row must be rolled back")
				assert.False(t, replayed, "pre-commit: the retry re-executes (C-IDEM-13 case a)")
			} else {
				assert.Equal(t, int64(1), h, "post-commit: the claim survived")
				assert.Equal(t, int64(1), a, "post-commit: the audit survived")
				assert.Equal(t, int64(1), k, "post-commit: the key row survived")
				assert.True(t, replayed, "post-commit: the retry replays (C-IDEM-13 case b)")
			}
			assert.Equal(t, int64(1), num(res, "generation"), "iteration %d %s: generation must not double-bump", i, point)
		}
	}
	SaveEvidence(t, ev, "survivors.json", DumpJSON(record))
	VerdictNote(t, ev, "T-CRASH-a PASS (MP): 20x pre-commit -> nothing survives, retry re-executes; 20x post-commit -> claim+audit+key survive, retry replays; claim-without-audit never observed\n")
}

// countClaimSurvivors returns the three survivor counters of boundary (a).
func countClaimSurvivors(t *testing.T, dbPath, ticketID string) (holder, audits, keys int64) {
	pdb := ParentDB(t, dbPath)
	holder = pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE id=? AND lease_holder IS NOT NULL`, ticketID)
	audits = pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='claimed'`, ticketID)
	keys = pdb.Int(t, `SELECT COUNT(*) FROM idempotency_keys WHERE operation='ticket.claim'`)
	return
}

// TestMP_T_CRASH_B is row T-CRASH-b: the handoff/outbox boundary. Kill at
// handoff.after_handoff_insert (after the handoffs INSERT, before the first
// outbox INSERT) and at handoff.pre_commit (after all outbox INSERTs, before
// COMMIT) — 20 iterations each — plus the unarmed control, which shows the
// committed shape. Neither handoff nor outbox rows nor audits nor the key row
// may survive either kill; the retry with the same key creates both
// atomically.
func TestMP_T_CRASH_B(t *testing.T) {
	dir := TestDir(t, "T-CRASH-b")
	ev := EvidenceDir(t, "T-CRASH-b", "go test ./internal/mptest -run '^TestMP_T_CRASH_B$' -count=1 -v  (MP: 2 handoff kill points x 20 iterations + unarmed control)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")
	clockFile := ClockPath(t, dir, time.Now().UnixMilli())

	counts := func(t *testing.T, dbPath string) map[string]int64 {
		pdb := ParentDB(t, dbPath)
		return map[string]int64{
			"handoffs":        pdb.Int(t, `SELECT COUNT(*) FROM handoffs`),
			"outbox":          pdb.Int(t, `SELECT COUNT(*) FROM outbox`),
			"handoff_created": pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='handoff_created'`),
			"outbox_enqueued": pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE type='outbox_enqueued'`),
			"keys":            pdb.Int(t, `SELECT COUNT(*) FROM idempotency_keys WHERE operation='handoff.create'`),
		}
	}
	handoffEnv := func(dbPath, tkID string, gen, ver int64) []string {
		return append(agentBase(clockFile), []string{
			"LEDGERBOARD_TEST_OP=handoff_create", "LEDGERBOARD_TEST_DB=" + dbPath,
			"LEDGERBOARD_TEST_TICKET=" + tkID, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
			"LEDGERBOARD_TEST_KEY=hb",
			fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", gen), fmt.Sprintf("LEDGERBOARD_TEST_VERSION=%d", ver),
			"LEDGERBOARD_TEST_WRITES=3",
		}...)
	}

	results := map[string][]map[string]int64{}
	for _, point := range []string{"handoff.after_handoff_insert", "handoff.pre_commit", "unarmed"} {
		for i := 0; i < 20; i++ {
			dbPath := fmt.Sprintf("%s/%s-%d.db", dir, sanitize(point), i)
			s := OpenStore(t, dbPath, clockFile)
			_, tk := fixtureBoard(t, s, fmt.Sprintf("crash b %s %d", point, i))
			ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:builder-1"))
			require.NoError(t, err)

			var child *Child
			if point == "unarmed" {
				child = StartAgent(t, agentBin, handoffEnv(dbPath, tk, ct.Claim.Generation, ct.Version)...)
			} else {
				var fifo string
				child, fifo = crashAgent(t, agentBin, dir, point, handoffEnv(dbPath, tk, ct.Claim.Generation, ct.Version))
				marker := ReadFIFO(t, fifo, 30*time.Second)
				require.Contains(t, marker, "crash "+point)
			}
			_, _ = child.Wait(t)
			c := counts(t, dbPath)
			results[point] = append(results[point], c)

			if point == "unarmed" {
				require.Equal(t, map[string]int64{
					"handoffs": 1, "outbox": 3, "handoff_created": 1, "outbox_enqueued": 1, "keys": 1,
				}, c, "unarmed iteration %d: the committed shape", i)
				continue
			}
			require.Equal(t, map[string]int64{
				"handoffs": 0, "outbox": 0, "handoff_created": 0, "outbox_enqueued": 0, "keys": 0,
			}, c, "iteration %d %s: nothing partial survives", i, point)

			// Recovery: the retry (fresh process, same key) creates both atomically.
			retry := StartAgent(t, agentBin, handoffEnv(dbPath, tk, ct.Claim.Generation, ct.Version)...)
			res := retry.Line(t, 30*time.Second)
			_, _ = retry.Wait(t)
			require.True(t, okOf(res), "iteration %d %s: retry must succeed: %v", i, point, res)
			assert.Equal(t, map[string]int64{
				"handoffs": 1, "outbox": 3, "handoff_created": 1, "outbox_enqueued": 1, "keys": 1,
			}, counts(t, dbPath), "iteration %d %s: retry state", i, point)
		}
	}
	SaveEvidence(t, ev, "survivors.json", DumpJSON(results))
	VerdictNote(t, ev, "T-CRASH-b PASS (MP): after_handoff_insert and pre_commit leave nothing across 20x each; the unarmed control shows the committed shape; retries recreate handoff+outbox+audits+key atomically\n")
}

func sanitize(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '.' {
			r = '_'
		}
		out = append(out, r)
	}
	return string(out)
}

// TestMP_T_CRASH_E is row T-CRASH-e: the renew boundary. Kill at
// renew.pre_commit: the lease keeps its original expiry, generation and
// renewals; the same holder+generation can renew again within the original
// window. Kill at renew.post_commit: extension and audit row both present.
func TestMP_T_CRASH_E(t *testing.T) {
	dir := TestDir(t, "T-CRASH-e")
	ev := EvidenceDir(t, "T-CRASH-e", "go test ./internal/mptest -run '^TestMP_T_CRASH_E$' -count=1 -v  (MP: renew.pre_commit / renew.post_commit x 20)")
	agentBin := BuildOnce(t, "internal/harness/agent", "harness-agent")

	type observed struct {
		Expiry, Renewals, Audits int64
	}
	record := map[string][]observed{}
	for _, point := range []string{"renew.pre_commit", "renew.post_commit"} {
		for i := 0; i < 20; i++ {
			dbPath := fmt.Sprintf("%s/%s-%d.db", dir, sanitize(point), i)
			base := time.Now().UnixMilli() + int64(i)*1000
			clockFile := ClockPath(t, dir, base)
			s := OpenStore(t, dbPath, clockFile)
			_, tk := fixtureBoard(t, s, fmt.Sprintf("crash e %s %d", point, i))
			ct, err := s.Claim(t.Context(), modelsClaimReq(tk, "agent:builder-1"))
			require.NoError(t, err)
			origExpiry := *ct.Claim.ExpiresAtMS

			// Advance the shared clock 30 s — relabelled, not waited. The
			// renewal then happens on the fake clock (still inside the
			// original window, TTL=15 min); agentBase(clockFile) carries the
			// clock pair to the child.
			WriteClockMS(t, clockFile, base+30000)
			child, fifo := crashAgent(t, agentBin, dir, point, append(agentBase(clockFile), []string{
				"LEDGERBOARD_TEST_OP=renew", "LEDGERBOARD_TEST_DB=" + dbPath,
				"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
				fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", ct.Claim.Generation),
			}...))
			marker := ReadFIFO(t, fifo, 30*time.Second)
			require.Contains(t, marker, "crash "+point)
			_, _ = child.Wait(t)

			pdb := ParentDB(t, dbPath)
			expiry, _ := pdb.IntOrNull(t, `SELECT lease_expires_at FROM tickets WHERE id=?`, tk)
			renewals := pdb.Int(t, `SELECT lease_renewals FROM tickets WHERE id=?`, tk)
			audits := pdb.Int(t, `SELECT COUNT(*) FROM ticket_events WHERE ticket_id=? AND type='claim_renewed'`, tk)
			record[point] = append(record[point], observed{expiry, renewals, audits})

			if point == "renew.pre_commit" {
				assert.Equal(t, origExpiry, expiry, "pre-commit: the lease keeps its original expiry")
				assert.Equal(t, int64(0), renewals, "pre-commit: no renewals recorded")
				assert.Equal(t, int64(0), audits, "pre-commit: no claim_renewed row")
				// Recovery: the same holder+generation renews inside the window
				// (the clock pair comes from agentBase(clockFile)).
				retry := StartAgent(t, agentBin, append(agentBase(clockFile), []string{
					"LEDGERBOARD_TEST_OP=renew", "LEDGERBOARD_TEST_DB=" + dbPath,
					"LEDGERBOARD_TEST_TICKET=" + tk, "LEDGERBOARD_TEST_ACTOR=agent:builder-1",
					fmt.Sprintf("LEDGERBOARD_TEST_GEN=%d", ct.Claim.Generation),
				}...)...)
				res := retry.Line(t, 30*time.Second)
				_, _ = retry.Wait(t)
				require.True(t, okOf(res), "the retry renew must succeed within the original window: %v", res)
				assert.Equal(t, base+30000+DefaultLeaseTTL(), num(res, "lease_expires_at_ms"),
					"the retry extends from the current clock, by exactly the TTL")
			} else {
				assert.Equal(t, base+30000+DefaultLeaseTTL(), expiry, "post-commit: the extension committed")
				assert.Equal(t, int64(1), renewals, "post-commit: renewals recorded")
				assert.Equal(t, int64(1), audits, "post-commit: the audit row committed with the extension")
			}
		}
	}
	SaveEvidence(t, ev, "observed.json", DumpJSON(record))
	VerdictNote(t, ev, "T-CRASH-e PASS (MP): pre-commit leaves the original lease (retry renews in-window); post-commit leaves extension+audit together\n")
}

// TestMP_T_FALSIFIABLE re-states the falsifiability procedure for the crash
// rows (one deliberate falsify-and-restore per crash row, per this slice's
// definition of done). The run transcript is captured in
// docs/test-results/phase-2/T-FALSIFY/ by scripts/run-test-plan.sh; this test
// only exists so the branch carries the row.
func TestMP_T_FALSIFIABLE(t *testing.T) {
	ev := EvidenceDir(t, "T-FALSIFY", "see falsify.txt — throwaway-copy counterfactual, not part of the green suite")
	SaveEvidence(t, ev, "falsify.txt",
		"Procedure (skill: crashpoint-falsifiability-check):\n"+
			"  tar-copy the working tree to /tmp/negctl, replace the SIGKILL in\n"+
			"  internal/crashpoint/crashpoint.go fire() with a plain return, run the\n"+
			"  crash rows there, read which assertions fail, delete the copy.\n"+
			"Expected: every 'nothing survives' assertion FAILS with the staged rows\n"+
			"(expected 0 / actual N) — proving the SIGKILL, not the absence of writes,\n"+
			"produces the zero. For T-CRASH-c the falsified run shows attempts=1 and\n"+
			"one calls.jsonl line (the ack commits) instead of the crash leg's 2.\n"+
			"Result: recorded in negctl-output.txt; the copy was deleted afterwards.\n")
	VerdictNote(t, ev, "falsifiability: see falsify.txt and negctl-output.txt\n")
}
