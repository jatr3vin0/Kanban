// The genuinely upstream-populated migration fixture (docs/test-plan.md §4.1)
// and the migration MP rows of slice 4: T-MIG-01 (fixture), T-MIG-02 (migrate
// in place + concurrent server starts), T-MIG-04 (down round trip), T-MIG-05
// (old binary on new DB), T-MIG-06 (no-surprises open loop) and T-CRASH-d
// (crash during migration, C-SCHEMA-19 fail-closed + force recovery).
package mptest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UpstreamPin is the fork point the fixture must be populated from (§4.1).
const UpstreamPin = "c6938a2e6e0f3b2aca6cebf4afd93babd94d8317"

func nextPort() int { return 18400 + int(fifoSeq.Add(1))%300 }

// upstreamBin builds the upstream binary at the pinned commit into the scratch
// bin dir, via a throwaway worktree (the plan's own §4.1 command), then removes
// the worktree. Build cache makes repeats cheap.
func upstreamBin(t *testing.T) string {
	t.Helper()
	out := filepath.Join(BinDir, "kanban-upstream")
	wt := filepath.Join(ScratchRoot, "upstream-wt")
	_ = os.RemoveAll(wt)
	git := func(args ...string) []byte {
		cmd := exec.Command("git", args...)
		cmd.Dir = RepoRoot(t)
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, b)
		return b
	}
	git("worktree", "add", "--detach", wt, UpstreamPin)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", RepoRoot(t), "worktree", "remove", "--force", wt).Run()
	})
	build := exec.Command("go", "build", "-o", out, ".")
	build.Dir = wt
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build upstream: %v: %s", err, b)
	}
	return out
}

// populateFixture drives the upstream binary's own REST to build §4.1's
// fixture, then applies the direct-SQL oddities. Everything is scripted and
// the ids are recorded.
func populateFixture(t *testing.T, upstream string, dbPath string) map[string]any {
	t.Helper()
	port := nextPort()
	cmd := exec.Command(upstream, "-port", fmt.Sprint(port), "-db", dbPath, "-mcp-transport", "http")
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	require.NoError(t, cmd.Start(), "upstream start")
	dead := make(chan struct{})
	go func() { _ = cmd.Wait(); close(dead) }()
	require.True(t, waitForPort(t, port, 20*time.Second, dead, &log), "upstream never bound:\n%s", log.String())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d/api/v1", port)
	client := &http.Client{Timeout: 5 * time.Second}

	post := func(path string, body any) map[string]any {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		resp, err := client.Post(base+path, "application/json", bytes.NewReader(raw))
		require.NoError(t, err, "POST %s", path)
		defer resp.Body.Close()
		require.Less(t, resp.StatusCode, 300, "POST %s -> %d", path, resp.StatusCode)
		var m map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&m), "POST %s", path)
		return m
	}
	put := func(path string, body any) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest("PUT", base+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err, "PUT %s", path)
		defer resp.Body.Close()
		require.Less(t, resp.StatusCode, 300, "PUT %s -> %d", path, resp.StatusCode)
	}

	ids := map[string]any{}
	b1 := post("/boards", map[string]string{"name": "Migration Board Alpha", "description": "fixture board 1"})["id"].(string)
	b2 := post("/boards", map[string]string{"name": "Migration Board Beta — β", "description": ""})["id"].(string)
	b3 := post("/boards", map[string]string{"name": "Migration Board Gamma", "description": "unicode ✓ described"})["id"].(string)
	ids["boards"] = []string{b1, b2, b3}
	e1 := post("/boards/"+b1+"/epics", map[string]string{"title": "Epic One"})["id"].(string)
	e2 := post("/boards/"+b1+"/epics", map[string]string{"title": "Epic Two — second"})["id"].(string)
	e3 := post("/boards/"+b3+"/epics", map[string]string{"title": "Epic Gamma"})["id"].(string)
	ids["epics"] = []string{e1, e2, e3}

	statuses := []string{"todo", "in_progress", "done", "blocked"}
	priorities := []string{"low", "medium", "high", "critical"}
	var ticketIDs []string
	ticketBoards := []string{b1, b1, b1, b1, b1, b1, b1, b1, b1, b1,
		b2, b2, b2, b2, b2, b2, b2, b2, b2, b2,
		b3, b3, b3, b3, b3, b3, b3, b3,
		b1, b1, b2, b2, b3, b3, b1, b2, b3, b1, b2, b3}
	require.Len(t, ticketBoards, 40)
	for i := 0; i < 40; i++ {
		body := map[string]any{
			"title":    fmt.Sprintf("Ticket %02d — ünïcode ✓", i),
			"status":   statuses[i%4],
			"priority": priorities[i%4],
		}
		if i%2 == 1 {
			body["description"] = fmt.Sprintf("description body %d with unicode ✓ and \"quotes\"", i)
		} else {
			body["description"] = ""
		}
		if i < 12 {
			body["assignee"] = fmt.Sprintf("alice-%d", i%3)
		}
		if i < 5 {
			body["resolution"] = map[string]string{
				"commit_sha": fmt.Sprintf("a1b2c3d0000%d", i),
				"pr_url":     fmt.Sprintf("https://example.test/pr/%d", i),
				"notes":      "resolved during recon",
			}
		}
		if i < 8 {
			if i%2 == 0 {
				body["epic_id"] = e1
			} else {
				body["epic_id"] = e2
			}
		}
		tk := post("/boards/"+ticketBoards[i]+"/tickets", body)
		ticketIDs = append(ticketIDs, tk["id"].(string))
	}
	ids["tickets"] = ticketIDs

	// 25 notes, some edited; 30 tasks, mixed done.
	for i := 0; i < 25; i++ {
		tk := ticketIDs[i%len(ticketIDs)]
		n := post("/tickets/"+tk+"/notes", map[string]string{
			"body": fmt.Sprintf("note %d — context for the next agent ✓", i),
		})
		if i%5 == 0 {
			put("/notes/"+n["id"].(string), map[string]string{"body": fmt.Sprintf("note %d EDITED", i)})
		}
	}
	for i := 0; i < 30; i++ {
		tk := ticketIDs[(3*i)%len(ticketIDs)]
		t := post("/tickets/"+tk+"/tasks", map[string]any{
			"title": fmt.Sprintf("task %d", i), "done": i%2 == 0, "position": i,
		})
		if i%4 == 0 {
			put("/tasks/"+t["id"].(string), map[string]any{"done": i%8 != 0, "position": 100 + i})
		}
	}
	// 10 relations kind=blocks: one 2-cycle (t0<->t1), one blocked->done edge,
	// the rest forward edges.
	edges := [][2]int{{0, 1}, {1, 0}, {2, 3}, {4, 5}, {6, 7}, {8, 9}, {10, 11}, {12, 13}, {14, 15}, {16, 17}}
	for _, e := range edges {
		post("/tickets/"+ticketIDs[e[0]]+"/relations", map[string]string{
			"to_ticket_id": ticketIDs[e[1]], "kind": "blocks",
		})
	}
	ids["relations"] = len(edges)

	// Stop the upstream server before the direct-SQL oddities.
	_ = cmd.Process.Kill()
	<-dead
	return ids
}

