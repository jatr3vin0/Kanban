// Package clocksource is the single source of `now_ms` (docs/contracts.md
// C-CONV-2) and the inert fake-clock seam of C-CONV-11.
//
// Production uses RealClock: `time.Now().UTC().UnixMilli()`, computed once per
// request and passed into every statement as a bound parameter. Never re-read
// inside SQL, never taken from a client.
//
// A test may substitute the source — and nothing else — by starting the process
// with LEDGERBOARD_CLOCK=fake and LEDGERBOARD_CLOCK_FILE=<path>. In that mode
// every NowMS reads a decimal unix-millisecond value from that file, so a test
// advances time by writing the file: no sleeps, no restarts, and every process
// watching the same file agrees. Unset, the seam is inert and production
// behaviour is byte-identical.
package clocksource

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Environment variables that arm the fake clock. Both are read at startup.
const (
	EnvClock     = "LEDGERBOARD_CLOCK"
	EnvClockFile = "LEDGERBOARD_CLOCK_FILE"

	// ModeReal is the default: server time from the host clock.
	ModeReal = "real"
	// ModeFake reads now_ms from EnvClockFile.
	ModeFake = "fake"
)

// Clock is the only way the store learns the time.
type Clock interface {
	// NowMS returns the current time as unix milliseconds.
	NowMS() int64
}

// RealClock is production time.
type RealClock struct{}

// NowMS implements Clock.
func (RealClock) NowMS() int64 { return time.Now().UTC().UnixMilli() }

// FakeClock reads now_ms from a file. It never caches across a successful read:
// the point of the seam is that a test can move time between two calls.
type FakeClock struct {
	path string
	// last keeps the most recent value successfully read, so a transient read
	// failure mid-test cannot silently jump the clock to real time (which would
	// be off by hours and turn a deterministic test into a flaky one).
	last atomic.Int64
}

// NewFakeClock returns a FakeClock reading path.
func NewFakeClock(path string) *FakeClock { return &FakeClock{path: path} }

// NowMS implements Clock.
func (f *FakeClock) NowMS() int64 {
	if b, err := os.ReadFile(f.path); err == nil {
		if v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			f.last.Store(v)
			return v
		}
	}
	if v := f.last.Load(); v != 0 {
		return v
	}
	// No value has ever been readable: fall back to real time rather than to
	// zero, which would make every lease look live forever.
	return RealClock{}.NowMS()
}

// FromEnv builds the clock the process was configured with. An invalid
// LEDGERBOARD_CLOCK, or `fake` without a file, is an error naming the variable —
// never a silent fallback to real time, because a test that silently ran on the
// real clock would fail confusingly instead of loudly.
func FromEnv() (Clock, error) {
	mode := strings.TrimSpace(os.Getenv(EnvClock))
	switch mode {
	case "", ModeReal:
		return RealClock{}, nil
	case ModeFake:
		path := strings.TrimSpace(os.Getenv(EnvClockFile))
		if path == "" {
			return nil, fmt.Errorf("%s=fake requires %s to name the clock file", EnvClock, EnvClockFile)
		}
		return NewFakeClock(path), nil
	default:
		return nil, fmt.Errorf("invalid %s %q: must be %q or %q", EnvClock, mode, ModeReal, ModeFake)
	}
}
