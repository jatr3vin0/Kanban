package clocksource

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealClockIsDefault: with the variables unset, FromEnv gives real server
// time and nothing else changes (C-CONV-11: the seam is inert by default).
func TestRealClockIsDefault(t *testing.T) {
	t.Setenv(EnvClock, "")
	t.Setenv(EnvClockFile, "")

	clock, err := FromEnv()
	require.NoError(t, err)
	_, isReal := clock.(RealClock)
	assert.True(t, isReal, "the default clock must be RealClock")

	before := time.Now().UTC().UnixMilli()
	now := clock.NowMS()
	after := time.Now().UTC().UnixMilli()
	assert.GreaterOrEqual(t, now, before)
	assert.LessOrEqual(t, now, after)
}

// TestExplicitRealMode.
func TestExplicitRealMode(t *testing.T) {
	t.Setenv(EnvClock, ModeReal)
	clock, err := FromEnv()
	require.NoError(t, err)
	_, isReal := clock.(RealClock)
	assert.True(t, isReal)
}

// TestFakeClockReadsTheFile: fake mode substitutes the source of now_ms and
// nothing else, so a test can advance time by writing the file.
func TestFakeClockReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clock")
	require.NoError(t, os.WriteFile(path, []byte("1761865702180\n"), 0o600))
	t.Setenv(EnvClock, ModeFake)
	t.Setenv(EnvClockFile, path)

	clock, err := FromEnv()
	require.NoError(t, err)
	assert.Equal(t, int64(1761865702180), clock.NowMS())

	// Moving time is one file write: no restart, no sleep.
	require.NoError(t, os.WriteFile(path, []byte("1761865702199"), 0o600))
	assert.Equal(t, int64(1761865702199), clock.NowMS())

	// A malformed write must not jump the clock to real time (which would be
	// hours off and make a deterministic test flaky); the last good value holds.
	require.NoError(t, os.WriteFile(path, []byte("not-a-number"), 0o600))
	assert.Equal(t, int64(1761865702199), clock.NowMS())
}

// TestFakeClockWithoutFileIsAStartupError: never a silent fallback.
func TestFakeClockWithoutFileIsAStartupError(t *testing.T) {
	t.Setenv(EnvClock, ModeFake)
	t.Setenv(EnvClockFile, "")

	_, err := FromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvClock)
	assert.Contains(t, err.Error(), EnvClockFile)
}

// TestInvalidModeIsAStartupError.
func TestInvalidModeIsAStartupError(t *testing.T) {
	t.Setenv(EnvClock, "banana")
	_, err := FromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvClock)
	assert.Contains(t, err.Error(), "banana")
}
