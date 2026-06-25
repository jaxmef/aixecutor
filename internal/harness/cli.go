package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// Prompt-delivery modes (config.Harness.PromptDelivery). They decide how the
// rendered prompt reaches the subprocess.
const (
	deliveryArg   = "arg"   // prompt is templated directly into args via {{.Prompt}}
	deliveryStdin = "stdin" // prompt is written to the process's stdin
	deliveryFile  = "file"  // prompt is written to a temp file, path exposed as {{.PromptFile}}
)

// Output formats (config.Harness.Output).
const (
	outputText = "text" // Result.Text is stdout verbatim
	outputJSON = "json" // Result.Text is the resultPath field of decoded stdout
)

// stderrTailBytes bounds how much trailing stderr is quoted in error messages,
// so a chatty failing agent does not produce an unreadable error.
const stderrTailBytes = 2048

// argTemplateData is the render context exposed to each templated arg. It is the
// documented surface for harness Args templates (CLAUDE.md §4.2): the request
// fields plus PromptFile, which is populated only for promptDelivery: file.
type argTemplateData struct {
	Prompt         string
	Model          string
	PermissionMode string
	WorkDir        string
	// PromptFile is the path to the temp prompt file; empty unless
	// PromptDelivery is "file".
	PromptFile string
}

// runResult is the low-level outcome of executing the command, returned by the
// injectable runner. It separates "the process ran" from output parsing so the
// os/exec call can be faked in tests without reimplementing parsing.
type runResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	duration time.Duration
	// timedOut reports that the run was killed because its context deadline
	// (derived from the timeout) expired, as opposed to a normal non-zero exit.
	timedOut bool
}

// runnerFunc executes cmd (already configured with Dir/Env/args), feeding stdin
// to the process, and returns its captured output. It is a field on cliHarness
// so tests can inject a fake and exercise the parsing/delivery logic without
// spawning a real process; the default is execRunner, which is itself covered by
// tests via the TestHelperProcess re-exec pattern.
type runnerFunc func(ctx context.Context, cmd *exec.Cmd, stdin []byte) (runResult, error)

// cliHarness is the generic, declarative adapter. It drives any coding-agent CLI
// purely from a config.Harness: render args, deliver the prompt, run the
// command with the requested workdir/env/timeout, then parse stdout. It bakes in
// no agent-specific behavior — that belongs to presets (AIX-0004/0005).
type cliHarness struct {
	name string
	cfg  config.Harness
	// argTmpls are the pre-parsed arg templates, compiled once in newCLIHarness
	// so a malformed template fails at construction, not at every Run.
	argTmpls []*template.Template
	// runner executes the built command; injectable for tests.
	runner runnerFunc
}

// newCLIHarness builds a generic CLI harness from a config.Harness entry. It
// validates the static parts of the config (known type, delivery, output) and
// pre-parses the arg templates so configuration errors surface at registry-build
// time with a clear, harness-named message.
func newCLIHarness(name string, cfg config.Harness) (*cliHarness, error) {
	if cfg.Type != "" && cfg.Type != "cli" {
		return nil, fmt.Errorf("harness %q: unsupported type %q (only %q is supported)", name, cfg.Type, "cli")
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("harness %q: command must be set", name)
	}
	switch cfg.PromptDelivery {
	case deliveryArg, deliveryStdin, deliveryFile:
	default:
		return nil, fmt.Errorf("harness %q: promptDelivery %q is invalid; must be %q, %q, or %q",
			name, cfg.PromptDelivery, deliveryArg, deliveryStdin, deliveryFile)
	}
	switch cfg.Output {
	case outputText:
	case outputJSON:
		if strings.TrimSpace(cfg.ResultPath) == "" {
			return nil, fmt.Errorf("harness %q: output is %q but resultPath is empty; set the JSON field holding the final text", name, outputJSON)
		}
	default:
		return nil, fmt.Errorf("harness %q: output %q is invalid; must be %q or %q", name, cfg.Output, outputText, outputJSON)
	}

	tmpls := make([]*template.Template, len(cfg.Args))
	for i, arg := range cfg.Args {
		t, err := template.New(fmt.Sprintf("%s-arg-%d", name, i)).Option("missingkey=error").Parse(arg)
		if err != nil {
			return nil, fmt.Errorf("harness %q: parsing arg template %d (%q): %w", name, i, arg, err)
		}
		tmpls[i] = t
	}

	return &cliHarness{
		name:     name,
		cfg:      cfg,
		argTmpls: tmpls,
		runner:   execRunner,
	}, nil
}

