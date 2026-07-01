package log

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jaxmef/aixecutor/internal/harness"
)

// livenessInterval is the coarse minimum spacing between non-TTY "still running"
// liveness lines, so a piped/CI run gets a periodic heartbeat without flooding
// the log. On a TTY the in-place status line is redrawn on every tick instead.
const livenessInterval = 10 * time.Second

// LiveStatus renders a live, single-line progress region. It follows CLAUDE.md §7:
// ONE owner goroutine holds the render state and is the only writer to the output;
// callers interact purely by sending commands on channels (no mutex over render
// state). The same handle works in two modes, chosen at construction:
//
//   - TTY: an in-place status line redrawn with \r + \x1b[2K (no cursor-up, no
//     scrollback churn). Permanent lines (Emit) are printed above the live line.
//   - non-TTY: ZERO ANSI/cursor bytes. Emit prints plain "line\n"; a rate-limited
//     "still running (Xs): roles" heartbeat is printed on the coarse tick.
//
// A nil *LiveStatus is a no-op everywhere, and every channel send also selects on
// done, so Begin/End/Emit/SetPhase after Stop are safe no-ops (never deadlock) and
// Stop is idempotent.
type LiveStatus struct {
	w      io.Writer
	tty    bool
	color  bool
	now    func() time.Time
	tick   <-chan time.Time
	width  func() int
	ticker *time.Ticker // internal ticker to stop on shutdown (nil if tick injected)

	cmds chan liveCmd
	done chan struct{}
}

// LiveConfig is the injected configuration for a LiveStatus. Writer/Now/Width fall
// back to sensible defaults when nil; Tick falls back to an internal ticker (1s on
// a TTY for smooth elapsed updates, livenessInterval otherwise). Tests inject all
// of these for determinism.
type LiveConfig struct {
	Writer io.Writer
	TTY    bool
	Color  bool
	Now    func() time.Time
	Tick   <-chan time.Time
	Width  func() int
}

type liveKind int

const (
	kindBegin liveKind = iota
	kindEnd
	kindPhase
	kindEmit
	kindStop
	kindSync
)

type liveCmd struct {
	kind  liveKind
	id    int
	role  string
	phase string
	line  string
	reply chan int
}

type liveEntry struct {
	role  string
	start time.Time
}

// NewLiveStatus builds a LiveStatus and starts its owner goroutine. The goroutine
// runs until Stop (or, in tests, until the process exits). Width falls back to 80
// when the configured func is nil.
func NewLiveStatus(cfg LiveConfig) *LiveStatus {
	l := &LiveStatus{
		w:     cfg.Writer,
		tty:   cfg.TTY,
		color: cfg.Color,
		now:   cfg.Now,
		width: cfg.Width,
		cmds:  make(chan liveCmd),
		done:  make(chan struct{}),
	}
	if l.w == nil {
		l.w = os.Stdout
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.width == nil {
		l.width = func() int { return 80 }
	}
	if cfg.Tick != nil {
		l.tick = cfg.Tick
	} else {
		interval := livenessInterval
		if l.tty {
			interval = time.Second
		}
		l.ticker = time.NewTicker(interval)
		l.tick = l.ticker.C
	}
	go l.run()
	return l
}

// Begin registers a role as actively running and returns its id (0 if Stopped or
// nil). The id must be passed to End when the work finishes; pair them with defer.
func (l *LiveStatus) Begin(role string) int {
	if l == nil {
		return 0
	}
	reply := make(chan int, 1)
	select {
	case l.cmds <- liveCmd{kind: kindBegin, role: role, reply: reply}:
	case <-l.done:
		return 0
	}
	select {
	case id := <-reply:
		return id
	case <-l.done:
		return 0
	}
}

// End removes the active entry previously registered by Begin. End(0) is a no-op,
// so a Begin that returned 0 (Stopped) needs no special handling.
func (l *LiveStatus) End(id int) {
	if l == nil || id == 0 {
		return
	}
	l.send(liveCmd{kind: kindEnd, id: id})
}

// SetPhase updates the coarse phase label shown in the live region.
func (l *LiveStatus) SetPhase(name string) {
	if l == nil {
		return
	}
	l.send(liveCmd{kind: kindPhase, phase: name})
}

// Emit prints a permanent progress line: on a TTY above the live status region, on
// a non-TTY as plain "line\n". This is the seam Progress routes its output through
// so the live line and permanent lines never tangle.
func (l *LiveStatus) Emit(line string) {
	if l == nil {
		return
	}
	l.send(liveCmd{kind: kindEmit, line: line})
}

// Stop wipes the live region (TTY) and shuts down the owner goroutine, blocking
// until the final wipe is flushed so subsequent output (the summary) is guaranteed
// to start on a clean line. It is idempotent and safe on a nil receiver;
// subsequent calls and any other commands become no-ops.
func (l *LiveStatus) Stop() {
	if l == nil {
		return
	}
	l.send(liveCmd{kind: kindStop})
	<-l.done
}

// flush is a synchronous round-trip through the owner, used by tests to barrier on
// all previously-issued commands (and any tick processed before it) so the output
// buffer can be inspected deterministically.
func (l *LiveStatus) flush() {
	if l == nil {
		return
	}
	reply := make(chan int, 1)
	select {
	case l.cmds <- liveCmd{kind: kindSync, reply: reply}:
	case <-l.done:
		return
	}
	select {
	case <-reply:
	case <-l.done:
	}
}

// send delivers a command to the owner, abandoning it if the region is already
// stopped (so callers never block after Stop).
func (l *LiveStatus) send(c liveCmd) {
	select {
	case l.cmds <- c:
	case <-l.done:
	}
}

func (l *LiveStatus) run() {
	defer close(l.done)
	if l.ticker != nil {
		defer l.ticker.Stop()
	}

	active := map[int]liveEntry{}
	var order []int
	nextID := 0
	phase := ""
	var lastLiveness time.Time

	for {
		select {
		case c := <-l.cmds:
			switch c.kind {
			case kindBegin:
				nextID++
				active[nextID] = liveEntry{role: c.role, start: l.now()}
				order = append(order, nextID)
				l.redraw(active, order, phase)
				c.reply <- nextID
			case kindEnd:
				if _, ok := active[c.id]; ok {
					delete(active, c.id)
					order = removeID(order, c.id)
					l.redraw(active, order, phase)
				}
			case kindPhase:
				phase = c.phase
				l.redraw(active, order, phase)
			case kindEmit:
				l.emit(c.line, active, order, phase)
			case kindSync:
				c.reply <- 0
			case kindStop:
				if l.tty {
					l.write("\r\x1b[2K")
				}
				return
			}
		case <-l.tick:
			if l.tty {
				l.redraw(active, order, phase)
			} else if len(order) > 0 && l.now().Sub(lastLiveness) >= livenessInterval {
				lastLiveness = l.now()
				l.write(l.livenessText(active, order) + "\n")
			}
		}
	}
}

// redraw repaints the in-place status line on a TTY (no-op on a non-TTY, which must
// emit zero ANSI/cursor bytes). The line is truncated rune-aware to width-1 so it
// can never wrap the terminal.
func (l *LiveStatus) redraw(active map[int]liveEntry, order []int, phase string) {
	if !l.tty {
		return
	}
	text := truncateRunes(l.statusText(active, order, phase), l.width()-1)
	if l.color && text != "" {
		text = Colorize(true, AnsiBlue, text)
	}
	l.write("\r\x1b[2K" + text)
}

// emit prints a permanent line. On a TTY it clears the live line, prints the line
// with a newline, then repaints the status beneath it. On a non-TTY it is just
// plain "line\n" — no escape codes at all.
func (l *LiveStatus) emit(line string, active map[int]liveEntry, order []int, phase string) {
	if !l.tty {
		l.write(line + "\n")
		return
	}
	l.write("\r\x1b[2K" + line + "\n")
	l.redraw(active, order, phase)
}

func (l *LiveStatus) write(s string) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(l.w, s)
}

