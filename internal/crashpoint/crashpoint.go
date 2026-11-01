// Package crashpoint is the inert crash-injection seam of docs/contracts.md
// C-CONV-11 and its §0 footnote.
//
// The vocabulary is closed and contains exactly eight points. Each point is one
// call site, `crashpoint.Hit("<name>")`, placed inside the transaction whose
// atomicity it tests. Armed, the hook writes `crash <name> <seq>` to the crash
// FIFO, fsyncs it, and SIGKILLs its own process — never os.Exit and never a
// panic, because a deferred commit or rollback is precisely what the test is
// testing. Unarmed (the default, and always in a production run) a call is one
// atomic load and a branch.
//
// It is not product behaviour: no contract outcome may depend on a point being
// armed, and nothing on the REST/MCP surface can arm one. The only way to arm a
// point is the LEDGERBOARD_CRASH_POINTS environment variable present when the
// process starts and Init called from main.
package crashpoint

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
)

// The closed eight-point vocabulary (C-CONV-11 footnote). Adding a point is a
// test-plan change, not a contract change, but no point may be added to a path
// with no rule behind it.
const (
	ClaimPreCommit         = "claim.pre_commit"
	ClaimPostCommit        = "claim.post_commit"
	RenewPreCommit         = "renew.pre_commit"
	RenewPostCommit        = "renew.post_commit"
	HandoffAfterInsert     = "handoff.after_handoff_insert"
	HandoffPreCommit       = "handoff.pre_commit"
	MemoryPostAdapterWrite = "memory.post_adapter_write"
	MemoryPreAckCommit     = "memory.pre_ack_commit"
)

// EnvPoints is the only variable that arms a point.
const EnvPoints = "LEDGERBOARD_CRASH_POINTS"

// EnvFIFO is the path of the crash FIFO the hook writes its marker to.
const EnvFIFO = "LEDGERBOARD_CRASH_FIFO"

// points is the closed set. Order here is the canonical order of Points().
var points = []string{
	ClaimPreCommit,
	ClaimPostCommit,
	RenewPreCommit,
	RenewPostCommit,
	HandoffAfterInsert,
	HandoffPreCommit,
	MemoryPostAdapterWrite,
	MemoryPreAckCommit,
}

// armed is the parsed armed-set: nil means "no point can fire", which is the
// production state and the one atomic load on the hot path.
var armed atomic.Pointer[map[string]struct{}]

// seq numbers the markers a single process writes.
var seq atomic.Uint64

// Points returns the closed vocabulary in canonical order.
func Points() []string {
	out := make([]string, len(points))
	copy(out, points)
	return out
}

// Valid reports whether name is in the closed vocabulary.
func Valid(name string) bool {
	for _, p := range points {
		if p == name {
			return true
		}
	}
	return false
}

// Init parses LEDGERBOARD_CRASH_POINTS into the armed-set. It is called once,
// from main, before any request is served: that is what makes the seam
// unreachable from a transport. An unset or empty variable leaves the set nil,
// so every Hit is one atomic load and a branch.
//
// Names outside the closed vocabulary are ignored rather than accepted: a typo
// must not silently arm something, and it must not crash a production process
// either.
func Init() {
	raw := strings.TrimSpace(os.Getenv(EnvPoints))
	if raw == "" {
		armed.Store(nil)
		return
	}
	set := make(map[string]struct{})
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if Valid(name) {
			set[name] = struct{}{}
		}
	}
	if len(set) == 0 {
		armed.Store(nil)
		return
	}
	armed.Store(&set)
}

// Armed reports whether name is currently armed. It exists for tests and
// diagnostics; no product path branches on it.
func Armed(name string) bool {
	set := armed.Load()
	if set == nil {
		return false
	}
	_, ok := (*set)[name]
	return ok
}

// ArmedNames returns the armed names, sorted. Empty when nothing is armed.
func ArmedNames() []string {
	set := armed.Load()
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(*set))
	for name := range *set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Hit is the call site. Unarmed it is one atomic load and a branch. Armed it
// writes its marker, fsyncs, and SIGKILLs this process, so no deferred function
// runs.
func Hit(name string) {
	set := armed.Load()
	if set == nil {
		return
	}
	if _, ok := (*set)[name]; !ok {
		return
	}
	fire(name, seq.Add(1))
}

// fire performs the armed action. It is separated from Hit so the decision and
// the action are readable apart, and so a test can prove the decision without
// dying.
func fire(name string, n uint64) {
	if path := os.Getenv(EnvFIFO); path != "" {
		// Opening a FIFO for writing blocks until a reader opens it. That block
		// is the test's synchronisation: the reader learns the exact instant the
		// point was reached, with no polling and no sleep.
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			_, _ = f.WriteString("crash " + name + " " + itoa(n) + "\n")
			// fsync on a FIFO returns EINVAL on Linux; the write itself is
			// already in the pipe, which is what the reader consumes.
			_ = f.Sync()
			_ = f.Close()
		}
	}
	// SIGKILL, never os.Exit and never panic: no deferred function may run.
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	// Unreachable unless the kill failed, which it cannot for SIGKILL on self.
	// Blocking forever is still safer than returning into product code that
	// believes it reached a point it did not.
	select {}
}

// itoa is strconv.FormatUint without the import, kept local so this package's
// import list stays minimal.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