// NewCLI builds the generic, declarative CLI harness from a config.Harness and
// returns it as a Harness. It is the public seam presets use to reuse the
// generic runner (arg rendering, prompt delivery, timeout/workdir/env handling,
// process-group kill) without reimplementing it: a preset (AIX-0004 claude,
// AIX-0005 pi) constructs one of these and wraps it to add agent-specific result
// handling. Construction validates the static config and pre-parses the arg
// templates, so a bad template/type/delivery surfaces here with a harness-named
// error rather than at Run time. Presets live in sub-packages that import this
// one, so exposing the constructor here avoids an import cycle.
func NewCLI(name string, cfg config.Harness) (Harness, error) {
	return newCLIHarness(name, cfg)
}

// Name implements Harness.
func (h *cliHarness) Name() string { return h.name }

// Run implements Harness: render args, deliver the prompt, execute the command
// with the resolved deadline/workdir/env, then parse stdout per the configured
// output format. It returns an actionable error on template, exec, timeout,
// non-zero-exit, or parse failures; Result.Duration is always populated.
func (h *cliHarness) Run(ctx context.Context, req Request) (Result, error) {
	// Resolve the deadline: an explicit per-request timeout wins; otherwise the
	// harness's configured timeout; otherwise no deadline.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = h.cfg.Timeout.Std()
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Prompt delivery. For "file" we materialize a temp file under the OS temp
	// dir (never inside the user's repo) and remove it after the run.
	td := argTemplateData{
		Prompt:         req.Prompt,
		Model:          req.Model,
		PermissionMode: req.PermissionMode,
		WorkDir:        req.WorkDir,
	}
	var stdin []byte
	switch h.cfg.PromptDelivery {
	case deliveryStdin:
		stdin = []byte(req.Prompt)
	case deliveryFile:
		path, cleanup, err := writePromptFile(req.Prompt)
		if err != nil {
			return Result{}, fmt.Errorf("harness %q: writing prompt file: %w", h.name, err)
		}
		defer cleanup()
		td.PromptFile = path
	}

	args, err := h.renderArgs(td)
	if err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(runCtx, h.cfg.Command, args...)
	cmd.Dir = req.WorkDir
	cmd.Env = mergedEnv(h.cfg.Env, req.Env)

	rr, err := h.runner(runCtx, cmd, stdin)

	res := Result{
		Raw:      rr.stdout,
		ExitCode: rr.exitCode,
		Duration: rr.duration,
	}

	// Classify each failure mode with a FailureKind so the retry wrapper (retry.go)
	// can decide whether to retry — transient (spawn/timeout/bad-output) vs hard —
	// without re-parsing the message. The messages themselves are unchanged.
	if rr.timedOut {
		return res, timeoutError(fmt.Errorf("harness %q: timed out after %s (limit %s); process group killed%s",
			h.name, rr.duration.Round(time.Millisecond), timeout, stderrTail(rr.stderr)))
	}
	if err != nil {
		// The process could not be run to completion (Start failed, or a non-exit
		// Wait error): "couldn't run the agent" — transient.
		return res, spawnError(fmt.Errorf("harness %q: running %q: %w%s", h.name, h.cfg.Command, err, stderrTail(rr.stderr)))
	}
	if rr.exitCode != 0 {
		// The agent RAN and chose to exit non-zero. This is a real, semantic failure
		// — hard, not retried (re-running an agent that deterministically errors out
		// just wastes tokens).
		return res, hardError(fmt.Errorf("harness %q: command %q exited with code %d%s",
			h.name, h.cfg.Command, rr.exitCode, stderrTail(rr.stderr)))
	}

	text, err := h.parseOutput(rr.stdout)
	if err != nil {
		// The agent ran but its output could not be parsed: bad output — transient.
		return res, badOutputError(err)
	}
	if strings.TrimSpace(text) == "" {
		// The agent ran but produced no usable result: bad output — transient.
		return res, badOutputError(fmt.Errorf("harness %q: agent ran but produced no usable result (empty output)", h.name))
	}
	res.Text = text
	return res, nil
}

