# Finding A15 — T-CRASH-D run-8 false failure: a leaked suite server held the row's probe port (environment, not product; fixed at the test layer)

Status: **CLOSED — root-caused, fixed, verified. Not a product defect; every
product assertion in the row passed in run 8.**

## Symptom (run 8, 2026-11-02 03:52)

`TestMP_T_CRASH_D` FAIL at 0.47s with one assertion:

```
migration_mp_test.go:910: An error is expected but got nil.
    Messages: no listener: a probe call is refused
```

Every other assertion in the row passed: the B1 watcher saw the dirty window on
attempt 1 (`landmarks.json` `b1_attempts:1`), the SIGKILL left `dirty=1`, both
fail-closed restarts exited 1 with the exact C-SCHEMA-19 message
(`failclosed.txt`, `failclosed-b2.txt`), recovery completed to version 2 with
the clean schema.

## Root cause chain (all steps observed, not inferred)

1. **Run 6 (03:20:37) leaked its B1 recovery server.** The session driving run 6
   died mid-row immediately after `StartServer` launched the recovery server:
   `pid 6306, /tmp/ledgerboard-scratch/bin/ledgerboard -port 18547 -db
   /tmp/ledgerboard-scratch/T-CRASH-d/crashed.db` (see
   `findings-A15-evidence/leaked-server-identity.txt`; start time == run 6's
   window; process alive for 48m59s until killed at 04:07). No cleanup runs
   when the test *process* dies — the leak was invisible to every later run.
2. **`nextPort()` handed a fixed range (18401–18700) to every mptest process on
   the box.** Deterministic arithmetic → any suite run could be assigned the
   leaked server's port. Run 7's row content consumed a different number of
   ports and missed it; run 8's landed on 18547.
3. **The killed migrating server never binds its port** (it is SIGKILLed
   mid-migration; `dirty=1` proves it) — so the post-kill `dialPort(port)`
   probe connected to the *leaked* server. A foreign listener is
   indistinguishable from a surviving server for a port-only probe.

Reproduction with the leaked server still up (2026-11-02 ~04:00):
`go test ./internal/mptest -run 'TestMP_T_CRASH_D$' -count=3` →
PASS / PASS / **FAIL** (0.33s, the same dial assertion) — the collision repeats
deterministically because the port arithmetic is deterministic. With the
leaked server killed, the same command passes 3/3.

## Fix (test layer only; assertions unchanged, one message clarified)

`internal/mptest/migration_mp_test.go`:

- `nextPort()` now returns ports from a **per-process band**,
  `18000 + (pid%48)*300 .. +299` (48 disjoint 300-port bands, all below the
  kernel ephemeral range 32768+). Two suites running concurrently on this box
  no longer share port numbers at all.
- New `claimPort(t)`: returns a candidate that **verifiably refuses
  connections right now** (a 250 ms dial), stepping over occupied ones, and
  `t.Fatalf`s with an explicit environment-collision message if the whole band
  is occupied. The row that asserts a listener's ABSENCE now claims its port
  with it (`port = claimPort(t)`), so the post-kill probe means exactly one
  thing. All post-kill assertions are byte-identical; only the assert message
  gained the ambiguity note.

Why this cannot mask a product defect: claimPort only skips ports that refuse
*before* the server starts. If the killed server ever survived (inherited-fd
listener), the post-kill dial still succeeds on the claimed port and the row
still fails — that is the defect class the probe exists for.

## Verification

- Post-fix, `go test ./internal/mptest -run 'TestMP_T_CRASH_D$' -count=5` →
  5/5 PASS (`ok 2.246s`) while the leaked server still held 18547.
- Leaked server killed at 04:07 (18547 DOWN re-verified).
- Full plan re-run (run 9) via `scripts/run-test-plan.sh` → see
  `../index.md`; expected 19 PASS · 1 FAIL (T-MIG-05, finding A14).

## Residual hazard (documented, accepted)

A foreign process could still bind a claimed port inside the ~0.3 s between
claim and probe. claimPort shrinks the window from "any time in the row" to
that sliver; if it ever fires again the failure signature is unambiguous and
`findings-A15-evidence/` holds the identification recipe (`/proc/net/tcp` →
inode → `/proc/*/fd`).
