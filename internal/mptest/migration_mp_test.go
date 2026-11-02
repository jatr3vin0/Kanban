// The genuinely upstream-populated migration fixture (docs/test-plan.md §4.1)
// and the migration MP rows of slice 4: T-MIG-01 (fixture), T-MIG-02 (migrate
// in place + concurrent server starts), T-MIG-04 (down round trip), T-MIG-05
// (old binary on new DB), T-MIG-06 (no-surprises open loop) and T-CRASH-d
// (crash during migration, C-SCHEMA-19 fail-closed + force recovery).
package mptest

import (
    "bytes"
    "database/sql"
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

    "github.com/golang-migrate/migrate/v4"
    msqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
    "github.com/golang-migrate/migrate/v4/source/iofs"
    "github.com/rgracey/kanban-mcp/internal/db"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// UpstreamPin is the fork point the fixture must be populated from (§4.1).
const UpstreamPin = "c6938a2e6e0f3b2aca6cebf4afd93babd94d8317"

// nextPort returns a candidate port in a band derived from this process's PID,
// so two suites running concurrently on this shared box never hand out the
// same numbers. The old fixed 18401-18700 range made every mptest process on
// the machine use identical ports, and T-CRASH-d's post-kill "no listener"
// probe then raced a CONCURRENT suite's short-lived server: reproduced as
// PASS/PASS/FAIL under -count=3 while another session ran the same row (run
// 8's sole failed assertion). 48 disjoint 300-port bands stay below the
// kernel's ephemeral range (32768+).
func nextPort() int { return 18000 + (os.Getpid()%48)*300 + int(fifoSeq.Add(1))%300 }

// claimPort returns a nextPort() candidate that verifiably refuses connections
// right now. T-CRASH-d asserts the ABSENCE of a listener on the killed
// migrating server's port; on a shared box a foreign listener in the band
// (a non-suite service, or a suite whose PID band collides with ours) turns
// that assertion into a false failure — the killed server never binds its port
// (it dies mid-migration), so a foreign occupant is indistinguishable from a
// surviving server (run 8's failed dial). Stepping over occupied candidates
// keeps the probe meaningful; the post-kill assertion itself is unchanged.
func claimPort(t *testing.T) int {
    t.Helper()
    for i := 0; i < 300; i++ {
        p := nextPort()
        conn, err := dialPort(p)
        if conn != nil {
            _ = conn.Close()
        }
        if err != nil {
            return p // refused: nothing listens, the port is ours to use
        }
    }
    t.Fatalf("claimPort: all 300 candidate ports are occupied — a process outside this suite is squatting on this band; environment collision, not product behavior")
    return 0
}

// migrateDownOne runs one golang-migrate Down step against dbPath, sourcing
// the migrations from the repo's own directory (never a copy) — the operator
// CLI equivalent, from the parent's own process and connection.
func migrateDownOne(t *testing.T, dbPath string) {
    t.Helper()
    dbh, err := sql.Open("sqlite", db.DSN(dbPath))
    require.NoError(t, err)
    defer dbh.Close()
    src, err := iofs.New(os.DirFS(AbsFromRepo(t, "internal/db/migrations")), ".")
    require.NoError(t, err)
    defer func() { _ = src.Close() }()
    drv, err := msqlite.WithInstance(dbh, &msqlite.Config{MigrationsTable: "schema_migrations"})
    require.NoError(t, err)
    m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
    require.NoError(t, err)
    require.NoError(t, m.Steps(-1), "down one step (v2 -> v1)")
}

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
    git("worktree", "prune") // a stale registration from an interrupted run makes `worktree add` fail (observed run 4/5: T-MIG-04 at 0.3s)
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
// harness-dbproc and returns the bytes. tables == "" means "all tables" via
// the explicit "*" sentinel (review §3.2(3): getenv cannot distinguish an
// empty value from an unset one, so the sentinel is the only all-tables form).
// A dump that proves nothing fails HERE, with a message that names the
// problem — never downstream as a phantom data diff (review §3.2(2)).
func tableSnapshot(t *testing.T, dbprocBin, dbPath, outPath, tables string) []byte {
    t.Helper()
    spec := tables
    if spec == "" {
        spec = "*"
    }
    d := StartDBProc(t, dbprocBin,
        "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=dump",
        "LEDGERBOARD_TEST_DB="+dbPath, "LEDGERBOARD_TEST_OUT="+outPath,
        "LEDGERBOARD_TEST_TABLES="+spec)
    res := d.Line(t, 30*time.Second)
    _, _ = d.Wait(t)
    require.True(t, okOf(res), "dump: %v", res)
    tabs, ok := res["tables"].(map[string]any)
    require.True(t, ok, "dump result carries no tables map — a dump that proves nothing must fail at the dump: %v", res)
    require.NotEmpty(t, tabs, "dump reports zero tables — nothing to compare")
    raw, err := os.ReadFile(outPath)
    require.NoError(t, err)
    require.NotEmpty(t, raw, "dump OUT file %s is empty", outPath)
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
    VerdictNote(t, ev, "T-MIG-01 PASS: upstream-populated fixture (pinned "+UpstreamPin[:12]+"), 3 boards/3 epics/40 tickets/25 notes/30 tasks/10 relations + oddities; before.json is the baseline\n")
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
    bb, aa := project0001(t, before, after)
    assert.Equal(t, bb, aa, "all 0001 tables byte-identical (C-COMPAT-4)")

    // Second open is a no-op.
    migrateWithProduct(t, serverBin, migPath)
    v2, dirty2, _ := migrationsState(t, dbprocBin, migPath)
    assert.Equal(t, int64(2), v2)
    assert.False(t, dirty2)
    after2 := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "after2.json"), "")
    assert.Equal(t, normalizeDump(t, after), normalizeDump(t, after2), "second open changed nothing")

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
    bb, aa = project0001(t, before, concDump)
    assert.Equal(t, bb, aa, "concurrent starts: 0001 data intact (no torn state)")
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
        assert.Equal(t, normalizeDump(t, after), normalizeDump(t, recovered), "recovered state equals the clean migration")
        recoveryNote = "concurrent starts left dirty=1; fail-closed restart + force recovered to the clean state"
    } else {
        assert.Equal(t, int64(2), vc, "healthy migration outcome")
        recoveryNote = "concurrent starts serialized to a clean migration"
    }
    SaveEvidence(t, ev, "concurrent-outcomes.json", DumpJSON(map[string]any{
        "outcomes": outcomes, "version": vc, "dirty": dirtyc, "note": recoveryNote,
    }))
    VerdictNote(t, ev, "T-MIG-02 PASS: allowlist schema, defaults on 40 rows, 0001 dumps identical, fk empty, second open no-op; concurrent starts left no torn state ("+recoveryNote+")\n")
}

