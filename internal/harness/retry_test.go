package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// noSleep is an injectable sleep that records how many backoffs happened without
// actually waiting, so retry tests are fast and deterministic. It honors ctx
// cancellation like the real ctxSleep.
type noSleep struct{ calls int }

func (n *noSleep) sleep(ctx context.Context, _ time.Duration) error {
	n.calls++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// newTestRetry wraps inner with a retry policy and a no-op (counting) sleep so the
// test never waits. It returns the wrapper and the sleep recorder.
func newTestRetry(inner Harness, attempts int, backoff time.Duration) (*retryHarness, *noSleep) {
	ns := &noSleep{}
	r := newRetry(inner, config.Retry{MaxAttempts: attempts, Backoff: config.Duration(backoff)}, nil)
	r.sleep = ns.sleep
	return r, ns
}

// fixedHarness returns a fixed (result,err) on every Run and counts calls, so
// tests can assert exactly how many attempts the retry wrapper made. Unlike Mock,
// it does not consume a script — every call yields the same outcome, which is what
// "transient failure retried up to the cap" needs.
type fixedHarness struct {
	name  string
	res   Result
	err   error
	calls int
}

func (h *fixedHarness) Name() string { return h.name }
func (h *fixedHarness) Run(context.Context, Request) (Result, error) {
	h.calls++
	return h.res, h.err
}

func TestRetryTransientRetriedUpToCap(t *testing.T) {
	// A transient failure (spawn) on every attempt must be retried until the
	// attempt budget is exhausted: 3 attempts ⇒ 3 calls, 2 backoffs.
	inner := &fixedHarness{name: "h", err: spawnError(errors.New("couldn't run the agent"))}
	r, ns := newTestRetry(inner, 3, time.Second)

	_, err := r.Run(context.Background(), Request{Role: "executor"})
	if err == nil {
		t.Fatal("expected the transient failure to surface after the cap, got nil")
	}
	if inner.calls != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", inner.calls)
	}
	if ns.calls != 2 {
		t.Errorf("backoffs = %d, want 2 (between the 3 attempts)", ns.calls)
	}
	// The final error names the attempt count and preserves the spawn classification.
	if !strings.Contains(err.Error(), "after 3 attempt(s)") {
		t.Errorf("error %q should report the attempt count", err.Error())
	}
	if Classify(err) != FailureSpawn {
		t.Errorf("final error kind = %v, want spawn (the underlying classification is preserved)", Classify(err))
	}
}

func TestRetryEachTransientKindIsRetried(t *testing.T) {
	// Spawn ("couldn't run"), timeout, and bad-output ("agent ran but produced no
	// usable result") are all transient and must be retried; the distinction is
	// visible in the surfaced error's classification.
	cases := []struct {
		name string
		err  error
		kind FailureKind
	}{
		{"spawn", spawnError(errors.New("couldn't run the agent")), FailureSpawn},
		{"timeout", timeoutError(errors.New("timed out")), FailureTimeout},
		{"bad-output", badOutputError(errors.New("agent ran but produced no usable result")), FailureBadOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &fixedHarness{name: "h", err: tc.err}
			r, ns := newTestRetry(inner, 2, time.Second)
			_, err := r.Run(context.Background(), Request{})
			if inner.calls != 2 {
				t.Errorf("%s: attempts = %d, want 2", tc.name, inner.calls)
			}
			if ns.calls != 1 {
				t.Errorf("%s: backoffs = %d, want 1", tc.name, ns.calls)
			}
			if Classify(err) != tc.kind {
				t.Errorf("%s: kind = %v, want %v", tc.name, Classify(err), tc.kind)
			}
		})
	}
}

func TestRetryHardFailureNotRetried(t *testing.T) {
	// A hard/semantic failure (a real non-zero exit) must NOT be retried: exactly
	// one attempt, no backoff, and the error is returned verbatim (not wrapped with
	// an attempt count).
	hard := hardError(errors.New("command exited with code 2"))
	inner := &fixedHarness{name: "h", err: hard}
	r, ns := newTestRetry(inner, 5, time.Second)

	_, err := r.Run(context.Background(), Request{})
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (hard failure not retried)", inner.calls)
	}
	if ns.calls != 0 {
		t.Errorf("backoffs = %d, want 0", ns.calls)
	}
	if !errors.Is(err, hard) {
		t.Errorf("error = %v, want the hard error returned as-is", err)
	}
	if strings.Contains(err.Error(), "attempt(s)") {
		t.Errorf("hard failure should not be wrapped with an attempt count: %q", err.Error())
	}
}

func TestRetryUnclassifiedErrorNotRetried(t *testing.T) {
	// An error with no RunError classification (e.g. a foreign error) is treated as
	// hard and never retried — fail safe.
	foreign := errors.New("something we don't understand")
	inner := &fixedHarness{name: "h", err: foreign}
	r, _ := newTestRetry(inner, 3, time.Second)

	_, err := r.Run(context.Background(), Request{})
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (unclassified treated as hard)", inner.calls)
	}
	if !errors.Is(err, foreign) {
		t.Errorf("error = %v, want the foreign error returned as-is", err)
	}
}

func TestRetrySuccessNotRetried(t *testing.T) {
	// A successful run is returned immediately; the result is not re-run.
	inner := &fixedHarness{name: "h", res: Result{Text: "ok"}}
	r, ns := newTestRetry(inner, 5, time.Second)

	res, err := r.Run(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (success not retried)", inner.calls)
	}
	if ns.calls != 0 {
		t.Errorf("backoffs = %d, want 0", ns.calls)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want ok", res.Text)
	}
}

