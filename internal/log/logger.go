// Package log provides structured logging (log/slog) to the run's logs/ and
// concise, semantic human progress output to stdout, honoring -v/--verbose. See
// CLAUDE.md §7. It depends only on the standard library (slog + os for TTY
// detection), per CLAUDE.md §6.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Verbosity selects the logging level for both the console handler and the
// run-file handler. It is derived from the CLI flags: -v/--verbose → Debug,
// default → Info, an explicit quiet mode → Warn.
type Verbosity int

const (
	// Normal is the default: info-level progress and per-invocation records.
	Normal Verbosity = iota
	// Verbose (-v/--verbose) adds debug detail (e.g. retry/backoff internals).
	Verbose
	// Quiet suppresses info, leaving only warnings and errors.
	Quiet
)

// level maps a Verbosity to its slog.Level.
func (v Verbosity) level() slog.Level {
	switch v {
	case Verbose:
		return slog.LevelDebug
	case Quiet:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// Logger is the structured logger for a run. It writes to a console handler
// (text, to the supplied writer, normally stderr so it never pollutes a command's
// stdout) and, once a run dir is known, ALSO to <run>/logs/aixecutor.log. The
// two share one verbosity. A nil *Logger is safe: every method is a no-op, so
// callers never need to nil-check.
//
// It implements the harness.Logger interface (Infof) so it can be handed to the
// harness registry for retry/dry-run logging, and it backs the invocation-logging
// wrapper (LogInvocation) and the redaction helpers.
type Logger struct {
	verbosity Verbosity
	console   io.Writer

	mu sync.Mutex
	// slogger is rebuilt whenever a file sink is attached, so it always fans out to
	// the currently-active handlers (console, and the run file once known).
	slogger *slog.Logger
	// file is the open run log file (nil until AttachRunFile). Closed by Close.
	file *os.File
	// logsDir is the run's logs directory once attached, so the invocation logger
	// knows where to persist raw output files.
	logsDir string
	// seq counts persisted raw-output files so each invocation gets a unique,
	// ordered filename even when several share a role.
	seq int
}

// New builds a Logger at the given verbosity writing its console handler to w
// (use os.Stderr in production so stdout stays clean for command output). It has
// no file sink until AttachRunFile is called; that is normal, since the run dir is
// not known until the run is created.
func New(v Verbosity, w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	l := &Logger{verbosity: v, console: w}
	l.rebuildLocked()
	return l
}

// Discard returns a Logger that drops everything (console writer is io.Discard,
// no file). Handy for tests and for code paths that want a non-nil logger without
// output.
func Discard() *Logger {
	return New(Quiet, io.Discard)
}

// AttachRunFile opens <logsDir>/aixecutor.log (creating logsDir if needed) and
// adds it as a second structured sink, so from this point every log line also
// lands in the run's durable log. It is idempotent-ish: calling it again swaps to
// a new file (the previous one is closed). A failure to open the file is returned
// but is non-fatal to the caller — logging simply stays console-only.
func (l *Logger) AttachRunFile(logsDir string) error {
	if l == nil {
		return nil
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("log: creating logs dir %q: %w", logsDir, err)
	}
	f, err := os.OpenFile(filepath.Join(logsDir, "aixecutor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("log: opening run log file: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
	}
	l.file = f
	l.logsDir = logsDir
	l.rebuildLocked()
	return nil
}

// LogsDir returns the attached run logs directory, or "" if none is attached.
func (l *Logger) LogsDir() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logsDir
}

// rebuildLocked reconstructs the slog.Logger to fan out to the console handler and
// the file handler (when a file is attached). Caller holds mu (or is in New before
// the logger is shared).
//
// The two handlers run at DIFFERENT levels. The file keeps the full
// verbosity.level() mapping (Normal→Info, Verbose→Debug, Quiet→Warn) so the run's
// durable log records everything. The console is quieter by default: structured
// slog lines are gated to Warn unless verbosity is Verbose (then Debug, matching the
// file), so a default run shows only human progress output, not raw slog records.
func (l *Logger) rebuildLocked() {
	fileLevel := l.verbosity.level()
	consoleLevel := slog.LevelWarn
	if l.verbosity == Verbose {
		consoleLevel = slog.LevelDebug
	}
	handlers := []slog.Handler{
		slog.NewTextHandler(l.console, &slog.HandlerOptions{Level: consoleLevel}),
	}
	if l.file != nil {
		handlers = append(handlers, slog.NewTextHandler(l.file, &slog.HandlerOptions{Level: fileLevel}))
	}
	l.slogger = slog.New(fanout(handlers))
}

// Close releases the run log file, if any. Safe to call on a nil logger or when
// no file is attached.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	l.rebuildLocked()
	return err
}

// slog returns the current fan-out logger under the lock.
func (l *Logger) slog() *slog.Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.slogger
}