// project0001 restricts the post-migration dump to the tables and row columns
// that exist in the 0001 fixture dump. "The 0001 data is intact" (C-COMPAT-4)
// compares DATA, not the additive schema: 0002 adds three empty tables and
// eight ticket columns, and counting them as differences fails every healthy
// migration (observed in the first full run: tables 7 vs 10).
//
// Run-4 defect (review §3): normalizeDump returns the dbproc RESULT LINE
// (op/ok/tables) and the old project0001 iterated its TOP-LEVEL keys — so the
// "tables" key was skipped (its value is a map of tables, not a row list) and
// the projection was ALWAYS empty, even for two healthy dumps. It now
// descends into "tables" on both sides.
func project0001(t *testing.T, before, after any) (map[string]any, map[string]any) {
    b := tablesOf(t, normalizeDump(t, before))
    a := tablesOf(t, normalizeDump(t, after))
    proj := map[string]any{}
    for name, rows := range a {
        brows, ok := b[name].([]any)
        if !ok {
            continue // a table 0002 added
        }
        if len(brows) == 0 {
            proj[name] = brows
            continue
        }
        keys := map[string]bool{}
        for k := range brows[0].(map[string]any) {
            keys[k] = true
        }
        arows, _ := rows.([]any)
        out := make([]any, 0, len(arows))
        for _, r := range arows {
            rm, _ := r.(map[string]any)
            nr := map[string]any{}
            for k, v := range rm {
                if keys[k] {
                    nr[k] = v
                }
            }
            out = append(out, nr)
        }
        proj[name] = out
    }
    return b, proj
}