// renderArgs executes each pre-parsed arg template against td and returns the
// rendered argument vector. Execution errors (e.g. an arg referencing
// {{.PromptFile}} when delivery is not "file") are reported with the harness
// name and offending template index.
func (h *cliHarness) renderArgs(td argTemplateData) ([]string, error) {
	args := make([]string, len(h.argTmpls))
	var buf bytes.Buffer
	for i, t := range h.argTmpls {
		buf.Reset()
		if err := t.Execute(&buf, td); err != nil {
			return nil, fmt.Errorf("harness %q: rendering arg template %d (%q): %w", h.name, i, h.cfg.Args[i], err)
		}
		args[i] = buf.String()
	}
	return args, nil
}

// parseOutput turns captured stdout into Result.Text per the configured output
// format: text returns stdout verbatim; json decodes stdout and extracts the
// dot-separated resultPath, tolerating (and preserving in Raw) any extra fields.
func (h *cliHarness) parseOutput(stdout []byte) (string, error) {
	switch h.cfg.Output {
	case outputText:
		return string(stdout), nil
	case outputJSON:
		var decoded any
		if err := json.Unmarshal(stdout, &decoded); err != nil {
			return "", fmt.Errorf("harness %q: decoding JSON output: %w", h.name, err)
		}
		val, err := extractPath(decoded, h.cfg.ResultPath)
		if err != nil {
			return "", fmt.Errorf("harness %q: extracting resultPath %q: %w", h.name, h.cfg.ResultPath, err)
		}
		return val, nil
	default:
		// Unreachable: newCLIHarness validates Output.
		return "", fmt.Errorf("harness %q: unknown output format %q", h.name, h.cfg.Output)
	}
}

// extractPath walks a dot-separated path (e.g. "result" or "data.result")
// through a decoded JSON value and returns the leaf as a string. Each segment
// must traverse a JSON object; the leaf may be a string, or a number/bool, which
// is rendered to its textual form for convenience.
func extractPath(v any, path string) (string, error) {
	segments := strings.Split(path, ".")
	cur := v
	for i, seg := range segments {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("path segment %q: parent is not a JSON object", strings.Join(segments[:i+1], "."))
		}
		next, ok := obj[seg]
		if !ok {
			return "", fmt.Errorf("key %q not found", strings.Join(segments[:i+1], "."))
		}
		cur = next
	}
	switch s := cur.(type) {
	case string:
		return s, nil
	case nil:
		return "", fmt.Errorf("value at %q is null", path)
	case float64:
		return fmt.Sprintf("%v", s), nil
	case bool:
		return fmt.Sprintf("%v", s), nil
	default:
		return "", fmt.Errorf("value at %q is not a string (got %T)", path, cur)
	}
}

// mergedEnv composes the subprocess environment: the current process env, then
// the harness's configured env, then the per-request env (request wins). Later
// assignments override earlier ones for the same key.
func mergedEnv(harnessEnv, reqEnv map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for k, v := range harnessEnv {
		merged[k] = v
	}
	for k, v := range reqEnv {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// writePromptFile writes prompt to a temp file under the OS temp dir and returns
// its path plus an idempotent cleanup func. The file is created outside the
// user's repo by construction (os.CreateTemp uses os.TempDir), satisfying the
// ticket's "never inside the user's repo" requirement.
func writePromptFile(prompt string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "aixecutor-prompt-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	if _, err := f.WriteString(prompt); err != nil {
		f.Close()
		os.Remove(name)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", func() {}, err
	}
	return name, func() { os.Remove(name) }, nil
}

// stderrTail returns a quoted, length-bounded tail of stderr suitable for
// appending to an error message, or "" when stderr is empty. The leading
// newline keeps multi-line errors readable in the terminal.
func stderrTail(stderr []byte) string {
	trimmed := bytes.TrimSpace(stderr)
	if len(trimmed) == 0 {
		return ""
	}
	if len(trimmed) > stderrTailBytes {
		trimmed = trimmed[len(trimmed)-stderrTailBytes:]
	}
	return fmt.Sprintf("\nstderr: %s", trimmed)
}
