package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// retryHarness wraps a Harness and retries ONLY transient invocation failures
// (process spawn, timeout, empty/unparseable output — see FailureKind.Transient)
// up to cfg.MaxAttempts total attempts, sleeping cfg.Backoff between them. It
// never retries a hard/semantic failure (a non-zero exit, a deliberate agent
// error result, or any unclassified error), and it never retries a SUCCESS —
// even a successful run whose result is a reviewer's "not approved" verdict,
// because that is a valid result, not a failure (the harness returns nil error
// for it; only the caller interprets the text).
//
// Composition (AIX-0014): the registry wraps the REAL cli harness in this, i.e.
// retry(cli). The dry-run wrapper is applied OUTSIDE/instead — a dry run returns
// a deterministic placeholder with no error, so there is nothing to retry, and it
// must not be wrapped (the registry handles that ordering).
//
// The final error after exhausting attempts is the LAST attempt's error, wrapped
// with the attempt count so the user sees how many tries happened and why it
// ultimately failed (the distinction between "couldn't run the agent" and "agent
// ran but produced bad output" is preserved from the underlying RunError).
type retryHarness struct {
	inner    Harness
	attempts int           // total attempts including the first; >= 1.
	backoff  time.Duration // base delay between attempts; >= 0.
	logger   Logger        // optional; logs each attempt + backoff. nil is fine.
	// sleep is the backoff sleep, injectable so tests do not actually wait. It must
	// honor ctx cancellation. Defaults to ctxSleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// newRetry wraps inner with the retry policy from cfg. An attempts count below 1
// is floored to 1 (no retry) so a hand-built config can never disable execution
// entirely; a negative backoff is floored to 0. logger may be nil. The dry-run
// wrapper is applied by the registry AROUND/instead of this, never inside it.
func newRetry(inner Harness, cfg config.Retry, logger Logger) *retryHarness {
	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	backoff := cfg.Backoff.Std()
	if backoff < 0 {
		backoff = 0
	}
	return &retryHarness{
		inner:    inner,
		attempts: attempts,
		backoff:  backoff,
		logger:   logger,
		sleep:    ctxSleep,
	}
}

// Name returns the wrapped harness's name unchanged, so role→harness resolution
// and logging are identical with or without retry.
func (r *retryHarness) Name() string { return r.inner.Name() }

// Unwrap returns the wrapped harness, so callers (and tests) can reach the
// underlying preset/adapter through the retry layer.
func (r *retryHarness) Unwrap() Harness { return r.inner }

// Run invokes the inner harness, retrying transient failures per the policy. On
// success it returns immediately. On a hard failure it returns immediately
// (never retried). On a transient failure it logs the attempt, sleeps the
// backoff, and retries until the attempt budget is exhausted, then returns the
// last error. A context cancellation (during the call or the backoff) stops
// retrying at once.
func (r *retryHarness) Run(ctx context.Context, req Request) (Result, error) {
	name := r.inner.Name()
	var lastRes Result
	var lastErr error

	for attempt := 1; attempt <= r.attempts; attempt++ {
		res, err := r.inner.Run(ctx, req)
		if err == nil {
			// Success — including a "valid result" the caller may later judge as "not
			// approved". Never retried; that is a decision for the caller, not a
			// transport failure.
			return res, nil
		}
		lastRes, lastErr = res, err

		kind := classifyKind(err)
		// A hard/semantic failure (or anything we cannot classify) is not retried.
		if !kind.Transient() {
			return res, err
		}
		// Out of budget: surface the final transient error, annotated with the count.
		if attempt == r.attempts {
			break
		}
		// Context already canceled? Stop now rather than sleeping/retrying.
		if ctx.Err() != nil {
			return res, err
		}

		r.logf("[retry] harness=%s role=%s attempt=%d/%d failed (%s); retrying in %s: %v",
			name, roleLabel(req.Role), attempt, r.attempts, kind, r.backoff, err)

		if serr := r.sleep(ctx, r.backoff); serr != nil {
			// Cancelled during backoff: return the last invocation error, not the
			// sleep's context error, so the user sees WHY the agent was failing.
			return res, err
		}
	}

	// Exhausted all attempts on a transient failure. Wrap with the count so the
	// message says how many tries happened; the underlying RunError (and thus the
	// spawn-vs-bad-output distinction) is preserved for errors.As/Is.
	return lastRes, fmt.Errorf("harness %q: failed after %d attempt(s): %w", name, r.attempts, lastErr)
}

// logf logs an attempt/backoff line when a logger is configured; a nil logger is
// a no-op so retry never depends on logging being wired.
func (r *retryHarness) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Infof(format, args...)
	}
}

// ctxSleep waits d, returning early with the context error if ctx is cancelled
// first. A non-positive d returns immediately (still honoring an already-cancelled
// ctx). It is the default retryHarness.sleep; tests inject a no-op.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// roleLabel returns a non-empty label for logging an invocation's role, falling
// back to "-" when the request did not set one.
func roleLabel(role string) string {
	if role == "" {
		return "-"
	}
	return role
}
