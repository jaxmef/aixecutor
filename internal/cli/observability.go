package cli

import (
	"io"
	"os"
	"strconv"

	"github.com/jaxmef/aixecutor/internal/log"
)

// verbosity maps the global flags onto a log.Verbosity. -v/--verbose wins over
// -q/--quiet when both are set (showing more is the safer surprise when a user
// explicitly asked for verbosity).
func verbosity(opts *GlobalOptions) log.Verbosity {
	switch {
	case opts.Verbose:
		return log.Verbose
	case opts.Quiet:
		return log.Quiet
	default:
		return log.Normal
	}
}

// newLogger builds the structured logger for a pipeline command. Its console
// handler writes to errOut — the command's stderr (c.ErrOrStderr()) — so
// structured logs never pollute machine-readable stdout, while the run's
// logs/aixecutor.log (attached later by the orchestrator) keeps the durable
// record. The caller must Close() it to flush/close the run log file. A nil
// errOut falls back to os.Stderr.
func newLogger(opts *GlobalOptions, errOut io.Writer) *log.Logger {
	if errOut == nil {
		errOut = os.Stderr
	}
	return log.New(verbosity(opts), errOut)
}

// newProgress builds the human-facing Progress for a pipeline command, writing
// the concise incremental output to the command's stdout (out). TTY-awareness is
// detected from out; a non-TTY (pipe/CI/test buffer) gets plain line output. Colour
// is enabled per colorEnabled (TTY, NO_COLOR unset, --no-color not set).
//
// Unless quiet, a live status region is attached: a redrawn single line on a TTY,
// or a periodic plain "still running" heartbeat on a non-TTY. The caller must stop
// it (progress.Close()) before printing the summary so it lands on a clean line.
func newProgress(opts *GlobalOptions, out io.Writer) *log.Progress {
	tty := log.IsTTY(out)
	color := colorEnabled(opts, out)
	p := log.NewProgress(out).WithColor(color)
	if opts == nil || !opts.Quiet {
		p.WithLive(log.NewLiveStatus(log.LiveConfig{
			Writer: out,
			TTY:    tty,
			Color:  color,
			Width:  terminalWidth,
		}))
	}
	return p
}

// terminalWidth resolves the live region's wrap width from $COLUMNS, falling back
// to 80 when it is unset or not a positive integer.
func terminalWidth() int {
	if v, ok := os.LookupEnv("COLUMNS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

// colorEnabled decides whether coloured human output is appropriate for out: only
// on an interactive terminal, with the de-facto NO_COLOR convention honoured, and
// not overridden by --no-color. It is the single colour-policy seam, reused by the
// run summary so progress and summary agree.
func colorEnabled(opts *GlobalOptions, out io.Writer) bool {
	if opts != nil && opts.NoColor {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return log.IsTTY(out)
}
