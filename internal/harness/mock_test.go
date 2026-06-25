package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMockRecordsRequestsAndScriptsResults covers the core mock contract:
// requests are recorded in order and scripted results are returned in order,
// falling back to the default once exhausted.
func TestMockRecordsRequestsAndScriptsResults(t *testing.T) {
	m := NewMock("planner")
	m.PushText("first").PushText("second")
	m.DefaultResult = Result{Text: "default"}

	if m.Name() != "planner" {
		t.Errorf("Name = %q, want planner", m.Name())
	}

	r1, err := m.Run(context.Background(), Request{Prompt: "p1", Model: "opus"})
	if err != nil || r1.Text != "first" {
		t.Fatalf("run 1 = (%q, %v), want first", r1.Text, err)
	}
	r2, _ := m.Run(context.Background(), Request{Prompt: "p2"})
	if r2.Text != "second" {
		t.Errorf("run 2 = %q, want second", r2.Text)
	}
	r3, _ := m.Run(context.Background(), Request{Prompt: "p3"})
	if r3.Text != "default" {
		t.Errorf("run 3 = %q, want default (script exhausted)", r3.Text)
	}

	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(reqs))
	}
	if reqs[0].Prompt != "p1" || reqs[0].Model != "opus" {
		t.Errorf("request[0] = %+v, want prompt p1 model opus", reqs[0])
	}
	if reqs[2].Prompt != "p3" {
		t.Errorf("request[2].Prompt = %q, want p3", reqs[2].Prompt)
	}
	if m.CallCount() != 3 {
		t.Errorf("CallCount = %d, want 3", m.CallCount())
	}
}

// TestMockSimulatedError checks PushError returns the scripted error.
func TestMockSimulatedError(t *testing.T) {
	boom := errors.New("simulated harness failure")
	m := NewMock("executor")
	m.PushError(Result{ExitCode: 1}, boom)

	res, err := m.Run(context.Background(), Request{Prompt: "x"})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

// TestMockDefaultError checks DefaultErr applies once the script is exhausted.
func TestMockDefaultError(t *testing.T) {
	boom := errors.New("default failure")
	m := NewMock("executor")
	m.DefaultErr = boom
	if _, err := m.Run(context.Background(), Request{}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// TestMockSimulatedDelayCancel checks a scripted delay honors context
// cancellation, enabling timeout tests against the mock.
func TestMockSimulatedDelayCancel(t *testing.T) {
	m := NewMock("slow")
	m.PushDelay(5*time.Second, Result{Text: "late"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := m.Run(ctx, Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("delay did not honor cancellation, took %v", time.Since(start))
	}
}

// TestMockDelayCompletes checks a delay shorter than the deadline returns the
// scripted result.
func TestMockDelayCompletes(t *testing.T) {
	m := NewMock("ok")
	m.PushDelay(10*time.Millisecond, Result{Text: "done"})
	res, err := m.Run(context.Background(), Request{})
	if err != nil || res.Text != "done" {
		t.Fatalf("got (%q, %v), want done", res.Text, err)
	}
}

// TestMockResultFromFile checks file-backed scripted results load testdata.
func TestMockResultFromFile(t *testing.T) {
	m := NewMock("planner")
	if _, err := m.PushResultFromFile(filepath.Join("..", "..", "testdata", "harness", "result.json")); err != nil {
		t.Fatalf("PushResultFromFile: %v", err)
	}
	res, err := m.Run(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Text, "Implemented the feature") {
		t.Errorf("file-backed Text = %q, want it to contain the fixture", res.Text)
	}
	if len(res.Raw) == 0 {
		t.Error("Raw should hold the file bytes")
	}
}

// TestMockResultFromFileMissing checks a missing file returns an error.
func TestMockResultFromFileMissing(t *testing.T) {
	m := NewMock("planner")
	if _, err := m.PushResultFromFile("does-not-exist.json"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestMockConcurrentSafe runs the mock from many goroutines to catch data races
// (the scheduler runs subtasks in parallel). Run under -race to be meaningful.
func TestMockConcurrentSafe(t *testing.T) {
	m := NewMock("parallel")
	m.DefaultResult = Result{Text: "ok"}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = m.Run(context.Background(), Request{Prompt: "concurrent"})
		}()
	}
	wg.Wait()
	if m.CallCount() != n {
		t.Errorf("CallCount = %d, want %d", m.CallCount(), n)
	}
}