// statusText builds the TTY status line: "phase | role 3s, role 0s". Empty when
// nothing is active so redraw just clears the line.
func (l *LiveStatus) statusText(active map[int]liveEntry, order []int, phase string) string {
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(order))
	for _, id := range order {
		e := active[id]
		parts = append(parts, fmt.Sprintf("%s %ds", e.role, int(l.now().Sub(e.start).Seconds())))
	}
	body := strings.Join(parts, ", ")
	if phase != "" {
		return phase + " | " + body
	}
	return body
}

// livenessText builds the non-TTY heartbeat line, reporting the longest-running
// active role's elapsed time and the set of active roles.
func (l *LiveStatus) livenessText(active map[int]liveEntry, order []int) string {
	maxSecs := 0
	roles := make([]string, 0, len(order))
	for _, id := range order {
		e := active[id]
		if s := int(l.now().Sub(e.start).Seconds()); s > maxSecs {
			maxSecs = s
		}
		roles = append(roles, e.role)
	}
	return fmt.Sprintf("still running (%ds): %s", maxSecs, strings.Join(roles, ", "))
}

func removeID(order []int, id int) []int {
	for i, v := range order {
		if v == id {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

// truncateRunes shortens s to at most max runes (rune-aware so multi-byte glyphs
// are never split). max <= 0 yields the empty string.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// liveTimerHarness wraps a harness so each invocation registers a live timer entry
// for its role for the call's duration. The defer guarantees the entry is removed
// on return, panic, or context cancellation.
type liveTimerHarness struct {
	inner harness.Harness
	live  *LiveStatus
}

// WrapLiveTimer decorates h so every Run shows a live, per-role timer in the live
// region. A nil live (e.g. --quiet, or no live region) returns h unchanged, so the
// decorator composes transparently at the single orchestrator wrap point.
func WrapLiveTimer(h harness.Harness, live *LiveStatus) harness.Harness {
	if live == nil {
		return h
	}
	return &liveTimerHarness{inner: h, live: live}
}

func (h *liveTimerHarness) Name() string { return h.inner.Name() }

func (h *liveTimerHarness) Run(ctx context.Context, req harness.Request) (harness.Result, error) {
	id := h.live.Begin(req.Role)
	defer h.live.End(id)
	return h.inner.Run(ctx, req)
}