// normalizeDump parses a dbproc dump file into its result-line map so two
// snapshots compare structurally (JSON object key order is not guaranteed).
// Unparseable or empty input is a HARNESS failure with a named message —
// never an empty map swallowed into a phantom data diff (review §3.2(2)).
func normalizeDump(t *testing.T, raw any) map[string]any {
    t.Helper()
    var b []byte
    switch v := raw.(type) {
    case string:
        var err error
        b, err = os.ReadFile(v)
        if err != nil {
            t.Fatalf("dump file %s unreadable: %v", v, err)
        }
    case []byte:
        b = v
    default:
        t.Fatalf("normalizeDump: unsupported input %T", raw)
    }
    var m map[string]any
    if err := json.Unmarshal(b, &m); err != nil {
        t.Fatalf("dump unparseable (%d bytes): %v", len(b), err)
    }
    if m == nil || len(m) == 0 {
        t.Fatalf("dump parsed to an empty object (%d bytes) — not a successful dump", len(b))
    }
    return m
}

// tablesOf extracts the dump's tables map, failing loudly when absent.
func tablesOf(t *testing.T, m map[string]any) map[string]any {
    t.Helper()
    tabs, ok := m["tables"].(map[string]any)
    if !ok {
        t.Fatalf("dump result has no tables map (keys: %v) — a dump that proves nothing must fail at the dump", keysOf(m))
    }
    return tabs
}

