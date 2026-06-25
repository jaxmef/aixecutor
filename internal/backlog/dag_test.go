package backlog

import (
	"strings"
	"testing"
)

// tk is a terse Ticket constructor for graph tests.
func tk(id string, deps ...string) Ticket {
	return Ticket{ID: id, DependsOn: deps, Status: StatusPending, Task: "t"}
}

func TestBuildGraphRejectsUnknownDep(t *testing.T) {
	_, err := BuildGraph([]Ticket{tk("A", "ghost")})
	if err == nil || !strings.Contains(err.Error(), "unknown ticket") {
		t.Errorf("expected unknown-dep error, got %v", err)
	}
}

func TestBuildGraphRejectsCycle(t *testing.T) {
	_, err := BuildGraph([]Ticket{tk("A", "B"), tk("B", "C"), tk("C", "A")})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestBuildGraphRejectsSelfCycle(t *testing.T) {
	_, err := BuildGraph([]Ticket{tk("A", "A")})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected self-cycle error, got %v", err)
	}
}

func TestNextReadyOrderAndDeps(t *testing.T) {
	g, err := BuildGraph([]Ticket{tk("C"), tk("A", "B"), tk("B")})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}

	done := map[string]bool{}
	// Lowest-id ready ticket with deps satisfied: A depends on B (not done), so the
	// candidates are B and C; lowest id is B.
	got, ok := g.NextReady(done)
	if !ok || got.ID != "B" {
		t.Fatalf("first ready = %q (ok=%v), want B", got.ID, ok)
	}
	done["B"] = true
	// Now A's dep is satisfied; A < C so A is next.
	got, _ = g.NextReady(done)
	if got.ID != "A" {
		t.Fatalf("second ready = %q, want A", got.ID)
	}
	done["A"] = true
	got, _ = g.NextReady(done)
	if got.ID != "C" {
		t.Fatalf("third ready = %q, want C", got.ID)
	}
	done["C"] = true
	if _, ok := g.NextReady(done); ok {
		t.Errorf("expected no ready tickets once all done")
	}
}

func TestNextReadySkipsDoneAndBlockedStatus(t *testing.T) {
	tickets := []Ticket{
		{ID: "A", Status: StatusDone, Task: "t"},
		{ID: "B", Status: StatusBlocked, Task: "t"},
		{ID: "C", Status: StatusPending, Task: "t", DependsOn: []string{"A"}},
	}
	g, err := BuildGraph(tickets)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	// A is author-done (treated as satisfied for C's dep), B is parked. Only C is
	// selectable. Author-done A counts toward the done set for dependency checks.
	done := map[string]bool{"A": true}
	got, ok := g.NextReady(done)
	if !ok || got.ID != "C" {
		t.Errorf("ready = %q (ok=%v), want C", got.ID, ok)
	}
}