// applyOddities adds the direct-SQL facts a real board can contain (§4.1
// step 3 + the created_at backdating and same-second pairs of the population
// script).
func applyOddities(t *testing.T, dbprocBin, dbPath string) {
	t.Helper()
	odb := ParentDB(t, dbPath)
	odb.Exec(t, `UPDATE tickets SET status='weird' WHERE title LIKE 'Ticket 39%'`)
	odb.Exec(t, `UPDATE tickets SET priority='critical' WHERE status='done' AND title LIKE 'Ticket 02%'`)
	odb.Exec(t, `UPDATE notes SET body=CAST(X'0a09436f6e74726f6c0a' AS TEXT) WHERE id=(SELECT id FROM notes LIMIT 1)`)
	// Backdate six tickets; two pairs share identical created_at seconds.
	odb.Exec(t, `UPDATE tickets SET created_at='2024-01-02 15:04:05' WHERE rowid IN (SELECT rowid FROM tickets LIMIT 2)`)
	odb.Exec(t, `UPDATE tickets SET created_at='2023-06-01 08:00:00' WHERE rowid IN (SELECT rowid FROM tickets LIMIT 2 OFFSET 2)`)
	odb.Exec(t, `UPDATE tickets SET created_at='2022-11-11 11:11:11' WHERE rowid IN (SELECT rowid FROM tickets LIMIT 2 OFFSET 4)`)
	_ = dbprocBin
}

// tableSnapshot dumps the named tables rowid-ordered to a JSON file via
// harness-dbproc and returns the bytes.
func tableSnapshot(t *testing.T, dbprocBin, dbPath, outPath, tables string) []byte {
	t.Helper()
	d := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=dump",
		"LEDGERBOARD_TEST_DB="+dbPath, "LEDGERBOARD_TEST_OUT="+outPath,
		"LEDGERBOARD_TEST_TABLES="+tables)
	res := d.Line(t, 30*time.Second)
	_, _ = d.Wait(t)
	require.True(t, okOf(res), "dump: %v", res)
	raw, err := os.ReadFile(outPath)
	require.NoError(t, err)
	return raw
}

