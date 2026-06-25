package harness

import (
	"context"
	"fmt"
)

// promptPreviewLen bounds how much of the prompt the dry-run wrapper echoes, so
// the log stays readable even for very long prompts.
const promptPreviewLen = 120

// dryRunHarness wraps a Harness and short-circuits Run: it logs the invocation
// it *would* have made and returns a deterministic placeholder Result without
// executing the underlying harness at all. This backs the global --dry-run flag,
// letting the whole pipeline be exercised with no tokens spent and no real agent
// (or command) invoked.
type dryRunHarness struct {
	inner  Harness
	logger Logger
}

// newDryRun wraps inner so its Run never executes. logger may be nil.
func newDryRun(inner Harness, logger Logger) *dryRunHarness {
	return &dryRunHarness{inner: inner, logger: logger}
}

// Name returns the wrapped harness's name unchanged, so role→harness resolution
// and logging are identical to a real run.
func (d *dryRunHarness) Name() string { return d.inner.Name() }

// Unwrap returns the wrapped harness, so callers (and tests) can reach the
// underlying preset/adapter through the dry-run layer.
func (d *dryRunHarness) Unwrap() Harness { return d.inner }

// Run logs the intended invocation and returns a placeholder Result. The
// underlying harness is never called, so no subprocess runs. The returned Text
// is deterministic so dry-run pipeline tests can assert on it.
func (d *dryRunHarness) Run(_ context.Context, req Request) (Result, error) {
	name := d.inner.Name()
	if d.logger != nil {
		d.logger.Infof("[dry-run] harness=%s model=%s workdir=%s prompt=%q",
			name, req.Model, req.WorkDir, truncate(req.Prompt, promptPreviewLen))
	}
	return Result{
		Text:     fmt.Sprintf("[dry-run] %s would run", name),
		Raw:      nil,
		ExitCode: 0,
		Duration: 0,
	}, nil
}

// truncate shortens s to at most n runes, appending an ellipsis marker when it
// had to cut. It counts runes (not bytes) so multibyte prompts are not split
// mid-character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
