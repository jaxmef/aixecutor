package log

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestProgressSemanticEvents proves each semantic method renders a concise,
// line-oriented string on a non-TTY writer (the plain fallback required by the
// ticket), and that a buffer is detected as non-TTY.
func TestProgressSemanticEvents(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf)
	if p.TTY() {
		t.Error("a bytes.Buffer must be detected as non-TTY (plain fallback)")
	}

	p.RunCreated("run-1", "add a flag")
	p.PhaseStarted("Planning")
	p.PlanningDone("/runs/run-1/docs", 3)
	p.SubtaskStarted("st-01", "schema")
	p.SubtaskReviewVerdict("st-01", 1, false, 2)
	p.SubtaskReviewVerdict("st-01", 2, true, 0)
	p.SubtaskDone("st-01", 1, 0)
	p.SubtaskDone("st-02", 3, 2)
	p.SeniorRound(1, false, 4)
	p.SeniorRound(2, true, 0)
	p.RunCompleted("completed")

	out := buf.String()
	wants := []string{
		"Run run-1 created for task: add a flag",
		"Planning",
		"Planning complete: 3 subtask(s). Docs: /runs/run-1/docs",
		"[st-01] implementing: schema",
		"[st-01] review round 1: 2 finding(s)",
		"[st-01] review round 2: approved",
		"[st-01] done (loops=1)",
		"[st-02] done (loops=3, 2 unresolved finding(s) carried forward)",
		"senior review round 1: 4 finding(s)",
		"senior review round 2: clean",
		"Run completed.",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("progress output missing %q:\n%s", w, out)
		}
	}
	// Plain, line-oriented: no ANSI escape sequences on a non-TTY.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-TTY output must be plain (no ANSI escapes):\n%q", out)
	}
	// Every event is its own line.
	if n := strings.Count(out, "\n"); n != len(wants) {
		t.Errorf("expected %d lines, got %d:\n%s", len(wants), n, out)
	}
}

// TestProgressNilSafe proves methods on a nil *Progress are no-ops (so phases need
// not nil-check).
func TestProgressNilSafe(t *testing.T) {
	var p *Progress
	p.PhaseStarted("x")
	p.Logf("y %d", 1)
	p.SubtaskDone("a", 1, 0)
	if p.TTY() {
		t.Error("nil Progress TTY should be false")
	}
}

// TestProgressConcurrentWritesDoNotInterleave proves Progress serializes writes so
// concurrent subtask workers cannot corrupt a line. We emit many lines from many
// goroutines and assert every line is intact (each is a full SubtaskDone line).
func TestProgressConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf)

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.SubtaskDone("st", i, 0)
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != workers {
		t.Fatalf("expected %d lines, got %d", workers, len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "  [st] done (loops=") {
			t.Errorf("interleaved/corrupted line: %q", l)
		}
	}
}