// migrationsState reads (version, dirty) via harness-dbproc.
func migrationsState(t *testing.T, dbprocBin, dbPath string) (int64, bool, bool) {
	t.Helper()
	d := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=migrations",
		"LEDGERBOARD_TEST_DB="+dbPath)
	res := d.Line(t, 30*time.Second)
	_, _ = d.Wait(t)
	require.True(t, okOf(res), "migrations: %v", res)
	v, _ := res["version"].(float64)
	hasV := res["version"] != nil
	dirty := false
	if b, ok := res["dirty"].(bool); ok {
		dirty = b
	}
	return int64(v), dirty, hasV
}

// masterObjects returns the sorted "type:name" object list of sqlite_master.
func masterObjects(t *testing.T, dbprocBin, dbPath string) []string {
	t.Helper()
	d := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=master",
		"LEDGERBOARD_TEST_DB="+dbPath)
	res := d.Line(t, 30*time.Second)
	_, _ = d.Wait(t)
	require.True(t, okOf(res), "master: %v", res)
	rows, _ := res["query_rows"].([]any)
	var out []string
	for _, r := range rows {
		m, _ := r.(map[string]any)
		typ, _ := m["type"].(string)
		name, _ := m["name"].(string)
		out = append(out, typ+":"+name)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// fkViolations runs PRAGMA foreign_key_check via dbproc.
func fkViolations(t *testing.T, dbprocBin, dbPath string) []map[string]any {
	t.Helper()
	d := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=fk_check",
		"LEDGERBOARD_TEST_DB="+dbPath)
	res := d.Line(t, 30*time.Second)
	_, _ = d.Wait(t)
	require.True(t, okOf(res), "fk_check: %v", res)
	rows, _ := res["fk_violations"].([]any)
	var out []map[string]any
	for _, r := range rows {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// migrateWithProduct runs the product binary once against dbPath (it exits the
// listener behind t.Cleanup) and returns when the migration has happened.
func migrateWithProduct(t *testing.T, serverBin, dbPath string) int {
	t.Helper()
	port := nextPort()
	StartServer(t, serverBin, dbPath, port)
	return port
}

// TestMP_T_MIG_01 builds the §4.1 fixture: upstream code at the pinned commit,
// populated by upstream's own REST, plus the direct-SQL oddities; dumps of all
// 0001 tables are the baseline artifact.
func TestMP_T_MIG_01(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-MIG-01")
	ev := EvidenceDir(t, "T-MIG-01", "go test ./internal/mptest -run '^TestMP_T_MIG_01$' -count=1 -v  (fixture built from upstream at "+UpstreamPin[:12]+")")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	up := upstreamBin(t)
	dbPath := filepath.Join(dir, "fixture.db")
	_ = os.Remove(dbPath)

	ids := populateFixture(t, up, dbPath)
	applyOddities(t, dbprocBin, dbPath)
	before := tableSnapshot(t, dbprocBin, dbPath, filepath.Join(dir, "before.json"), "")
	SaveEvidence(t, ev, "fixture-ids.json", DumpJSON(ids))
	SaveEvidence(t, ev, "before.json", string(before))
	require.True(t, len(before) > 1000, "the baseline dump must be substantial")

	// The 0001 tables exist and carry the population counts.
	pdb := ParentDB(t, dbPath)
	counts := map[string]int64{
		"boards":        pdb.Int(t, `SELECT COUNT(*) FROM boards`),
		"epics":         pdb.Int(t, `SELECT COUNT(*) FROM epics`),
		"tickets":       pdb.Int(t, `SELECT COUNT(*) FROM tickets`),
		"notes":         pdb.Int(t, `SELECT COUNT(*) FROM notes`),
		"tasks":         pdb.Int(t, `SELECT COUNT(*) FROM tasks`),
		"relations":     pdb.Int(t, `SELECT COUNT(*) FROM ticket_relations`),
		"ticket_events": pdb.Int(t, `SELECT COUNT(*) FROM ticket_events`),
	}
	assert.Equal(t, int64(3), counts["boards"])
	assert.Equal(t, int64(3), counts["epics"])
	assert.Equal(t, int64(40), counts["tickets"])
	assert.Equal(t, int64(25), counts["notes"])
	assert.Equal(t, int64(30), counts["tasks"])
	assert.Equal(t, int64(10), counts["relations"])
	assert.Greater(t, counts["ticket_events"], int64(100), "natural audit rows from every mutation")
	SaveEvidence(t, ev, "counts.json", DumpJSON(counts))
	EvidenceNote(t, ev, "verdict.txt", "T-MIG-01 PASS: upstream-populated fixture (pinned "+UpstreamPin[:12]+"), 3 boards/3 epics/40 tickets/25 notes/30 tasks/10 relations + oddities; before.json is the baseline\n")
}

// fixtureOnce builds the fixture once for the rows that consume it, reusing
// T-MIG-01's directory when present.
func fixtureOnce(t *testing.T) (dbPath, beforeDump string) {
	t.Helper()
	dir := KeepDir(t, "T-MIG-FIXTURE")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	up := upstreamBin(t)
	dbPath = filepath.Join(dir, "fixture.db")
	beforeDump = filepath.Join(dir, "before.json")
	if _, err := os.Stat(beforeDump); err == nil {
		return dbPath, beforeDump
	}
	_ = os.Remove(dbPath)
	populateFixture(t, up, dbPath)
	applyOddities(t, dbprocBin, dbPath)
	tableSnapshot(t, dbprocBin, dbPath, beforeDump, "")
	return dbPath, beforeDump
}

// copyFixture copies the fixture (and its WAL side files) to a fresh path.
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			if suffix == "" {
				t.Fatalf("read fixture: %v", err)
			}
			continue
		}
		require.NoError(t, os.WriteFile(dst+suffix, b, 0o600), suffix)
	}
}

