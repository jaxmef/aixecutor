package log

// ANSI SGR codes used to colourise human progress and the run summary. They are
// raw escape strings (no dependency) — the single place colour codes live so the
// palette stays consistent across Progress and the summary.
const (
	AnsiReset  = "\x1b[0m"
	AnsiBold   = "\x1b[1m"
	AnsiRed    = "\x1b[31m"
	AnsiGreen  = "\x1b[32m"
	AnsiYellow = "\x1b[33m"
	AnsiBlue   = "\x1b[34m"
	AnsiCyan   = "\x1b[36m"
)

// Colorize wraps s in the SGR code and a reset when enabled and code is non-empty,
// returning s unchanged otherwise. Keeping the gating here means callers colourise
// unconditionally and the enabled/NO_COLOR/TTY decision lives at one seam.
func Colorize(enabled bool, code, s string) string {
	if !enabled || code == "" {
		return s
	}
	return code + s + AnsiReset
}
