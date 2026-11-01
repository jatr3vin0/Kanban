// Package mptest is the multi-process test harness of docs/test-plan.md
// §1.5–§1.7: it spawns real OS processes (harness-agent, harness-dbproc and the
// server binary) against one SQLite file, synchronises them through FIFOs and
// the fake-clock file, arms seams by environment, captures evidence under
// docs/test-results/, and tears everything down.
//
// This package is test infrastructure, not product: it is imported only by
// _test.go files (never shipped), and it writes nothing outside
// /tmp/ledgerboard-scratch and docs/test-results.
package mptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ScratchRoot is where every scratch DB, binary and FIFO lives (§1: never
// inside /home, never committed).
const ScratchRoot = "/tmp/ledgerboard-scratch"

// BinDir is the shared binary cache for the harness children.
const BinDir = ScratchRoot + "/bin"

// KeepDir creates a scratch directory that survives the test (the shared
// migration fixture, which later rows reuse instead of rebuilding).
func KeepDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(ScratchRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	return dir
}

// TestDir returns (and creates) the scratch directory of one test row.
func TestDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(ScratchRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

var (
	buildMu    sync.Mutex
	buildOnce  map[string]*sync.Once
	buildCache = map[string]string{}
)

// BuildOnce compiles a package into BinDir/name at most once per test run.
// Binaries are built out of tree into /tmp (never committed, WORKFLOW.md).
func BuildOnce(t *testing.T, pkgPath, name string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildOnce == nil {
		buildOnce = map[string]*sync.Once{}
	}
	if _, ok := buildOnce[pkgPath]; !ok {
		buildOnce[pkgPath] = &sync.Once{}
	}
	out := filepath.Join(BinDir, name)
	var err error
	buildOnce[pkgPath].Do(func() {
		if err = os.MkdirAll(BinDir, 0o755); err != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", out, "./"+pkgPath)
		cmd.Dir = RepoRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, e := cmd.CombinedOutput(); e != nil {
			err = fmt.Errorf("go build %s: %v: %s", pkgPath, e, b)
		}
	})
	if err != nil {
		t.Fatalf("build %s: %v", pkgPath, err)
	}
	buildCache[pkgPath] = out
	return out
}

// ServerBin returns the path of the built product binary.
func ServerBin(t *testing.T) string {
	t.Helper()
	out := filepath.Join(BinDir, "ledgerboard")
	var err error
	buildMu.Lock()
	defer buildMu.Unlock()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = RepoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, e := cmd.CombinedOutput(); e != nil {
		err = fmt.Errorf("go build server: %v: %s", e, b)
	}
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	return out
}

// RepoRoot is the repository root (the test runs from internal/mptest).
func RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// AbsFromRepo resolves a repo-relative path.
func AbsFromRepo(t *testing.T, rel string) string {
	return filepath.Join(RepoRoot(t), rel)
}

// ---- fake clock ------------------------------------------------------------

// ClockPath returns the clock-file path inside dir and seeds it with nowMS.
func ClockPath(t *testing.T, dir string, nowMS int64) string {
	t.Helper()
	p := filepath.Join(dir, "clock.txt")
	WriteClockMS(t, p, nowMS)
	return p
}

// WriteClockMS advances the shared fake clock (§1.3): a write, not a wait.
func WriteClockMS(t *testing.T, path string, ms int64) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.FormatInt(ms, 10)), 0o600); err != nil {
		t.Fatalf("write clock: %v", err)
	}
}

// ---- FIFOs -----------------------------------------------------------------

// MakeFIFO creates one named FIFO in dir.
func MakeFIFO(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", p, err)
	}
	return p
}

// ReadFIFO blocks until a writer delivers one line, with a timeout.
func ReadFIFO(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	lines := make(chan string, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			lines <- ""
			return
		}
		defer f.Close()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		lines <- string(buf[:n])
	}()
	select {
	case l := <-lines:
		return l
	case <-time.After(timeout):
		t.Fatalf("no FIFO line within %v: %s", timeout, path)
		return ""
	}
}

// ---- child processes --------------------------------------------------------

// Child is one spawned harness process.
type Child struct {
	cmd       *exec.Cmd
	stdout    *bufio.Reader
	pipe      *os.File
	outBuf    *strings.Builder
	outMu     *sync.Mutex
	waitOnce  sync.Once
	waitErr   error
	waitState *string
}

// StartAgent spawns one harness-agent. env is appended over os.Environ().
func StartAgent(t *testing.T, bin string, env ...string) *Child {
	return startChild(t, bin, env...)
}

// StartDBProc spawns one harness-dbproc.
func StartDBProc(t *testing.T, bin string, env ...string) *Child {
	return startChild(t, bin, env...)
}