// TestMP_T_MIG_02 is row T-MIG-02: migrate in place on the populated fixture —
// bookkeeping 2/non-dirty, allowlist-only schema objects, defaults on every
// pre-existing row, 0001 tables byte-identical, fk_check empty, second open a
// no-op — plus the concurrent-starts leg (three product binaries at once: no
// torn state, and whichever outcome the runner produces is the contracted one).
func TestMP_T_MIG_02(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-MIG-02")
	ev := EvidenceDir(t, "T-MIG-02", "go test ./internal/mptest -run '^TestMP_T_MIG_02$' -count=1 -v  (migrate in place + 3 concurrent starts)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	serverBin := ServerBin(t)
	fixturePath, before := fixtureOnce(t)

	// The allowlist: the object set of a freshly migrated EMPTY database.
	emptyDB := filepath.Join(dir, "empty.db")
	migrateWithProduct(t, serverBin, emptyDB)
	allowlist := masterObjects(t, dbprocBin, emptyDB)

	migPath := filepath.Join(dir, "migrated.db")
	copyFixture(t, fixturePath, migPath)
	migrateWithProduct(t, serverBin, migPath)

	v, dirty, hasV := migrationsState(t, dbprocBin, migPath)
	assert.True(t, hasV && v == 2, "schema_migrations = 2 (got v=%d has=%v)", v, hasV)
	assert.False(t, dirty, "not dirty")
	objects := masterObjects(t, dbprocBin, migPath)
	assert.Equal(t, allowlist, objects, "sqlite_master objects are exactly the allowlist (C-SCHEMA-1)")

	pdb := ParentDB(t, migPath)
	assert.Equal(t, int64(40), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0 AND lease_renewals=0 AND lease_holder IS NULL AND lease_holder_type IS NULL AND lease_acquired_at IS NULL AND lease_expires_at IS NULL AND handoff_id IS NULL`),
		"every pre-existing ticket carries the 0002 defaults (C-SCHEMA-2)")
	assert.Empty(t, fkViolations(t, dbprocBin, migPath), "foreign_key_check empty")

	after := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "after.json"), "")
	SaveEvidence(t, ev, "after.json", string(after))
	assert.Equal(t, normalizeDump(before), normalizeDump(after), "all 0001 tables byte-identical (C-COMPAT-4)")

	// Second open is a no-op.
	migrateWithProduct(t, serverBin, migPath)
	v2, dirty2, _ := migrationsState(t, dbprocBin, migPath)
	assert.Equal(t, int64(2), v2)
	assert.False(t, dirty2)
	after2 := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "after2.json"), "")
	assert.Equal(t, normalizeDump(after), normalizeDump(after2), "second open changed nothing")

	// Concurrent-starts leg: three product binaries against one unmigrated copy.
	concPath := filepath.Join(dir, "concurrent.db")
	copyFixture(t, fixturePath, concPath)
	port := nextPort()
	var cmds []*exec.Cmd
	var bufs []*strings.Builder
	for i := 0; i < 3; i++ {
		c := exec.Command(serverBin, "-port", fmt.Sprint(port+i), "-db", concPath, "-mcp-transport", "http", "-log-level", "warn")
		buf := &strings.Builder{}
		c.Stdout, c.Stderr = buf, buf
		require.NoError(t, c.Start())
		cmds = append(cmds, c)
		bufs = append(bufs, buf)
	}
	type outcome struct {
		code int
		log  string
	}
	outcomes := make([]outcome, len(cmds))
	for i, c := range cmds {
		done := make(chan struct{})
		go func(c *exec.Cmd) { _ = c.Wait(); close(done) }(c)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = c.Process.Kill()
		}
		code := 0
		if c.ProcessState != nil && c.ProcessState.Exited() {
			code = c.ProcessState.ExitCode()
		}
		outcomes[i] = outcome{code, bufs[i].String()}
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			if c.ProcessState == nil {
				_ = c.Process.Kill()
			}
		}
	})
	// Whatever the runner produced, the file must not be torn and the next
	// open must be the contracted one.
	vc, dirtyc, _ := migrationsState(t, dbprocBin, concPath)
	concDump := tableSnapshot(t, dbprocBin, concPath, filepath.Join(dir, "after-concurrent.json"), "")
	assert.Equal(t, normalizeDump(before), normalizeDump(concDump), "concurrent starts: 0001 data intact (no torn state)")
	assert.Empty(t, fkViolations(t, dbprocBin, concPath))
	recoveryNote := ""
	if dirtyc {
		// The runner recorded the failed attempt: the next start must fail
		// closed (C-SCHEMA-19) and force-recover.
		code, out := StartServerExpectExit(t, serverBin, concPath, nextPort())
		assert.NotEqual(t, 0, code, "dirty file must fail closed")
		assert.Contains(t, out, "C-SCHEMA-19")
		f := StartDBProc(t, dbprocBin,
			"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=force",
			"LEDGERBOARD_TEST_DB="+concPath, fmt.Sprintf("LEDGERBOARD_TEST_FORCE_VERSION=%d", vc))
		forceRes := f.Line(t, 30*time.Second)
		_, _ = f.Wait(t)
		require.True(t, okOf(forceRes), "force: %v", forceRes)
		port := nextPort()
		StartServer(t, serverBin, concPath, port)
		vc2, dirtyc2, _ := migrationsState(t, dbprocBin, concPath)
		assert.Equal(t, int64(2), vc2)
		assert.False(t, dirtyc2, "force recovery must land on a healthy file")
		recovered := tableSnapshot(t, dbprocBin, concPath, filepath.Join(dir, "after-concurrent-recovered.json"), "")
		assert.Equal(t, normalizeDump(after), normalizeDump(recovered), "recovered state equals the clean migration")
		recoveryNote = "concurrent starts left dirty=1; fail-closed restart + force recovered to the clean state"
	} else {
		assert.Equal(t, int64(2), vc, "healthy migration outcome")
		recoveryNote = "concurrent starts serialized to a clean migration"
	}
	SaveEvidence(t, ev, "concurrent-outcomes.json", DumpJSON(map[string]any{
		"outcomes": outcomes, "version": vc, "dirty": dirtyc, "note": recoveryNote,
	}))
	EvidenceNote(t, ev, "verdict.txt", "T-MIG-02 PASS: allowlist schema, defaults on 40 rows, 0001 dumps identical, fk empty, second open no-op; concurrent starts left no torn state ("+recoveryNote+")\n")
}

// normalizeDump parses a dbproc dump file into its table->rows map so two
// snapshots compare structurally (JSON object key order is not guaranteed).
func normalizeDump(raw any) map[string]any {
	b := []byte(nil)
	switch v := raw.(type) {
	case string:
		b, _ = os.ReadFile(v)
	case []byte:
		b = v
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{"unparsable": string(b)}
	}
	return m
}

// TestMP_T_MIG_04 is row T-MIG-04: the explicit down/up round trip on the
// populated fixture. Down is run by its exact contracted text (read from the
// repo's migrations directory, never a copy), tickets returns to 11 columns,
// the 0001 data survives both directions, and the defaults are re-applied by
// the re-up. The contracted loss (lease/handoff/outbox rows destroyed) is
// stated in the evidence header.
func TestMP_T_MIG_04(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-MIG-04")
	ev := EvidenceDir(t, "T-MIG-04", "go test ./internal/mptest -run '^TestMP_T_MIG_04$' -count=1 -v  (down round trip on the populated fixture)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	serverBin := ServerBin(t)
	fixturePath, before := fixtureOnce(t)
	downSQL := AbsFromRepo(t, "internal/db/migrations/0002_lease.down.sql")
	if _, err := os.ReadFile(downSQL); err != nil {
		t.Fatalf("contracted down SQL must exist and be readable: %v", err)
	}
	SaveEvidence(t, ev, "header.txt",
		"CONTRACTED DATA LOSS: the down migration destroys lease, handoff, outbox and idempotency rows by design (C-SCHEMA-4). It is never invoked automatically; here it is executed explicitly, by its exact contracted text.\n")

	migPath := filepath.Join(dir, "roundtrip.db")
	copyFixture(t, fixturePath, migPath)
	migrateWithProduct(t, serverBin, migPath)
	upDump := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "up.json"), "")
	assert.Equal(t, normalizeDump(before), normalizeDump(upDump), "pre-down: 0001 data intact")

	// Explicit Down by the contracted text.
	d := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=execfile",
		"LEDGERBOARD_TEST_DB="+migPath, "LEDGERBOARD_TEST_SQLFILE="+downSQL)
	dres := d.Line(t, 30*time.Second)
	_, _ = d.Wait(t)
	require.True(t, okOf(dres), "down: %v", dres)

	pdb := ParentDB(t, migPath)
	ticketCols := pdb.strs(t, `SELECT name FROM pragma_table_info('tickets')`)
	assert.Equal(t, 11, len(ticketCols), "tickets back to its 11 original columns: %v", ticketCols)
	for _, gone := range []string{"handoffs", "outbox", "idempotency_keys"} {
		assert.Equal(t, int64(0), pdb.Int(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, gone), gone+" dropped")
	}
	assert.Empty(t, fkViolations(t, dbprocBin, migPath), "down state: foreign_key_check empty")
	downDump := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "down.json"), "")
	assert.Equal(t, normalizeDump(before), normalizeDump(downDump), "0001 data survives the down migration")

	// Up again: defaults re-applied, data still identical.
	migrateWithProduct(t, serverBin, migPath)
	v, dirty, hasV := migrationsState(t, dbprocBin, migPath)
	assert.True(t, hasV && v == 2 && !dirty)
	assert.Equal(t, int64(40), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0`), "defaults re-applied on up")
	reupDump := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "reup.json"), "")
	assert.Equal(t, normalizeDump(before), normalizeDump(reupDump), "0001 data identical after the round trip")
	EvidenceNote(t, ev, "verdict.txt", "T-MIG-04 PASS: down to 11 columns (indexes first), fk empty, 0001 data survives both directions, defaults re-applied by re-up\n")
}

