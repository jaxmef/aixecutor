package log

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jaxmef/aixecutor/internal/harness"
)

// invocationHarness wraps a harness.Harness so that EVERY invocation is logged
// with a structured record — role, harness, model, workdir, duration, exit code —
// and a pointer to the full stdout/stderr persisted under the run's logs/ dir
// (<run>/logs/NNN-<role>.out). This is the AIX-0014 "every harness invocation is
// logged + a pointer to full output" requirement, kept in one decorator so phase
// code stays clean.
//
// Redaction: the request's Env VALUES are never logged. Only the (sorted) keys are
// recorded, and keys that look like secrets are suffixed "(redacted)" — so a
// secret value cannot leak into the logs (asserted by a test).
//
// It is applied OVER the retry/dry-run-wrapped harness (so one log line is emitted
// per real attempt by the inner retry wrapper's own logging, while THIS records
// the final result of the call the phase made, with its persisted raw output).
type invocationHarness struct {
	inner  harness.Harness
	logger *Logger
}

// WrapHarness decorates h so its Run is logged and its raw output persisted via
// logger. A nil logger returns h unchanged (no overhead, no decoration), so
// callers need not branch. The returned harness has the same Name as h.
func WrapHarness(h harness.Harness, logger *Logger) harness.Harness {
	if logger == nil || h == nil {
		return h
	}
	return &invocationHarness{inner: h, logger: logger}
}

// Name returns the wrapped harness's name unchanged so role→harness resolution is
// unaffected by the decoration.
func (h *invocationHarness) Name() string { return h.inner.Name() }

// Run invokes the inner harness, persists the raw output to a per-invocation file
// under the run logs dir, and logs a structured record of the call. Both success
// and failure are logged (failures at warn level, with the error); the underlying
// result/error is returned unchanged so behavior is identical with or without the
// wrapper.
func (h *invocationHarness) Run(ctx context.Context, req harness.Request) (harness.Result, error) {
	// One seq per logical invocation: WrapHarness is the outermost decorator (it
	// wraps retry(cli)), so retries happen INSIDE h.inner.Run and never allocate a
	// new seq — the started/completed records and the .out file all share this seq.
	seq := h.logger.nextSeq()
	h.logger.Info("harness invocation started",
		"role", roleAttr(req.Role),
		"harness", h.inner.Name(),
		"model", req.Model,
		"seq", seq,
	)

	res, err := h.inner.Run(ctx, req)

	outPath := h.persistRaw(seq, req.Role, res.Raw)

	attrs := []any{
		"role", roleAttr(req.Role),
		"harness", h.inner.Name(),
		"model", req.Model,
		"seq", seq,
		"workdir", req.WorkDir,
		"duration", res.Duration.Round(time.Millisecond).String(),
		"exitCode", res.ExitCode,
	}
	if outPath != "" {
		attrs = append(attrs, "output", outPath)
	}
	if keys := redactedEnvKeys(req.Env); keys != nil {
		attrs = append(attrs, "envKeys", keys)
	}

	if err != nil {
		// Record WHY it failed (the classified kind, when present) and the message,
		// so the log distinguishes "couldn't run the agent" from "agent ran but
		// produced bad output" without re-parsing.
		attrs = append(attrs, "kind", harness.Classify(err).String(), "error", err.Error())
		h.logger.Warn("harness invocation failed", attrs...)
		return res, err
	}
	h.logger.Info("harness invocation completed", attrs...)
	return res, nil
}

// persistRaw writes the invocation's raw stdout to <logsDir>/NNN-<role>.out and
// returns the path, so the structured log line can point at the full output. The
// zero-padded seq PREFIX makes a plain `ls` sort by execution order. It is
// best-effort: with no logs dir attached, or no raw output, or a write failure, it
// returns "" and the log simply omits the pointer (logging must never fail a run).
func (h *invocationHarness) persistRaw(seq int, role string, raw []byte) string {
	logsDir := h.logger.LogsDir()
	if logsDir == "" || len(raw) == 0 {
		return ""
	}
	name := fmt.Sprintf("%03d-%s.out", seq, safeRole(role))
	path := filepath.Join(logsDir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		// Surface the failure in the log but do not fail the invocation.
		h.logger.Warn("persisting harness raw output failed", "path", path, "error", err.Error())
		return ""
	}
	return path
}

// roleAttr returns a non-empty role label for the structured log, falling back to
// "unknown" when the request did not set one.
func roleAttr(role string) string {
	if role == "" {
		return "unknown"
	}
	return role
}

// safeRole sanitizes a role label for use in a filename: it keeps it to a single,
// filesystem-safe segment so a stray role string can never escape the logs dir or
// introduce separators. Empty → "invocation".
func safeRole(role string) string {
	if role == "" {
		return "invocation"
	}
	cleaned := filepath.Base(filepath.Clean("/" + role))
	if cleaned == "." || cleaned == string(filepath.Separator) || cleaned == "" {
		return "invocation"
	}
	return cleaned
}
