// Package claude is the first-class Claude Code (`claude`) harness preset. It is
// a thin layer over the generic declarative CLI adapter (internal/harness): the
// command, args, prompt delivery, timeout, and env all stay config-driven (the
// verified-correct default block lives in internal/config; see CLAUDE.md §5),
// while this package adds the claude-specific concerns the generic adapter
// cannot express:
//
//   - robust extraction of the final assistant text from claude's JSON envelope
//     (`claude -p --output-format json` emits a single result object whose final
//     text is the top-level "result" field), via ParseResult;
//   - surfacing a claude *error envelope* (`is_error: true`) as a Go error
//     instead of silently returning the error text as if it were a result;
//   - a PATH preflight (Available) with an actionable install hint.
//
// # Model & permission-mode passthrough
//
// Nothing about the model or permission mode is hardcoded here. The role config
// supplies both, and the default args template them straight through:
// {{.Model}} accepts an alias (opus / sonnet / haiku / fable) or a full model
// id, and {{.PermissionMode}} carries the role's mode (planner/reviewers use
// "plan", the executor uses "acceptEdits"). They flow into argv unchanged via
// the generic adapter, so changing a role's model or permission mode is purely a
// config edit.
//
// # Sub-agent git safety
//
// Per CLAUDE.md invariant #1, neither the app nor the sub-agent may run write
// git commands. The app never shells out to git at all (read access goes through
// internal/git). For the *sub-agent*, that safety is enforced by the prompts we
// hand it (internal/prompt, AIX-0008), not here. The claude CLI does support a
// defense-in-depth `--disallowedTools "Bash(git *)"` flag (confirmed present in
// the installed CLI), but it is intentionally NOT added to the default args:
// doing so would diverge from the canonical config schema in CLAUDE.md §5, and
// the prompt-level instruction is the authoritative mechanism. A user who wants
// the extra guard can add the flag in their own config.
//
// # Future work
//
// Session resumption and streaming output (--output-format stream-json) are out
// of scope for this preset; "json" returns a single envelope, which is all the
// pipeline needs today.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
)

// errEnvelope marks a parse failure that is a deliberate claude error envelope
// (is_error: true) rather than malformed/empty output. The wrapper uses it to
// classify the failure as HARD (not retried), since re-running an agent that
// returned a real error result just wastes tokens. errors.Is sees it through the
// wrapped ParseResult error.
var errEnvelope = errors.New("claude error envelope")

// Name is the canonical harness key this preset is registered under (it matches
// the "claude" entry in the default config / CLAUDE.md §5). Callers wiring the
// registry use it as the single source of truth for the key, so the string is
// not duplicated at every call site.
const Name = "claude"

// command is the executable this preset drives. It is used only for the PATH
// preflight; the actual command run is whatever the config specifies (normally
// the same name), so the preset never hardcodes the invocation.
const command = "claude"

// installHint points users at the install docs when the binary is missing. Kept
// as a const so the message is consistent across the error paths.
const installHint = "https://claude.com/claude-code"

// Factory builds the claude preset for a harness named in config. It matches
// harness.Factory, so a registry plugs it in via
// harness.Options{Factories: {"claude": claude.Factory}} and every other harness
// keeps building generically. It constructs the generic CLI harness from the
// passed cfg — preserving command/args/promptDelivery/timeout/env exactly as
// configured — and wraps it so claude's JSON envelope is parsed authoritatively
// (see harnessWrapper.Run / ParseResult).
//
// The inner adapter is built with output forced to "text": the generic adapter
// then returns claude's stdout verbatim (in both Result.Text and Result.Raw),
// and this preset owns the JSON decode. That keeps envelope handling — including
// the is_error check, which the generic resultPath extractor does not perform —
// in one place and decoupled from the cfg's resultPath. The user's args
// (`--output-format json`) still drive what claude actually emits; only the
// adapter's interpretation of that stdout is overridden.
func Factory(name string, cfg config.Harness) (harness.Harness, error) {
	inner, err := harness.NewCLI(name, withTextOutput(cfg))
	if err != nil {
		return nil, fmt.Errorf("claude preset %q: %w", name, err)
	}
	return &harnessWrapper{name: name, inner: inner}, nil
}

// withTextOutput returns a copy of cfg with Output set to "text" and ResultPath
// cleared, so the generic adapter passes stdout through untouched and this
// preset performs the JSON extraction itself. The original cfg is not mutated
// (Default() hands out fresh maps/slices, but copying keeps that contract local
// and obvious). Command, Args, PromptDelivery, Timeout, and Env are unchanged.
func withTextOutput(cfg config.Harness) config.Harness {
	out := cfg
	out.Output = "text"
	out.ResultPath = ""
	return out
}

// harnessWrapper decorates the generic CLI harness with claude-specific result
// handling. It delegates the actual subprocess execution to inner and only
// reinterprets the captured stdout.
type harnessWrapper struct {
	name  string
	inner harness.Harness
}

// Name implements harness.Harness, returning the configured harness name so
// role→harness resolution and logging are unaffected by the preset.
func (h *harnessWrapper) Name() string { return h.name }