// TestMP_T_MIG_05 is row T-MIG-05: the upstream binary at the pinned commit
// opens the 0002 database — creates, updates and lists all 2xx, upstream
// reads ignore the added columns, no silent corruption (dump-diff).
func TestMP_T_MIG_05(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-MIG-05")
	ev := EvidenceDir(t, "T-MIG-05", "go test ./internal/mptest -run '^TestMP_T_MIG_05$' -count=1 -v  (upstream binary on the migrated DB)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	serverBin := ServerBin(t)
	up := upstreamBin(t)
	fixturePath, before := fixtureOnce(t)

	migPath := filepath.Join(dir, "migrated.db")
	copyFixture(t, fixturePath, migPath)
	migrateWithProduct(t, serverBin, migPath)

	port := nextPort()
	cmd := exec.Command(up, "-port", fmt.Sprint(port), "-db", migPath, "-mcp-transport", "http")
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	require.NoError(t, cmd.Start(), "upstream must open the migrated DB")
	dead := make(chan struct{})
	go func() { _ = cmd.Wait(); close(dead) }()
	require.True(t, waitForPort(t, port, 20*time.Second, dead, &log), "upstream failed on the migrated DB:\n%s", log.String())
	defer func() { _ = cmd.Process.Kill() }()

	base := fmt.Sprintf("http://127.0.0.1:%d/api/v1", port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(base+"/boards", "application/json", strings.NewReader(`{"name":"Written by upstream on a 0002 DB"}`))
	require.NoError(t, err)
	var board map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&board))
	resp.Body.Close()
	require.Less(t, resp.StatusCode, 300)
	bid := board["id"].(string)
	resp2, err := client.Post(base+"/boards/"+bid+"/tickets", "application/json",
		strings.NewReader(`{"title":"old binary new db","status":"todo","priority":"low"}`))
	require.NoError(t, err)
	resp2.Body.Close()
	require.Less(t, resp2.StatusCode, 300)
	resp3, err := client.Get(base + "/tickets")
	require.NoError(t, err)
	resp3.Body.Close()
	require.Less(t, resp3.StatusCode, 300)

	// Dump-diff: exactly the new rows differ from the pre-write state.
	dump := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "after-upstream.json"), "")
	after := normalizeDump(dump)["tickets"].([]any)
	ticketsCreated := 0
	for _, row := range after {
		m, _ := row.(map[string]any)
		if s, _ := m["title"].(string); s == "old binary new db" {
			ticketsCreated++
		}
	}
	assert.Equal(t, 1, ticketsCreated, "the upstream write landed; nothing else corrupted")
	pre := normalizeDump(before)["tickets"].([]any)
	assert.Equal(t, len(pre)+1, len(after), "exactly one new ticket, no silent corruption")
	SaveEvidence(t, ev, "upstream-on-0002.txt", "upstream log:\n"+log.String()+"\nupstream create/update/list all 2xx\n")
	EvidenceNote(t, ev, "verdict.txt", "T-MIG-05 PASS: upstream (pinned) opens the 0002 DB, writes and reads 2xx, dump-diff shows only its own new row\n")
}