func keysOf(m map[string]any) []string {
    out := make([]string, 0, len(m))
    for k := range m {
        out = append(out, k)
    }
    sortStrings(out)
    return out
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
    bProj, aProj := project0001(t, before, upDump)
    assert.Equal(t, bProj, aProj, "pre-down: 0001 data intact")

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
    bProj, aProj = project0001(t, before, downDump)
    assert.Equal(t, bProj, aProj, "0001 data survives the down migration")

    // The contracted down text touches schema objects only; the bookkeeping
    // row still claims version 2 — so a re-open would be a golang-migrate
    // no-op and the defaults below would be asserted against a table without
    // its lease columns (observed run 5). Step the bookkeeping down the way
    // golang-migrate's Down() would: the operator recovery path, force 1.
    fdown := StartDBProc(t, dbprocBin,
        "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=force",
        "LEDGERBOARD_TEST_DB="+migPath, "LEDGERBOARD_TEST_FORCE_VERSION=1")
    fdownRes := fdown.Line(t, 30*time.Second)
    _, _ = fdown.Wait(t)
    require.True(t, okOf(fdownRes), "down-step bookkeeping to version 1: %v", fdownRes)

    // Up again: defaults re-applied, data still identical.
    migrateWithProduct(t, serverBin, migPath)
    v, dirty, hasV := migrationsState(t, dbprocBin, migPath)
    assert.True(t, hasV && v == 2 && !dirty)
    assert.Equal(t, int64(40), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0`), "defaults re-applied on up")
    reupDump := tableSnapshot(t, dbprocBin, migPath, filepath.Join(dir, "reup.json"), "")
    bProj, aProj = project0001(t, before, reupDump)
    assert.Equal(t, bProj, aProj, "0001 data identical after the round trip")
    VerdictNote(t, ev, "T-MIG-04 PASS: down to 11 columns (indexes first), fk empty, 0001 data survives both directions, defaults re-applied by re-up\n")
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
    after := tablesOf(t, normalizeDump(t, dump))["tickets"].([]any)
    ticketsCreated := 0
    for _, row := range after {
        m, _ := row.(map[string]any)
        if s, _ := m["title"].(string); s == "old binary new db" {
            ticketsCreated++
        }
    }
    assert.Equal(t, 1, ticketsCreated, "the upstream write landed; nothing else corrupted")
    pre := tablesOf(t, normalizeDump(t, before))["tickets"].([]any)
    assert.Equal(t, len(pre)+1, len(after), "exactly one new ticket, no silent corruption")
    SaveEvidence(t, ev, "upstream-on-0002.txt", "upstream log:\n"+log.String()+"\nupstream create/update/list all 2xx\n")
    VerdictNote(t, ev, "T-MIG-05 PASS: upstream (pinned) opens the 0002 DB, writes and reads 2xx, dump-diff shows only its own new row\n")
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
        bProj, aProj := project0001(t, before, lastDump)
        require.Equal(t, bProj, aProj, "open %d: data stable", i+1)
    }
    VerdictNote(t, ev, "T-MIG-06 PASS: five opens, bookkeeping stays 2/non-dirty, data stable across all five\n")
}

// TestMP_T_CRASH_D is row T-CRASH-d: crash DURING migration, from a copy of
// the populated fixture, at the two boundaries that actually exist for the
// vendored runner (finding A12, accepted by the design review §1.4). The
// golang-migrate v4.19.1 sqlite driver tx-wraps the WHOLE migration file in
// one Begin/Exec/Commit (database/sqlite/sqlite.go executeQuery), so SQLite
// DDL never shows per-statement committed state — the §4.3 per-statement
// landmarks of the original plan do not exist (empirical proof: landmarks.json
// saw one observation). The row instead verifies:
//
//   - B1 (the kill): the migrating server is SIGKILLed at the dirty boundary —
//     an external harness-dbproc watcher observes schema_migrations.dirty=1
//     from a separate process, and the controller (this test) issues the kill.
//     Restart must fail closed (C-SCHEMA-19): exit non-zero before the
//     listener binds, the one-line message naming the version and the remedy,
//     probe refused. Recovery: operator `force 1` (B1's target: 0002 has not
//     applied), restart, migration completes to a dump-identical clean state.
//   - B2 (the constructed state): the window between the migration
//     transaction's commit and the bookkeeping update cannot be hit by an
//     external poller (microseconds, same process) — so it is CONSTRUCTED, not
//     raced: a clean-migrated copy gets schema_migrations.dirty=1 by direct
//     SQL through harness-dbproc. The identical fail-closed contract applies;
//     recovery is `force 2` (B2's target: the DDL is already on disk), and the
//     end state is again dump-identical to the clean migration.
//
// B1 is the row's only kill point; B2 is a state, deterministically set. Both
// record which boundary was reached in recovery.json.
func TestMP_T_CRASH_D(t *testing.T) {
    if testing.Short() {
        t.Skip("fixture build is expensive")
    }
    dir := TestDir(t, "T-CRASH-d")
    ev := EvidenceDir(t, "T-CRASH-d", "go test ./internal/mptest -run '^TestMP_T_CRASH_D$' -count=1 -v  (B1: kill at the dirty boundary + force 1; B2: constructed dirty state + force 2)")
    dbprocBin := BuildOnce(t, "internal/harness/dbproc", "harness-dbproc")
    serverBin := ServerBin(t)
    fixturePath, before := fixtureOnce(t)

    cleanPath := filepath.Join(dir, "clean.db")
    copyFixture(t, fixturePath, cleanPath)
    migrateWithProduct(t, serverBin, cleanPath)
    cleanDump := tableSnapshot(t, dbprocBin, cleanPath, filepath.Join(dir, "clean.json"), "")
    allowlist := masterObjects(t, dbprocBin, cleanPath)

    // ---- B1: the dirty-boundary kill ---------------------------------------
    // The watcher must have POLLED before the server can start, or a fast
    // migration completes before the watcher's first observation and the leg
    // times out with only end-state landmarks (run 6, loaded box: the watcher's
    // own process startup lost the race). The watcher therefore announces its
    // first poll with an `attached` line and the server starts only after it.
    // If a watch still misses the window under extreme scheduling pressure, the
    // leg retries on a fresh fixture copy — every assertion below is unchanged;
    // only the observation re-runs, and every miss is recorded as evidence.
    var (
        wres   map[string]any
        saw    []any
        migLog strings.Builder
        port   int
    )
    var crashPath string
    b1Attempts := 0
    for attempt := 1; attempt <= 5; attempt++ {
        b1Attempts = attempt
        crashPath = filepath.Join(dir, fmt.Sprintf("crashed-%d.db", attempt))
        copyFixture(t, fixturePath, crashPath)

        // The watcher: a separate process polling the schema until the dirty flag
        // is visible. Its SawLandmarks list records which landmarks were reached
        // and in which observation — the A12 evidence that per-statement landmarks
        // are not observable (all DDL appears in one observation or not at all).
        watcher := StartDBProc(t, dbprocBin,
            "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=wait_schema",
            "LEDGERBOARD_TEST_DB="+crashPath,
            "LEDGERBOARD_TEST_WAIT_KIND=dirty", "LEDGERBOARD_TEST_WAIT_NAME=version",
            "LEDGERBOARD_TEST_WAIT_MAX_MS=5000")

        // Sequencing point: the watcher's first poll has happened once its
        // `attached` line is read; only then does the migrating server start.
        attached := watcher.Line(t, 30*time.Second)
        require.True(t, okOf(attached), "watcher attach (attempt %d): %v", attempt, attached)
        require.Equal(t, "attached", attached["phase"], "the watcher must announce its first poll before the server starts")

        // The migrating server. claimPort, not nextPort: this row
        // later asserts the port REFUSES connections, so it must
        // start from a port that refuses them now (see claimPort).
        port = claimPort(t)
        mig := exec.Command(serverBin, "-port", fmt.Sprint(port), "-db", crashPath, "-mcp-transport", "http", "-log-level", "warn")
        migLog.Reset()
        mig.Stdout, mig.Stderr = &migLog, &migLog
        require.NoError(t, mig.Start())

        // The watcher's result line is the signal; then the controller SIGKILLs
        // the migrating server — the kill is issued by this separate process, at
        // a boundary observed by a separate process. On a missed window the kill
        // still happens (the server has long finished migrating and is serving).
        wres = watcher.Line(t, 30*time.Second)
        require.NoError(t, syscall.Kill(mig.Process.Pid, syscall.SIGKILL))
        _ = mig.Wait()
        if okOf(wres) {
            saw, _ = wres["saw_landmarks"].([]any)
            SaveEvidence(t, ev, "migrating-server-log.txt", migLog.String())
            break
        }
        SaveEvidence(t, ev, fmt.Sprintf("b1-miss-%d.json", attempt), DumpJSON(wres))
    }
    require.True(t, okOf(wres), "watcher: %v (across %d attempts — the dirty window was never observed; treat as a real finding if it persists)", wres, b1Attempts)
    SaveEvidence(t, ev, "landmarks.json", DumpJSON(map[string]any{
        "saw_landmarks": saw,
        "b1_attempts":   b1Attempts,
        "note":          "A12 finding (accepted): the landmarks appear together in one observation — the vendored driver commits the whole migration file as ONE transaction, so per-statement landmarks (k=1..20) are not observable. The observable boundaries are the dirty window (B1, killed here) and DDL-complete-with-dirty (B2, constructed below); §4.3 was amended accordingly. b1_attempts records how many watch attempts the dirty window needed (the watcher attaches before the server starts; a miss means the window was missed under scheduling pressure, not that it does not exist).",
    }))

    // Post-kill state: the dirty flag is set, the 0001 data is intact.
    v, dirty, hasV := migrationsState(t, dbprocBin, crashPath)
    require.True(t, hasV, "bookkeeping row exists")
    require.True(t, dirty, "the killed migration left dirty=1 (got version=%d dirty=%v)", v, dirty)
    killedDump := tableSnapshot(t, dbprocBin, crashPath, filepath.Join(dir, "killed.json"), "")
    bb, aa := project0001(t, before, killedDump)
    assert.Equal(t, bb, aa, "the 0001 data survived the mid-migration kill untouched")

    // Fail-closed restart: exits non-zero BEFORE binding, with the one-line
    // C-SCHEMA-19 message naming the version and the recovery command.
    code, out := StartServerExpectExit(t, serverBin, crashPath, nextPort())
    assert.NotEqual(t, 0, code, "the dirty database must fail closed")
    assert.Contains(t, out, "C-SCHEMA-19", "the one-line message names the rule")
    assert.Contains(t, out, "force", "and names the recovery command")
    conn, dialErr := dialPort(port)
    assert.Error(t, dialErr, "no listener: a probe call is refused (the port verified free before the server started; a success here means the server survived the kill or a foreign listener took the port in between)")
    if conn != nil {
        _ = conn.Close()
    }
    SaveEvidence(t, ev, "failclosed.txt", fmt.Sprintf("B1 exit=%d\noutput:\n%s\n", code, out))

    // Operator recovery at B1: force to version 1 (0002 has not applied here —
    // force 2 would re-run an applied file), restart, migration completes.
    f := StartDBProc(t, dbprocBin,
        "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=force",
        "LEDGERBOARD_TEST_DB="+crashPath, "LEDGERBOARD_TEST_FORCE_VERSION=1")
    fres := f.Line(t, 30*time.Second)
    _, _ = f.Wait(t)
    require.True(t, okOf(fres), "force 1: %v", fres)

    port2 := nextPort()
    StartServer(t, serverBin, crashPath, port2)
    v2, dirty2, _ := migrationsState(t, dbprocBin, crashPath)
    assert.Equal(t, int64(2), v2)
    assert.False(t, dirty2, "the recovery completes the migration")
    assert.Equal(t, allowlist, masterObjects(t, dbprocBin, crashPath), "recovered schema equals the clean migration")
    recoveredDump := tableSnapshot(t, dbprocBin, crashPath, filepath.Join(dir, "recovered.json"), "")
    assert.Equal(t, normalizeDump(t, cleanDump), normalizeDump(t, recoveredDump), "B1 recovered end state is dump-identical to the clean migration of T-MIG-02")
    assert.Empty(t, fkViolations(t, dbprocBin, crashPath))
    pdb := ParentDB(t, crashPath)
    assert.Equal(t, int64(40), pdb.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0`), "pre-existing rows carry the defaults after B1 recovery")

    // ---- B2: the constructed dirty state -----------------------------------
    // The DDL-complete-with-dirty window lives for microseconds inside the
    // migrating process (review §1.4: an external poller would hit it ~1 time
    // in 20 — a flake generator). B2 is a STATE, so construct it deterministi-
    // cally: clean-migrate a fresh copy, then set dirty=1 by direct SQL through
    // harness-dbproc (the operator CLI's own access path, production DSN).
    b2Path := filepath.Join(dir, "b2-dirty.db")
    copyFixture(t, fixturePath, b2Path)
    migrateWithProduct(t, serverBin, b2Path)
    vb, dirtyb, _ := migrationsState(t, dbprocBin, b2Path)
    require.Equal(t, int64(2), vb)
    require.False(t, dirtyb, "B2 starts from a CLEAN migration")

    s := StartDBProc(t, dbprocBin,
        "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=sql",
        "LEDGERBOARD_TEST_DB="+b2Path,
        "LEDGERBOARD_TEST_SQL=UPDATE schema_migrations SET dirty = 1")
    sres := s.Line(t, 30*time.Second)
    _, _ = s.Wait(t)
    require.True(t, okOf(sres), "set dirty=1: %v", sres)
    _, dirtyAfter, _ := migrationsState(t, dbprocBin, b2Path)
    require.True(t, dirtyAfter, "B2 constructed: DDL complete, bookkeeping dirty")

    // The identical fail-closed contract at B2. The probe dials the port the
    // fail-closed start was GIVEN (not any earlier leg's port — B1's recovered
    // server is still listening on its own port at this point).
    probePort := nextPort()
    code2, out2 := StartServerExpectExit(t, serverBin, b2Path, probePort)
    assert.NotEqual(t, 0, code2, "B2: the dirty database must fail closed")
    assert.Contains(t, out2, "C-SCHEMA-19", "B2: the one-line message names the rule")
    assert.Contains(t, out2, "force", "B2: and names the recovery command")
    conn2, dialErr2 := dialPort(probePort)
    assert.Error(t, dialErr2, "B2: no listener: a probe call is refused")
    if conn2 != nil {
        _ = conn2.Close()
    }
    SaveEvidence(t, ev, "failclosed-b2.txt", fmt.Sprintf("B2 exit=%d\noutput:\n%s\n", code2, out2))

    // Operator recovery at B2: force to version 2 — the DDL is ALREADY on disk
    // (force 1 here would tell the runner 0002 is unapplied and the restart
    // would fail on `duplicate column name`). Then the restart is a no-op.
    f2 := StartDBProc(t, dbprocBin,
        "LEDGERBOARD_TEST_DBPROC=1", "LEDGERBOARD_TEST_OP=force",
        "LEDGERBOARD_TEST_DB="+b2Path, "LEDGERBOARD_TEST_FORCE_VERSION=2")
    fres2 := f2.Line(t, 30*time.Second)
    _, _ = f2.Wait(t)
    require.True(t, okOf(fres2), "force 2: %v", fres2)

    StartServer(t, serverBin, b2Path, nextPort())
    vb2, dirtyb2, _ := migrationsState(t, dbprocBin, b2Path)
    assert.Equal(t, int64(2), vb2)
    assert.False(t, dirtyb2, "B2 recovery lands on a healthy file")
    assert.Equal(t, allowlist, masterObjects(t, dbprocBin, b2Path), "B2 recovered schema equals the clean migration")
    b2Dump := tableSnapshot(t, dbprocBin, b2Path, filepath.Join(dir, "b2-recovered.json"), "")
    assert.Equal(t, normalizeDump(t, cleanDump), normalizeDump(t, b2Dump), "B2 recovered end state is dump-identical to the clean migration of T-MIG-02")
    assert.Empty(t, fkViolations(t, dbprocBin, b2Path))
    pdb2 := ParentDB(t, b2Path)
    assert.Equal(t, int64(40), pdb2.Int(t, `SELECT COUNT(*) FROM tickets WHERE version=1 AND lease_generation=0`), "pre-existing rows carry the defaults after B2 recovery")

    SaveEvidence(t, ev, "recovery.json", DumpJSON(map[string]any{
        "b1_dirty_boundary_kill": map[string]any{"watcher": wres, "fail_closed_exit": code, "force": 1, "version_after": v2, "dirty_after": dirty2},
        "b2_constructed_state":   map[string]any{"fail_closed_exit": code2, "force": 2, "version_after": vb2, "dirty_after": dirtyb2},
        "boundary_rule":          "B1 is the row's only kill point; B2 is a constructed state (dirty=1 on a clean-migrated copy). B1 recovers by force 1, B2 by force 2.",
    }))
    VerdictNote(t, ev, "T-CRASH-d PASS (MP, A12 design): B1 — external watcher + controller SIGKILL at the dirty boundary, dirty=1, restart fails closed (exit!=0, C-SCHEMA-19 line, no listener), force 1 completes it dump-identical to the clean migration. B2 — constructed state (dirty=1 on a clean-migrated copy), same fail-closed contract, force 2 recovers dump-identical. Per-statement landmarks are unobservable (vendored driver tx-wraps the file — landmarks.json); §4.3 amended (finding-A12.md).\n")
}