func TestRetryNotApprovedResultIsNotAFailure(t *testing.T) {
	// The load-bearing distinction (AIX-0014): a SUCCESSFUL run whose result text is
	// a reviewer's "not approved" verdict is a VALID RESULT, not a failure — the
	// harness returns a nil error, so retry returns it immediately and never re-runs
	// it. Only the caller interprets the text.
	notApproved := Result{Text: "verdict: not approved\nfindings: ...", Raw: []byte("...")}
	inner := &fixedHarness{name: "reviewer", res: notApproved} // nil err = success
	r, ns := newTestRetry(inner, 3, time.Second)

	res, err := r.Run(context.Background(), Request{Role: "subtask-reviewer"})
	if err != nil {
		t.Fatalf("a not-approved result must be a success (nil error), got: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (a valid result is never retried)", inner.calls)
	}
	if ns.calls != 0 {
		t.Errorf("backoffs = %d, want 0", ns.calls)
	}
	if !strings.Contains(res.Text, "not approved") {
		t.Errorf("result text not preserved: %q", res.Text)
	}
}

func TestRetryMaxAttemptsOneMeansNoRetry(t *testing.T) {
	// MaxAttempts=1 disables retry even for a transient failure: one attempt only.
	inner := &fixedHarness{name: "h", err: timeoutError(errors.New("timed out"))}
	r, ns := newTestRetry(inner, 1, time.Second)

	if _, err := r.Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected the failure to surface")
	}
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (maxAttempts=1 ⇒ no retry)", inner.calls)
	}
	if ns.calls != 0 {
		t.Errorf("backoffs = %d, want 0", ns.calls)
	}
}

func TestRetrySpawnVsBadOutputDistinctionVisible(t *testing.T) {
	// The two transient buckets the ticket calls out — "couldn't run the agent"
	// (spawn) vs "agent ran but produced bad output" (bad-output) — stay
	// distinguishable through the retry wrapper, both in classification and message.
	spawn := &fixedHarness{name: "h", err: spawnError(errors.New("couldn't run the agent"))}
	rs, _ := newTestRetry(spawn, 1, 0)
	_, serr := rs.Run(context.Background(), Request{})
	if Classify(serr) != FailureSpawn || !strings.Contains(serr.Error(), "couldn't run the agent") {
		t.Errorf("spawn failure not distinguishable: kind=%v msg=%q", Classify(serr), serr)
	}

	bad := &fixedHarness{name: "h", err: badOutputError(errors.New("agent ran but produced no usable result"))}
	rb, _ := newTestRetry(bad, 1, 0)
	_, berr := rb.Run(context.Background(), Request{})
	if Classify(berr) != FailureBadOutput || !strings.Contains(berr.Error(), "agent ran but produced no usable result") {
		t.Errorf("bad-output failure not distinguishable: kind=%v msg=%q", Classify(berr), berr)
	}
}

func TestRetryStopsOnContextCancellation(t *testing.T) {
	// A cancelled context during retry stops further attempts and surfaces the
	// invocation error (so the user sees WHY it was failing), not the ctx error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	inner := &fixedHarness{name: "h", err: spawnError(errors.New("boom"))}
	r, ns := newTestRetry(inner, 5, time.Second)

	_, err := r.Run(ctx, Request{})
	if inner.calls != 1 {
		t.Errorf("attempts = %d, want 1 (cancelled ctx stops retry)", inner.calls)
	}
	if ns.calls != 0 {
		t.Errorf("backoffs = %d, want 0 (no sleep after cancellation)", ns.calls)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should be the invocation error, not the ctx error", err.Error())
	}
}

// TestRetryLogsEachAttempt proves each retry (and its backoff) is logged through
// the injected logger, so a transient failure is never silently swallowed.
func TestRetryLogsEachAttempt(t *testing.T) {
	inner := &fixedHarness{name: "h", err: spawnError(errors.New("couldn't run the agent"))}
	logger := &captureLogger{}
	r := newRetry(inner, config.Retry{MaxAttempts: 3, Backoff: config.Duration(time.Second)}, logger)
	r.sleep = (&noSleep{}).sleep

	_, _ = r.Run(context.Background(), Request{Role: "executor"})

	// 3 attempts ⇒ 2 retry-decision log lines (one before each backoff).
	if len(logger.lines) != 2 {
		t.Fatalf("retry log lines = %d, want 2:\n%v", len(logger.lines), logger.lines)
	}
	for _, l := range logger.lines {
		if !strings.Contains(l, "[retry]") || !strings.Contains(l, "role=executor") || !strings.Contains(l, "spawn") {
			t.Errorf("retry log line %q should name the retry, role, and kind", l)
		}
	}
}

// TestRetryComposedInRegistry proves the registry wraps a real (non-dry-run)
// harness in retry, and does NOT wrap a dry-run harness in retry.
func TestRetryComposedInRegistry(t *testing.T) {
	cfg := config.Default()

	reg, err := NewRegistry(cfg, Options{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, _ := reg.Get("claude")
	if _, ok := h.(*retryHarness); !ok {
		t.Errorf("non-dry-run harness = %T, want *retryHarness (retry composed over the real harness)", h)
	}

	dryReg, err := NewRegistry(cfg, Options{DryRun: true})
	if err != nil {
		t.Fatalf("NewRegistry(dry): %v", err)
	}
	dh, _ := dryReg.Get("claude")
	if _, ok := dh.(*dryRunHarness); !ok {
		t.Errorf("dry-run harness = %T, want *dryRunHarness (no retry over dry-run)", dh)
	}
}