// Run implements harness.Harness. It runs the generic adapter (which executes
// claude with the configured args/model/permission-mode), then parses the raw
// stdout with ParseResult to extract the final assistant text and to convert a
// claude error envelope (is_error: true) into a Go error.
//
// Error handling:
//   - If the adapter itself errors (exec failure, timeout, non-zero exit), that
//     error is returned as-is, with Result.Raw preserved for logging.
//   - On a clean exit, ParseResult decides success: a missing/empty "result" or
//     an error envelope yields an error (so we never silently return empty or
//     error text as if it were a real result). Result.Raw always holds the full
//     stdout.
func (h *harnessWrapper) Run(ctx context.Context, req harness.Request) (harness.Result, error) {
	res, err := h.inner.Run(ctx, req)
	if err != nil {
		// The subprocess failed before producing a usable envelope; surface the
		// adapter's actionable error and keep whatever raw output we captured.
		return res, err
	}

	text, perr := ParseResult(res.Raw)
	if perr != nil {
		// The agent RAN but its envelope could not be turned into a usable result
		// (empty output, undecodable JSON, or no "result" text). That is "bad
		// output" — transient — so the retry wrapper (internal/harness) may re-run
		// it. An explicit claude error envelope (is_error: true) is a deliberate
		// failure result, not a transport hiccup, so it is classified hard and not
		// retried. Result.Raw is preserved for logging either way.
		wrapped := fmt.Errorf("harness %q: %w", h.name, perr)
		if errors.Is(perr, errEnvelope) {
			return res, harness.HardError(wrapped)
		}
		return res, harness.BadOutputError(wrapped)
	}
	res.Text = text
	return res, nil
}

// resultEnvelope is the subset of claude's `--output-format json` object this
// preset cares about. Unknown/extra fields (session_id, total_cost_usd, usage,
// duration_ms, num_turns, …) are ignored by encoding/json, so the parser is
// resilient to envelope additions across CLI versions.
type resultEnvelope struct {
	// Result is the final assistant text on a successful run.
	Result string `json:"result"`
	// IsError reports that claude returned an error envelope; when true, Result
	// (or Subtype) typically carries the failure detail.
	IsError bool `json:"is_error"`
	// Subtype is a short machine-readable status (e.g. "success",
	// "error_during_execution"), included in error messages for context.
	Subtype string `json:"subtype"`
	// Type is the envelope kind (e.g. "result"); kept for clearer diagnostics.
	Type string `json:"type"`
}

// ParseResult decodes a claude `--output-format json` envelope and returns the
// final assistant text. It is standalone and unit-testable: hand it the raw
// stdout bytes.
//
// Behavior:
//   - Extra/unknown envelope fields are tolerated (forward-compatible).
//   - An error envelope (is_error: true) yields an error that includes the
//     subtype and any error text, rather than returning that text as a result.
//   - A missing or empty "result" on a non-error envelope is also an error, so
//     callers never mistake an empty envelope for a successful empty result.
//
// The returned text is the verbatim "result" string (not trimmed): callers that
// want it trimmed can do so, but we preserve the agent's output exactly.
func ParseResult(raw []byte) (string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", fmt.Errorf("claude returned empty output (expected a JSON result envelope)")
	}
	var env resultEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("decoding claude JSON envelope: %w", err)
	}
	if env.IsError {
		return "", fmt.Errorf("%w%s%s", errEnvelope,
			subtypeSuffix(env.Subtype), messageSuffix(env.Result))
	}
	if env.Result == "" {
		return "", fmt.Errorf("claude envelope has no \"result\" text%s (is_error=false)", subtypeSuffix(env.Subtype))
	}
	return env.Result, nil
}

// subtypeSuffix renders a " (subtype: X)" fragment for error messages, or "" if
// the subtype is absent, so messages read cleanly either way.
func subtypeSuffix(subtype string) string {
	if strings.TrimSpace(subtype) == "" {
		return ""
	}
	return fmt.Sprintf(" (subtype: %s)", subtype)
}

// messageSuffix renders a ": <msg>" fragment carrying any error text from the
// envelope's result field, or "" when there is none.
func messageSuffix(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return fmt.Sprintf(": %s", msg)
}

// Factories returns the preset's registry wiring: a single-entry map binding the
// claude harness name to Factory, ready to merge into harness.Options.Factories.
// This is the import-cycle-safe seam — the registry (internal/harness) must not
// import this package, so the wiring is supplied the other way: a caller
// (cli/pipeline, AIX-0009+) does
//
//	reg, err := harness.NewRegistry(cfg, harness.Options{Factories: claude.Factories()})
//
// so the "claude" harness is built by this preset while every other harness
// falls back to the generic adapter. Keeping the map construction here means the
// name and Factory are paired in one place.
func Factories() map[string]harness.Factory {
	return map[string]harness.Factory{Name: Factory}
}

// Available is a preflight that checks the claude CLI is resolvable on PATH. It
// returns a clear, actionable error (naming the tool and the install URL) when
// it is not, so callers can fail fast before attempting a run. A nil return
// means the binary was found; it does not validate the binary's version or auth.
func Available() error {
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("claude CLI not found on PATH: install Claude Code (%s) and ensure %q is on your PATH: %w",
			installHint, command, err)
	}
	return nil
}