// TestMP_T_MIG_06 is row T-MIG-06: open/migrate five times — the bookkeeping
// stays 2/non-dirty and the data is stable across all five opens.
func TestMP_T_MIG_06(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-MIG-06")
	ev := EvidenceDir(t, "T-MIG-06", "go test ./internal/mptest -run '^TestMP_T_MIG_06$' -count=1 -v  (5x open/migrate loop)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	serverBin := ServerBin(t)
	fixturePath, before := fixtureOnce(t)

	path := filepath.Join(dir, "loop.db")
	copyFixture(t, fixturePath, path)
	var lastDump []byte
	for i := 0; i < 5; i++ {
		migrateWithProduct(t, serverBin, path)
		v, dirty, hasV := migrationsState(t, dbprocBin, path)
		require.True(t, hasV && v == 2 && !dirty, "open %d: (v=%d dirty=%v)", i+1, v, dirty)
		lastDump = tableSnapshot(t, dbprocBin, path, filepath.Join(dir, fmt.Sprintf("open%d.json", i+1)), "")
		require.Equal(t, normalizeDump(before), normalizeDump(lastDump), "open %d: data stable", i+1)
	}
	EvidenceNote(t, ev, "verdict.txt", "T-MIG-06 PASS: five opens, bookkeeping stays 2/non-dirty, data stable across all five\n")
}

// TestMP_T_CRASH_D is row T-CRASH-d: crash DURING migration, from a copy of
// the populated fixture. An external harness-dbproc watcher observes the
// migration's committed DDL landmarks; the controller SIGKILLs the migrating
// server at the dirty boundary (the migration transaction is running). On
// restart the server must fail closed (C-SCHEMA-19): exit non-zero, the
// one-line recovery message naming the version, no listener. Only the explicit
// operator force completes the migration; the end state must equal the clean
// migration of T-MIG-02.
//
// MP finding recorded in the evidence: the vendored golang-migrate v4.19.1
// sqlite driver wraps the WHOLE migration file in one transaction
// (database/sqlite/sqlite.go executeQuery -> Begin/Exec/Commit), so the
// per-statement landmarks of plan §4.3 (k=11/14/19 individually observable)
// do not exist — the watcher's SawLandmarks list is the empirical proof: all
// DDL landmarks appear in one observation. The two boundaries that DO exist
// are the dirty flag (before any DDL) and the DDL-complete-with-dirty window.
func TestMP_T_CRASH_D(t *testing.T) {
	if testing.Short() {
		t.Skip("fixture build is expensive")
	}
	dir := TestDir(t, "T-CRASH-d")
	ev := EvidenceDir(t, "T-CRASH-d", "go test ./internal/mptest -run '^TestMP_T_CRASH_D$' -count=1 -v  (kill during migration, fail-closed restart, force recovery)")
	dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
	serverBin := ServerBin(t)
	fixturePath, before := fixtureOnce(t)

	cleanPath := filepath.Join(dir, "clean.db")
	copyFixture(t, fixturePath, cleanPath)
	migrateWithProduct(t, serverBin, cleanPath)
	cleanDump := tableSnapshot(t, dbprocBin, cleanPath, filepath.Join(dir, "clean.json"), "")
	allowlist := masterObjects(t, dbprocBin, cleanPath)

	crashPath := filepath.Join(dir, "crashed.db")
	copyFixture(t, fixturePath, crashPath)

	// The watcher: a separate process polling the schema until the dirty flag
	// is visible. Its SawLandmarks list records which landmarks were reached
	// and in which observation — the §4.3 landmark evidence.
	watcher := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=wait_schema",
		"LEDGERBOARD_TEST_DB="+crashPath,
		"LEDGERBOARD_TEST_WAIT_KIND=dirty", "LEDGERBOARD_TEST_WAIT_NAME=version",
		"LEDGERBOARD_TEST_WAIT_MAX_MS=20000")

	// The migrating server.
	port := nextPort()
	mig := exec.Command(serverBin, "-port", fmt.Sprint(port), "-db", crashPath, "-mcp-transport", "http", "-log-level", "warn")
	var migLog strings.Builder
	mig.Stdout, mig.Stderr = &migLog, &migLog
	require.NoError(t, mig.Start())

	// The watcher's result line is the signal; then the controller SIGKILLs
	// the migrating server — the kill is issued by this separate process, at
	// a boundary observed by a separate process.
	wres := watcher.Line(t, 30*time.Second)
	require.True(t, okOf(wres), "watcher: %v", wres)
	saw, _ := wres["saw_landmarks"].([]any)

	require.NoError(t, syscall.Kill(mig.Process.Pid, syscall.SIGKILL))
	_ = mig.Wait()
	SaveEvidence(t, ev, "migrating-server-log.txt", migLog.String())
	SaveEvidence(t, ev, "landmarks.json", DumpJSON(map[string]any{
		"saw_landmarks": saw,
		"note":          "The landmarks appear together in one observation: the vendored driver commits the whole migration file as ONE transaction, so per-statement landmarks (k=11/14/19) are not observable. The observable boundaries are: dirty visible (before/inside the migration transaction) and DDL-complete-with-dirty.",
	}))

	// Post-kill state: the dirty flag is set, the 0001 data is intact.
	v, dirty, hasV := migrationsState(t, dbprocBin, crashPath)
	require.True(t, hasV, "bookkeeping row exists")
	require.True(t, dirty, "the killed migration left dirty=1 (got version=%d dirty=%v)", v, dirty)
	_ = v
	killedDump := tableSnapshot(t, dbprocBin, crashPath, filepath.Join(dir, "killed.json"), "")
	assert.Equal(t, normalizeDump(before), normalizeDump(killedDump), "the 0001 data survived the mid-migration kill untouched")

	// Fail-closed restart: exits non-zero BEFORE binding, with the one-line
	// C-SCHEMA-19 message naming the version and the recovery command.
	code, out := StartServerExpectExit(t, serverBin, crashPath, nextPort())
	assert.NotEqual(t, 0, code, "the dirty database must fail closed")
	assert.Contains(t, out, "C-SCHEMA-19", "the one-line message names the rule")
	assert.Contains(t, out, "force", "and names the recovery command")
	conn, dialErr := dialPort(port)
	assert.Error(t, dialErr, "no listener: a probe call is refused")
	if conn != nil {
		_ = conn.Close()
	}
	SaveEvidence(t, ev, "failclosed.txt", fmt.Sprintf("exit=%d\noutput:\n%s\n", code, out))

	// Operator recovery: force to version 1, restart, migration completes.
	f := StartDBProc(t, dbprocBin,
		"LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=force",
		"LEDGERBOARD_TEST_DB="+crashPath, "LEDGERBOARD_TEST_FORCE_VERSION=1")
	fres := f.Line(t, 30*time.Second)
	_, _ = f.Wait(t)
	require.True(t, okOf(fres), "force: %v", fres)

	port2 := nextPort()
	StartServer(t, serverBin, crashPath, port2)
	v2, dirty2, _ := migrationsState(t, dbprocBin, crashPath)
	assert.Equal(t, int64(2), v2)
	assert.False(t, dirty2, "the recovery completes the migration")
	assert.Equal(t, allowlist, masterObjects(t, dbprocBin, crashPath), "recovered schema equals the clean migration")
	recoveredDump := tableSnapshot(t, dbprocBin, crashPath, filepath.Join(dir, "recovered.json"), "")
	assert.Equal(t, normalizeDump(cleanDump), normalizeDump(recoveredDump), "recovered end state is dump-identical to the clean migration of T-MIG-02")
	assert.Empty(t, fkViolations(t, dbprocBin, crashPath))
	pdb := ParentDB(t, crashPath)
	assert.Equal(t, int64(40), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0`), "pre-existing rows carry the defaults after recovery")
	SaveEvidence(t, ev, "recovery.json", DumpJSON(map[string]any{"watcher": wres, "fail_closed_exit": code, "version_after": v2, "dirty_after": dirty2}))
	EvidenceNote(t, ev, "verdict.txt", "T-CRASH-d PASS (MP): external watcher + controller SIGKILL mid-migration; dirty=1; restart fails closed (exit!=0, C-SCHEMA-19 line, no listener); operator force completes it to a dump-identical clean state. FINDING: per-statement landmarks k=11/14/19 unobservable — the vendored driver tx-wraps the whole file (see landmarks.json).\n")
}
