package cli

import (
	"io"
	"os"

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
// detected from out; a non-TTY (pipe/CI/test buffer) gets plain line output.
func newProgress(out io.Writer) *log.Progress {
	return log.NewProgress(out)
}
