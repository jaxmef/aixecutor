package harness

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Mock is a test double Harness. It records every Request it receives (for
// assertions) and returns scripted Results, optionally simulating an error and a
// delay. It is the backbone of later pipeline tests, so it is exported and
// deliberately ergonomic. It is safe for concurrent use, which matters because
// the scheduler (AIX-0010) runs subtasks in parallel.
//
// Scripting model: each Run consumes the next scripted step in order. When the
// script is exhausted, the configured DefaultResult is returned (and, if set,
// DefaultErr) — so a mock with no script still behaves predictably.
type Mock struct {
	name string

	mu sync.Mutex
	// requests is the ordered log of every Request seen, for assertions.
	requests []Request
	// steps is the scripted sequence of responses, consumed front-to-back.
	steps []mockStep
	// next indexes the step to use for the upcoming Run.
	next int

	// DefaultResult is returned once the script is exhausted.
	DefaultResult Result
	// DefaultErr, if non-nil, is returned alongside DefaultResult once the
	// script is exhausted.
	DefaultErr error
}

// mockStep is one scripted response: the Result to return, an optional error,
// and an optional delay applied before returning (cancellable via context, so it
// can drive timeout/cancellation tests).
type mockStep struct {
	result Result
	err    error
	delay  time.Duration
}

// NewMock returns an empty mock harness with the given name and no script; Run
// will return the zero Result until steps or a DefaultResult are configured.
func NewMock(name string) *Mock {
	return &Mock{name: name}
}

// Name implements Harness.
func (m *Mock) Name() string { return m.name }

// PushResult appends a scripted step returning res (and no error). Steps are
// consumed in the order they were pushed. Returns the receiver for chaining.
func (m *Mock) PushResult(res Result) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, mockStep{result: res})
	return m
}

// PushText is a convenience wrapper around PushResult for the common case of a
// text-only result.
func (m *Mock) PushText(text string) *Mock {
	return m.PushResult(Result{Text: text, Raw: []byte(text)})
}

// PushError appends a scripted step that returns res together with err,
// simulating a harness failure.
func (m *Mock) PushError(res Result, err error) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, mockStep{result: res, err: err})
	return m
}

// PushDelay appends a scripted step that waits delay before returning res. The
// wait honors context cancellation, so tests can exercise timeout/cancel paths.
func (m *Mock) PushDelay(delay time.Duration, res Result) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, mockStep{result: res, delay: delay})
	return m
}

// PushResultFromFile appends a scripted step whose Result.Text and Result.Raw
// are read from a testdata file at call-build time. It returns an error rather
// than panicking so tests stay in control of failure reporting.
func (m *Mock) PushResultFromFile(path string) (*Mock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("mock %q: reading scripted result %q: %w", m.name, path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, mockStep{result: Result{Text: string(data), Raw: data}})
	return m, nil
}

// Run implements Harness: it records req, then returns the next scripted step
// (or the configured default once the script is exhausted), honoring any
// configured delay and context cancellation.
func (m *Mock) Run(ctx context.Context, req Request) (Result, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	var step mockStep
	scripted := m.next < len(m.steps)
	if scripted {
		step = m.steps[m.next]
		m.next++
	} else {
		step = mockStep{result: m.DefaultResult, err: m.DefaultErr}
	}
	m.mu.Unlock()

	if step.delay > 0 {
		select {
		case <-time.After(step.delay):
		case <-ctx.Done():
			return Result{}, fmt.Errorf("mock %q: canceled during simulated delay: %w", m.name, ctx.Err())
		}
	}
	return step.result, step.err
}

// Requests returns a copy of every Request the mock has received, in order, so
// tests can assert on what the pipeline sent without racing concurrent Runs.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// CallCount reports how many times Run has been invoked.
func (m *Mock) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}