func startChild(t *testing.T, bin string, env ...string) *Child {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	c := &Child{cmd: cmd, pipe: pw, stdout: bufio.NewReader(pr), outBuf: &strings.Builder{}, outMu: &sync.Mutex{}}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		if c.cmd.ProcessState == nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
		_ = pw.Close()
		_ = pr.Close()
	})
	return c
}

// Line reads one JSON result line (blocking, with timeout). The line is both
// the op result and part of the process transcript.
func (c *Child) Line(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	type r struct {
		ch chan string
	}
	ch := make(chan string, 1)
	go func() {
		line, err := c.stdout.ReadString('\n')
		if err != nil && line == "" {
			ch <- ""
			return
		}
		ch <- line
	}()
	select {
	case line := <-ch:
		if line == "" {
			t.Fatalf("child exited before delivering a result line; output so far:\n%s", c.Transcript())
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad result line %q: %v", line, err)
		}
		c.outMu.Lock()
		c.outBuf.WriteString(line)
		c.outMu.Unlock()
		return m
	case <-time.After(timeout):
		t.Fatalf("no result line within %v; output:\n%s", timeout, c.Transcript())
		return nil
	}
}

// Transcript returns everything the child has written so far.
func (c *Child) Transcript() string {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return c.outBuf.String()
}

// Wait reaps the child exactly once and returns its exit state.
func (c *Child) Wait(t *testing.T) (*os.ProcessState, error) {
	c.waitOnce.Do(func() {
		c.waitErr = c.cmd.Wait()
	})
	return c.cmd.ProcessState, c.waitErr
}

// Kill SIGKILLs the child (the crash issued by the controller process).
func (c *Child) Kill() error {
	return syscall.Kill(c.cmd.Process.Pid, syscall.SIGKILL)
}

// ---- server binary ----------------------------------------------------------

// StartServer starts the product binary against dbPath on port, waiting for
// the listener by dialling (a readiness poll of a process we do not own the
// logging of; every owned child uses FIFOs instead). It returns the cmd.
func StartServer(t *testing.T, bin, dbPath string, port int, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "-port", strconv.Itoa(port), "-db", dbPath, "-mcp-transport", "http", "-log-level", "warn")
	cmd.Env = append(os.Environ(), env...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	dead := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(dead)
	}()
	if !waitForPort(t, port, 15*time.Second, dead, &buf) {
		_ = cmd.Process.Kill()
		t.Fatalf("server did not bind :%d\noutput:\n%s", port, buf.String())
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-dead:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	})
	return cmd
}

// StartServerExpectExit starts the product binary and waits for it to exit,
// returning its exit code and full output — the fail-closed probe of
// C-SCHEMA-19 (exit non-zero before the listener binds).
func StartServerExpectExit(t *testing.T, bin, dbPath string, port int, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, "-port", strconv.Itoa(port), "-db", dbPath, "-mcp-transport", "http", "-log-level", "warn")
	cmd.Env = append(os.Environ(), env...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("server still running after 30s (expected a fail-closed exit)\noutput:\n%s", buf.String())
	}
	code := 0
	if cmd.ProcessState.Exited() {
		code = cmd.ProcessState.ExitCode()
	}
	return code, buf.String()
}

func waitForPort(t *testing.T, port int, timeout time.Duration, dead <-chan struct{}, log *strings.Builder) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := dialPort(port)
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-dead:
			return false
		default:
		}
		// Tight bounded poll of a local listener — the readiness poll of a
		// foreign process; owned children synchronise on FIFOs instead.
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// ---- direct parent-side DB access ------------------------------------------

// ParentDB opens an extra parent-side connection with the production DSN.
func ParentDB(t *testing.T, path string) *sqlDB {
	t.Helper()
	conn, err := openDSN(path)
	if err != nil {
		t.Fatalf("parent db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &sqlDB{db: conn}
}

// ---- evidence ---------------------------------------------------------------

// EvidenceDir creates docs/test-results/phase-2/<row>/ and writes command.txt.
func EvidenceDir(t *testing.T, row string, command string) string {
	t.Helper()
	dir := filepath.Join(RepoRoot(t), "docs", "test-results", "phase-2", row)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("evidence dir: %v", err)
	}
	if command != "" {
		writeEvidence(t, dir, "command.txt", command)
	}
	return dir
}

// SaveEvidence writes one artifact file.
func SaveEvidence(t *testing.T, dir, name, content string) {
	t.Helper()
	writeEvidence(t, dir, name, content)
}

func writeEvidence(t *testing.T, dir, name, content string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("WARN: could not write evidence %s: %v", filepath.Join(dir, name), err)
	}
}

// EvidenceNote appends to a file (e.g. the per-row verdict line).
func EvidenceNote(t *testing.T, dir, name, content string) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("WARN: evidence note: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

// DumpJSON renders a value as indented JSON for evidence files.
func DumpJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("(marshal error: %v)", err)
	}
	return string(b)
}
