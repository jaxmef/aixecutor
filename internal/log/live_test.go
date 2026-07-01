package log

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeClock returns a now func reading a mutable time pointer, so tests advance the
// clock deterministically without time.Now.
func fakeClock(cur *time.Time) func() time.Time {
	return func() time.Time { return *cur }
}

// TestLiveTTYRedraw proves a TTY tick redraws a single status line carrying the \r
// and \x1b[2K control sequence and the elapsed time, that End removes the entry,
// and that Stop emits a final clear.
func TestLiveTTYRedraw(t *testing.T) {
	var buf bytes.Buffer
	tick := make(chan time.Time)
	base := time.Unix(1000, 0)
	cur := base
	l := NewLiveStatus(LiveConfig{
		Writer: &buf, TTY: true, Now: fakeClock(&cur), Tick: tick,
		Width: func() int { return 80 },
	})

	id := l.Begin("executor")
	cur = base.Add(3 * time.Second)
	tick <- cur
	l.flush()

	out := buf.String()
	if !strings.Contains(out, "\r") || !strings.Contains(out, "\x1b[2K") {
		t.Fatalf("TTY redraw missing \\r/\\x1b[2K: %q", out)
	}
	if !strings.Contains(out, "3s") {
		t.Fatalf("TTY redraw missing elapsed time: %q", out)
	}
	if !strings.Contains(out, "executor") {
		t.Fatalf("TTY redraw missing active role: %q", out)
	}

	buf.Reset()
	l.End(id)
	l.flush()
	if strings.Contains(buf.String(), "executor") {
		t.Fatalf("End must remove the active role: %q", buf.String())
	}

	buf.Reset()
	l.Stop()
	if !strings.Contains(buf.String(), "\x1b[2K") {
		t.Fatalf("Stop must emit a final clear: %q", buf.String())
	}
}

// TestLiveNonTTYPlain proves the non-TTY path emits ZERO ANSI/cursor bytes: Begin
// is silent, the periodic heartbeat and Emit are plain "text\n".
func TestLiveNonTTYPlain(t *testing.T) {
	var buf bytes.Buffer
	tick := make(chan time.Time)
	base := time.Unix(1000, 0)
	cur := base
	l := NewLiveStatus(LiveConfig{
		Writer: &buf, TTY: false, Now: fakeClock(&cur), Tick: tick,
		Width: func() int { return 80 },
	})

	l.Begin("executor")
	l.flush()
	if buf.Len() != 0 {
		t.Fatalf("Begin must be silent on a non-TTY: %q", buf.String())
	}

	cur = base.Add(11 * time.Second)
	tick <- cur
	l.flush()
	out := buf.String()
	if !strings.Contains(out, "still running") || !strings.Contains(out, "executor") {
		t.Fatalf("non-TTY heartbeat missing: %q", out)
	}
	if strings.Contains(out, "\x1b[") || strings.Contains(out, "\r") {
		t.Fatalf("non-TTY output must be plain (no ANSI/\\r): %q", out)
	}

	buf.Reset()
	l.Emit("hello world")
	l.flush()
	if got := buf.String(); got != "hello world\n" {
		t.Fatalf("non-TTY Emit must be plain line: %q", got)
	}

	l.Stop()
}

// TestLiveTwoBegin proves two simultaneous Begin calls (same role) are both
// rendered and each removed by its own id.
func TestLiveTwoBegin(t *testing.T) {
	var buf bytes.Buffer
	tick := make(chan time.Time)
	base := time.Unix(1000, 0)
	cur := base
	l := NewLiveStatus(LiveConfig{
		Writer: &buf, TTY: true, Now: fakeClock(&cur), Tick: tick,
		Width: func() int { return 80 },
	})

	id1 := l.Begin("executor")
	id2 := l.Begin("executor")
	if id1 == id2 || id1 == 0 || id2 == 0 {
		t.Fatalf("Begin ids must be distinct and non-zero: %d %d", id1, id2)
	}

	buf.Reset()
	tick <- base
	l.flush()
	if n := strings.Count(buf.String(), "executor"); n != 2 {
		t.Fatalf("both Begin entries must render, got %d: %q", n, buf.String())
	}

	buf.Reset()
	l.End(id1)
	l.flush()
	if n := strings.Count(buf.String(), "executor"); n != 1 {
		t.Fatalf("ending id1 must leave one entry, got %d: %q", n, buf.String())
	}

	buf.Reset()
	l.End(id2)
	l.flush()
	if strings.Contains(buf.String(), "executor") {
		t.Fatalf("ending id2 must remove the last entry: %q", buf.String())
	}

	l.Stop()
}

// TestLiveNilAndIdempotent proves a nil *LiveStatus is a no-op, Stop is idempotent,
// and Begin/End/Emit/SetPhase after Stop are no-ops that never deadlock.
func TestLiveNilAndIdempotent(t *testing.T) {
	var nilL *LiveStatus
	if nilL.Begin("x") != 0 {
		t.Fatal("nil Begin must return 0")
	}
	nilL.End(1)
	nilL.SetPhase("p")
	nilL.Emit("e")
	nilL.Stop() // must not panic

	var buf bytes.Buffer
	l := NewLiveStatus(LiveConfig{Writer: &buf, TTY: true, Tick: make(chan time.Time)})
	l.Stop()
	l.Stop() // idempotent, no deadlock

	if l.Begin("y") != 0 {
		t.Fatal("Begin after Stop must return 0")
	}
	l.End(1)
	l.Emit("z")
	l.SetPhase("w") // none deadlock
}

// TestLiveTruncateWidth proves the status line is truncated rune-aware to width-1.
func TestLiveTruncateWidth(t *testing.T) {
	var buf bytes.Buffer
	tick := make(chan time.Time)
	base := time.Unix(1000, 0)
	cur := base
	l := NewLiveStatus(LiveConfig{
		Writer: &buf, TTY: true, Now: fakeClock(&cur), Tick: tick,
		Width: func() int { return 10 },
	})

	l.Begin("a-very-long-role-name-that-exceeds-the-width")
	tick <- base
	l.flush()

	out := buf.String()
	idx := strings.LastIndex(out, "\x1b[2K")
	if idx < 0 {
		t.Fatalf("no redraw written: %q", out)
	}
	text := out[idx+len("\x1b[2K"):]
	if n := utf8.RuneCountInString(text); n > 9 {
		t.Fatalf("status line not truncated to width-1 (9): got %d runes %q", n, text)
	}

	l.Stop()
}

// TestLiveWidthFallback proves the width func defaults to 80 when not injected.
func TestLiveWidthFallback(t *testing.T) {
	l := NewLiveStatus(LiveConfig{Writer: &bytes.Buffer{}, TTY: true, Tick: make(chan time.Time)})
	if w := l.width(); w != 80 {
		t.Fatalf("default width must be 80, got %d", w)
	}
	l.Stop()
}
