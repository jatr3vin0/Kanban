// Shared helpers of the MP rows: barrier agents, result readers, fixtures.
package mptest

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/rgracey/kanban-mcp/internal/store"
)

var fifoSeq atomic.Int64

// DefaultLeaseTTL is the contract default lease (C-CONV-10).
func DefaultLeaseTTL() int64 { return store.DefaultLeaseTTLMS }

// modelsClaimReq builds a ClaimRequest for the parent's fixture setup.
func modelsClaimReq(ticketID, actorRef string) store.ClaimRequest {
	return store.ClaimRequest{TicketID: ticketID, Actor: actorRefActor(actorRef), RequestID: "req-mp-fixture"}
}

func actorRefActor(ref string) models.Actor {
	// "agent:name" | "human:name" — same rendering lease_holder stores.
	for _, p := range []struct {
		prefix string
		typ    models.ActorType
	}{
		{"agent:", models.ActorAgent},
		{"human:", models.ActorHuman},
		{"orchestrator:", models.ActorOrchestrator},
		{"system:", models.ActorSystem},
	} {
		if len(ref) > len(p.prefix) && ref[:len(p.prefix)] == p.prefix {
			return models.Actor{Type: p.typ, ID: ref[len(p.prefix):]}
		}
	}
	return models.Actor{Type: models.ActorAgent, ID: ref}
}

// barrierAgent is one harness-agent child with its own ready/go FIFO pair.
type barrierAgent struct {
	child    *Child
	readyCh  <-chan struct{}
	goFile   *os.File // parent's O_RDWR handle, held open for the whole test
	fifoBase string
}

// startBarrier spawns the agent; readiness is a message on the ready FIFO
// (§1.1), the go FIFO is written by releaseBarrier.
//
// The parent keeps its O_RDWR handle on the go FIFO until cleanup: a FIFO
// whose every open fd is closed is destroyed by the kernel and its buffered
// data is discarded, so a child that reaches its blocking O_RDONLY open after
// the parent's open/write/close would block forever (observed: one silent
// agent per run, the collect timeout, then the illegal-Fatalf hang). With the
// handle held, the child's open always finds a writer and then reads the
// buffered "go\n" line.
func startBarrier(t *testing.T, agentBin, fifoDir string, env []string) *barrierAgent {
	n := fifoSeq.Add(1)
	base := fmt.Sprintf("b%d-%d", n, time.Now().UnixNano()%1000000)
	ready := MakeFIFO(t, fifoDir, base+".ready")
	goP := MakeFIFO(t, fifoDir, base+".go")
	goF, err := os.OpenFile(goP, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open go fifo: %v", err)
	}
	t.Cleanup(func() { _ = goF.Close() })
	readyCh := make(chan struct{})
	go func() {
		// Blocks until the child opens its writer side: readiness is a message.
		f, err := os.OpenFile(ready, os.O_RDONLY, 0)
		if err == nil {
			buf := make([]byte, 32)
			_, _ = f.Read(buf)
			_ = f.Close()
		}
		close(readyCh)
	}()
	all := append(append([]string{}, env...),
		"LEDGERBOARD_TEST_READY_FIFO="+ready,
		"LEDGERBOARD_TEST_GO_FIFO="+goP,
	)
	return &barrierAgent{child: StartAgent(t, agentBin, all...), readyCh: readyCh, goFile: goF, fifoBase: base}
}

// waitReady blocks until all agents signal readiness (bounded).
func waitReady(t *testing.T, agents []*barrierAgent) {
	for i, a := range agents {
		select {
		case <-a.readyCh:
		case <-time.After(30 * time.Second):
			t.Fatalf("agent %d never signalled ready", i)
		}
	}
}

// releaseBarrier signals every agent through its own go FIFO. The parent's
// O_RDWR handle stays open (see startBarrier): the write buffers in the pipe
// and the child reads it whenever its open lands, whichever wins the race.
func releaseBarrier(t *testing.T, agents []*barrierAgent) {
	for _, a := range agents {
		if _, err := a.goFile.WriteString("go\n"); err != nil {
			t.Fatalf("write go fifo: %v", err)
		}
	}
}

// collect reads one result line per child, in any order, bounded. Children are
// read from helper goroutines, so failures come back as errors and are
// reported by t.Fatalf HERE, on the test goroutine (a t.Fatalf inside the
// reader goroutine would silently Goexit it and deadlock this loop).
func collect(t *testing.T, children []*Child, timeout time.Duration) []map[string]any {
	out := make([]map[string]any, len(children))
	type indexed struct {
		i   int
		m   map[string]any
		err error
	}
	results := make(chan indexed, len(children))
	for i, c := range children {
		go func(i int, c *Child) {
			m, err := c.LineErr(t, timeout)
			results <- indexed{i, m, err}
		}(i, c)
	}
	for range children {
		r := <-results
		if r.err != nil {
			t.Fatalf("child %d: %v", r.i, r.err)
		}
		out[r.i] = r.m
	}
	return out
}

func okOf(m map[string]any) bool {
	v, _ := m["ok"].(bool)
	return v
}

func codeOf(m map[string]any) string {
	s, _ := m["error_code"].(string)
	return s
}

func detailOf(m map[string]any) map[string]any {
	d, _ := m["details"].(map[string]any)
	return d
}

func num(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	return int64(v)
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func countOK(results []map[string]any) int {
	n := 0
	for _, m := range results {
		if okOf(m) {
			n++
		}
	}
	return n
}

// claimEnv assembles the env for a claim op.
func claimEnv(dbPath, ticket, actorRef string) []string {
	return []string{
		"LEDGERBOARD_TEST_OP=claim",
		"LEDGERBOARD_TEST_DB=" + dbPath,
		"LEDGERBOARD_TEST_TICKET=" + ticket,
		"LEDGERBOARD_TEST_ACTOR=" + actorRef,
	}
}

// agentBase is the common env prefix every agent child needs.
func agentBase() []string { return []string{"LEDGERBOARD_TEST_AGENT=1"} }

// fixtureBoard is the one-board/one-ticket fixture of the claim rows.
func fixtureBoard(t *testing.T, s *store.SQLiteStore, title string) (string, string) {
	t.Helper()
	board, err := s.CreateBoard(t.Context(), "MP board", "")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := s.CreateTicket(t.Context(), board.ID, models.Ticket{
		Title: title, Status: models.StatusTodo, Priority: models.PriorityMedium,
	})
	if err != nil {
		t.Fatal(err)
	}
	return board.ID, tk.ID
}
