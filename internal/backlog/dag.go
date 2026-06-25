package backlog

import (
	"fmt"
	"sort"
)

// Graph is the dependency DAG over a set of tickets. It is built once from
// discovered tickets, validates that every dependency exists and that there are no
// cycles, and then answers "which ticket is ready next" given the set of completed
// ids.
type Graph struct {
	byID  map[string]Ticket
	order []string // ticket ids, sorted ascending (deterministic selection)
}

// BuildGraph indexes the tickets, validates that every dependency names a known
// ticket, and rejects a dependency cycle. The returned graph drives selection.
func BuildGraph(tickets []Ticket) (*Graph, error) {
	g := &Graph{byID: make(map[string]Ticket, len(tickets))}
	for _, t := range tickets {
		g.byID[t.ID] = t
		g.order = append(g.order, t.ID)
	}
	sort.Strings(g.order)

	// Every dependency must reference a known ticket.
	for _, id := range g.order {
		for _, dep := range g.byID[id].DependsOn {
			if _, ok := g.byID[dep]; !ok {
				return nil, fmt.Errorf("ticket %q depends on unknown ticket %q", id, dep)
			}
		}
	}

	if cycle := g.findCycle(); cycle != nil {
		return nil, fmt.Errorf("dependency cycle detected: %s", formatCycle(cycle))
	}
	return g, nil
}

// Tickets returns the tickets in deterministic (id-sorted) order.
func (g *Graph) Tickets() []Ticket {
	out := make([]Ticket, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, g.byID[id])
	}
	return out
}

// NextReady returns the next ticket to run: the lowest-id ticket that is not yet
// done, is selectable (status pending — not done/blocked), and whose dependencies
// are all in done. done holds the ids considered complete (author-declared done
// plus runner-completed). It returns ok=false when nothing is ready (the backlog
// is exhausted or every remaining ticket is blocked by an unfinished dependency).
func (g *Graph) NextReady(done map[string]bool) (Ticket, bool) {
	for _, id := range g.order {
		if done[id] {
			continue
		}
		t := g.byID[id]
		if t.Status == StatusDone || t.Status == StatusBlocked {
			continue
		}
		if g.depsSatisfied(t, done) {
			return t, true
		}
	}
	return Ticket{}, false
}

// depsSatisfied reports whether all of t's dependencies are in done.
func (g *Graph) depsSatisfied(t Ticket, done map[string]bool) bool {
	for _, dep := range t.DependsOn {
		if !done[dep] {
			return false
		}
	}
	return true
}

// findCycle returns a cycle (as a slice of ids) if the graph has one, else nil. It
// is a standard DFS with a recursion stack, visiting ids in deterministic order so
// the reported cycle is stable.
func (g *Graph) findCycle() []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(g.order))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range g.byID[id].DependsOn {
			switch color[dep] {
			case gray:
				// Found a back-edge: return the cycle from dep to here.
				return append(cycleFrom(stack, dep), dep)
			case white:
				if c := visit(dep); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}

	for _, id := range g.order {
		if color[id] == white {
			if c := visit(id); c != nil {
				return c
			}
		}
	}
	return nil
}

// cycleFrom returns the suffix of stack starting at id, the path that closes a
// cycle back to id. id is always on the stack when called (it was colored gray on
// the current DFS path), so the final return is an unreachable safety net.
func cycleFrom(stack []string, id string) []string {
	for i, s := range stack {
		if s == id {
			return append([]string(nil), stack[i:]...)
		}
	}
	return append([]string(nil), stack...)
}

// formatCycle renders a cycle as "a -> b -> c -> a".
func formatCycle(cycle []string) string {
	out := ""
	for i, id := range cycle {
		if i > 0 {
			out += " -> "
		}
		out += id
	}
	return out
}