// Debug logs at debug level with structured attributes. No-op on a nil logger.
func (l *Logger) Debug(msg string, args ...any) {
	if l == nil {
		return
	}
	l.slog().Debug(msg, args...)
}

// Info logs at info level with structured attributes. No-op on a nil logger.
func (l *Logger) Info(msg string, args ...any) {
	if l == nil {
		return
	}
	l.slog().Info(msg, args...)
}

// Warn logs at warn level with structured attributes. No-op on a nil logger.
func (l *Logger) Warn(msg string, args ...any) {
	if l == nil {
		return
	}
	l.slog().Warn(msg, args...)
}

// Error logs at error level with structured attributes. No-op on a nil logger.
func (l *Logger) Error(msg string, args ...any) {
	if l == nil {
		return
	}
	l.slog().Error(msg, args...)
}

// Infof implements the harness.Logger interface (a printf-style info line), so
// this Logger can be passed to the harness registry for retry/dry-run logging.
// The formatted message is logged at info level. No-op on a nil logger.
func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.slog().Info(fmt.Sprintf(format, args...))
}

// nextSeq returns the next monotonically-increasing sequence number for naming a
// persisted raw-output file, under the lock.
func (l *Logger) nextSeq() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return l.seq
}

// secretKeyPattern matches environment variable NAMES that look like secrets, so
// their VALUES are never written to a log. It is intentionally broad (any name
// containing key/token/secret/password/auth, case-insensitive); over-redaction is
// safe, under-redaction is not.
var secretKeyPattern = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|AUTH)`)

// redactedEnvKeys returns the SORTED env variable NAMES from m, with the values
// elided entirely. Logging only the (possibly-redacted) keys — never the values —
// guarantees a secret value cannot leak into the logs while still recording that
// the invocation carried extra env. A key matching secretKeyPattern is suffixed
// with "(redacted)" so a reader can see a sensitive var was present without its
// value. Returns nil for empty input so the log attribute is omitted.
func redactedEnvKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if secretKeyPattern.MatchString(k) {
			keys = append(keys, k+" (redacted)")
		} else {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// IsTTY reports whether w is a character device (an interactive terminal) using
// only the standard library: it stats the underlying *os.File and checks
// os.ModeCharDevice. A non-*os.File writer (a buffer, a pipe) is never a TTY, so
// the progress output falls back to plain, line-oriented mode. This is the single
// TTY-detection seam the progress renderer uses.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// fanout is a slog.Handler that broadcasts each record to several handlers, so a
// single Logger writes to both the console and the run file at once. It is a
// minimal multiplexer: Enabled is true if ANY child is enabled; Handle forwards to
// every child whose level admits the record; WithAttrs/WithGroup propagate.
type fanout []slog.Handler

func (h fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, child := range h {
		if child.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanout) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, child := range h {
		if !child.Enabled(ctx, r.Level) {
			continue
		}
		if err := child.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(h))
	for i, child := range h {
		out[i] = child.WithAttrs(attrs)
	}
	return out
}

func (h fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(h))
	for i, child := range h {
		out[i] = child.WithGroup(name)
	}
	return out
}
