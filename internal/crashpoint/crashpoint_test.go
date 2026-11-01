package crashpoint

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVocabularyIsClosed pins the eight names of the C-CONV-11 footnote. A
// ninth point is a test-plan change, not an edit.
func TestVocabularyIsClosed(t *testing.T) {
	assert.Equal(t, []string{
		"claim.pre_commit",
		"claim.post_commit",
		"renew.pre_commit",
		"renew.post_commit",
		"handoff.after_handoff_insert",
		"handoff.pre_commit",
		"memory.post_adapter_write",
		"memory.pre_ack_commit",
	}, Points())
	for _, name := range Points() {
		assert.True(t, Valid(name), "%s must be in the closed set", name)
	}
	for _, bogus := range []string{"", "claim", "claim.pre-commit", "tx.post_commit", "handoff.accept"} {
		assert.False(t, Valid(bogus), "%q must not be a point", bogus)
	}
}

// TestUnarmedIsInert is T-SEAM-01 leg (ii) at package level: with no variable
// set, every point is a no-op and the process survives.
func TestUnarmedIsInert(t *testing.T) {
	t.Setenv(EnvPoints, "")
	Init()
	t.Cleanup(Init) // leave the package unarmed for any later test

	for _, name := range Points() {
		assert.False(t, Armed(name), "%s must not be armed by default", name)
		Hit(name) // must return, not kill the test process
	}
	assert.Empty(t, ArmedNames())
}

// TestUnknownNamesAreIgnored: a typo must neither arm something nor crash a
// production process.
func TestUnknownNamesAreIgnored(t *testing.T) {
	t.Setenv(EnvPoints, "claim.nope, handoff.accept ,claim.pre_commit")
	Init()
	t.Cleanup(Init)

	assert.Equal(t, []string{"claim.pre_commit"}, ArmedNames())
	assert.False(t, Armed("claim.nope"))
}

// TestArmedNamesParsed covers whitespace and multiple names.
func TestArmedNamesParsed(t *testing.T) {
	t.Setenv(EnvPoints, " renew.post_commit , memory.pre_ack_commit ")
	Init()
	t.Cleanup(Init)

	assert.Equal(t, []string{"memory.pre_ack_commit", "renew.post_commit"}, ArmedNames())
	assert.True(t, Armed("renew.post_commit"))
	assert.False(t, Armed("renew.pre_commit"), "only the named points are armed")
}

// TestArmedPointKillsOwnProcess is T-SEAM-01 leg (iii): a subprocess started
// with one point armed writes exactly one marker line to the crash FIFO and dies
// from signal 9 — not from a panic, not from os.Exit.
func TestArmedPointKillsOwnProcess(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "crash.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	cmd := helperCommand(t, fifo, "claim.post_commit")
	require.NoError(t, cmd.Start(), "starting the armed helper")

	// The child blocks opening the FIFO for writing until this read side opens.
	f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
	require.NoError(t, err)
	line, err := bufio.NewReader(f).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, f.Close())

	assert.True(t, strings.HasPrefix(line, "crash claim.post_commit "),
		"the marker names the point that fired, got %q", line)
	fields := strings.Fields(strings.TrimSpace(line))
	require.Len(t, fields, 3)
	assert.Equal(t, "1", fields[2], "the sequence number starts at 1")

	err = cmd.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the helper must not exit cleanly")
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.True(t, status.Signaled(), "the helper must die from a signal, not exit()")
	assert.Equal(t, syscall.SIGKILL, status.Signal(), "the signal must be SIGKILL (no deferred code may run)")
}

// TestUnarmedSubprocessSurvives is the negative control for the leg above: the
// same binary with nothing armed walks every call site and stays alive.
func TestUnarmedSubprocessSurvives(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "crash.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	cmd := helperCommand(t, fifo, "")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "an unarmed helper must exit 0, output: %s", string(out))
	assert.Contains(t, string(out), "alive", "the helper must reach the end of the call sites")

	// Nothing was ever written to the FIFO. A non-blocking open tells us
	// immediately: no writer means no marker.
	f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	assert.Zero(t, n, "an unarmed run must leave the crash FIFO empty, got %q", string(buf[:n]))
}

// helperCommand builds a subprocess that runs this test binary's helper entry
// point with the given crash configuration.
func helperCommand(t *testing.T, fifo, points string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashHelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"LEDGERBOARD_CRASH_HELPER=1",
		EnvFIFO+"="+fifo,
		EnvPoints+"="+points,
	)
	return cmd
}

// TestCrashHelperProcess is not a test: it is the body of the subprocess above.
// It only runs when LEDGERBOARD_CRASH_HELPER is set.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv("LEDGERBOARD_CRASH_HELPER") != "1" {
		t.Skip("helper process body; only runs as a subprocess")
	}
	Init()
	// Walk the whole vocabulary, as the unarmed negative control requires.
	for _, name := range Points() {
		Hit(name)
	}
	os.Stdout.WriteString("alive\n")
}
