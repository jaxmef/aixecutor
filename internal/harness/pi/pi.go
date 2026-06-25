// Package pi is the first-class pi coding-agent harness preset. Like the claude
// preset, it is a thin layer over the generic declarative CLI adapter
// (internal/harness): the command, args, prompt delivery, timeout, and env all
// stay config-driven (the verified-correct default block lives in
// internal/config; see CLAUDE.md §5). This package adds only the pi-specific
// concerns the generic adapter does not cover:
//
//   - a PATH preflight (Available) with an actionable hint;
//   - a thin result wrapper that trims trailing whitespace from the final text
//     (CLIs commonly append a trailing newline) while preserving Result.Raw
//     verbatim for logging.
//
// # Verified headless contract (pi v0.79.10)
//
// Confirmed against the installed binary via a read-only `pi --help` (no tokens):
//
//   - Non-interactive: `--print` (alias `-p`) — "process prompt and exit".
//   - Prompt delivery: the prompt is a POSITIONAL argument, NOT stdin (the
//     documented form is `pi -p "List all .ts files in src/"`). So the default
//     uses promptDelivery: arg with {{.Prompt}} as a positional in args.
//   - Model: `--model <pattern>` (provider defaults to google; templated via
//     {{.Model}}).
//   - Output: `--mode <mode>` = text (default) | json | rpc. The default uses
//     text, where the final assistant text is stdout. `--mode json` exists but
//     the JSON envelope field holding the final text is UNVERIFIED (confirming it
//     needs a live `pi -p --mode json` run = tokens + a provider key), so it is
//     deliberately deferred — TODO: verify the resultPath before switching to
//     output: json.
//   - Working dir: pi operates on the current working directory; the generic
//     adapter already sets the subprocess Dir to req.WorkDir, so no extra flag is
//     needed.
//
// # Model passthrough & the empty-model caveat
//
// Nothing about the model is hardcoded here: the role config supplies it and the
// default args template it straight through as {{.Model}}. The compiled defaults
// always set a model for every role, so {{.Model}} is non-empty in practice. If a
// user configures a role with harness: pi but omits a model, the adapter would
// render an empty `--model ""` argument; that is acceptable for v1 and documented
// rather than guarded, since the role schema otherwise always carries a model.
//
// # Sub-agent git safety
//
// Per CLAUDE.md invariant #1, neither the app nor the sub-agent may run write git
// commands. The app never shells out to git at all (read access goes through
// internal/git). For the sub-agent, that safety is enforced by the prompts we
// hand it (internal/prompt, AIX-0008), not here. pi does expose tool-permission
// flags (`--no-tools`, `--tools <allowlist>`, `--exclude-tools <denylist>`,
// confirmed in --help) which could provide defense-in-depth, but they are
// intentionally NOT added to the default args: doing so would diverge from the
// canonical config schema in CLAUDE.md §5, and the prompt-level instruction is
// the authoritative mechanism. A user who wants the extra guard can add a flag in
// their own config.
package pi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
)

// Name is the canonical harness key this preset is registered under (it matches
// the "pi" entry in the default config / CLAUDE.md §5). Callers wiring the
// registry use it as the single source of truth for the key, so the string is
// not duplicated at every call site.
const Name = "pi"

// command is the executable this preset drives. It is used only for the PATH
// preflight; the actual command run is whatever the config specifies (normally
// the same name), so the preset never hardcodes the invocation.
const command = "pi"

// installHint points users at pi's project when the binary is missing. Kept as a
// const so the message is consistent across the error paths.
const installHint = "https://github.com/paralleldrive/pi"

// Factory builds the pi preset for a harness named in config. It matches
// harness.Factory, so a registry plugs it in via
// harness.Options{Factories: {"pi": pi.Factory}} and every other harness keeps
// building generically. It constructs the generic CLI harness from the passed
// cfg — preserving command/args/promptDelivery/timeout/env exactly as configured,
// declaratively, with no hardcoded flags beyond what cfg provides — and wraps it
// so the final text is trimmed (see harnessWrapper.Run).
//
// Because pi's output is text, the generic adapter already returns pi's stdout in
// both Result.Text and Result.Raw; the wrapper only trims trailing whitespace
// from Result.Text (preserving Result.Raw), so no JSON envelope parsing is needed
// here. If/when the json contract is verified, this is where envelope handling
// would live (mirroring the claude preset).
func Factory(name string, cfg config.Harness) (harness.Harness, error) {
	inner, err := harness.NewCLI(name, cfg)
	if err != nil {
		return nil, fmt.Errorf("pi preset %q: %w", name, err)
	}
	return &harnessWrapper{name: name, inner: inner}, nil
}

// harnessWrapper decorates the generic CLI harness with pi-specific result
// handling. It delegates the actual subprocess execution to inner and only
// post-processes the final text.
type harnessWrapper struct {
	name  string
	inner harness.Harness
}

// Name implements harness.Harness, returning the configured harness name so
// role→harness resolution and logging are unaffected by the preset.
func (h *harnessWrapper) Name() string { return h.name }

// Run implements harness.Harness. It runs the generic adapter (which executes pi
// with the configured args/model and delivers the prompt as a positional arg),
// then trims trailing whitespace from the final assistant text. Result.Raw is
// left untouched so the full stdout is available for logging.
//
// Error handling: if the adapter errors (exec failure, timeout, non-zero exit),
// that error is returned as-is with Result.Raw preserved. On a clean exit the
// text-mode adapter cannot fail to parse (stdout is the result), so there is no
// pi-specific failure to surface beyond what the adapter already reports.
func (h *harnessWrapper) Run(ctx context.Context, req harness.Request) (harness.Result, error) {
	res, err := h.inner.Run(ctx, req)
	if err != nil {
		// The subprocess failed; surface the adapter's actionable error and keep
		// whatever raw output we captured.
		return res, err
	}
	res.Text = strings.TrimRight(res.Text, " \t\r\n")
	return res, nil
}

// Factories returns the preset's registry wiring: a single-entry map binding the
// pi harness name to Factory, ready to merge into harness.Options.Factories.
// This is the import-cycle-safe seam — the registry (internal/harness) must not
// import this package, so the wiring is supplied the other way: a caller
// (cli/pipeline, AIX-0009+) does
//
//	reg, err := harness.NewRegistry(cfg, harness.Options{Factories: pi.Factories()})
//
// so the "pi" harness is built by this preset while every other harness falls
// back to the generic adapter. Keeping the map construction here means the name
// and Factory are paired in one place. (No production caller wires this yet.)
func Factories() map[string]harness.Factory {
	return map[string]harness.Factory{Name: Factory}
}

// Available is a preflight that checks the pi CLI is resolvable on PATH. It
// returns a clear, actionable error (naming the tool and an availability hint)
// when it is not, so callers can fail fast before attempting a run. A nil return
// means the binary was found; it does not validate the binary's version or auth.
func Available() error {
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("pi CLI not found on PATH: install the pi coding agent (%s) and ensure %q is on your PATH: %w",
			installHint, command, err)
	}
	return nil
}
