package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaxmef/aixecutor/internal/config"
)

// fakeBaseliner is a hermetic Baseliner: it writes a small fake .baseline dir and
// returns a Baseline value, so run tests never touch a real git repo or run git.
// It records the dstDir it was asked to populate.
type fakeBaseliner struct {
	calls   int
	lastDst string
	files   int
	bytes   int64
	err     error
}

func (f *fakeBaseliner) CaptureBaseline(dstDir string) (Baseline, error) {
	f.calls++
	f.lastDst = dstDir
	if f.err != nil {
		return Baseline{}, f.err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return Baseline{}, err
	}
	// Drop a marker file so the baseline dir is observably non-empty, mimicking a
	// real snapshot without any git involvement.
	if err := os.WriteFile(filepath.Join(dstDir, "MARKER"), []byte("fake baseline\n"), 0o644); err != nil {
		return Baseline{}, err
	}
	return Baseline{Dir: dstDir, Files: f.files, Bytes: f.bytes}, nil
}

// newTestStore builds a Store rooted in a temp dir with a fixed clock and a fake
// baseliner, the standard hermetic setup for these tests.
func newTestStore(t *testing.T, opts ...Option) (*Store, *fakeBaseliner) {
	t.Helper()
	fb := &fakeBaseliner{files: 3, bytes: 42}
	base := []Option{
		WithClock(fixedClock{t: fixedAt}),
		WithBaseliner(fb),
	}
	s, err := NewStore(t.TempDir(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, fb
}

func TestCreateBuildsTreeAndArtifacts(t *testing.T) {
	s, fb := newTestStore(t)
	cfg := config.Default()

	r, err := s.Create("Add OAuth2 login", cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Identity + initial state.
	if r.ID != "20260623T120501-add-oauth2-login" {
		t.Errorf("run ID = %q", r.ID)
	}
	if r.Status != StatusCreated {
		t.Errorf("Status = %q, want %q", r.Status, StatusCreated)
	}
	if r.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, CurrentSchemaVersion)
	}
	if !r.CreatedAt.Equal(fixedAt) || !r.UpdatedAt.Equal(fixedAt) {
		t.Errorf("timestamps not from injected clock: created=%v updated=%v", r.CreatedAt, r.UpdatedAt)
	}
	// Senior review seeded from config (default enabled=true).
	if !r.SeniorReview.Enabled || r.SeniorReview.Status != SeniorReviewPending {
		t.Errorf("SeniorReview = %+v, want enabled+pending", r.SeniorReview)
	}

	// Baseliner was invoked exactly once, into the run's .baseline dir.
	if fb.calls != 1 {
		t.Errorf("baseliner calls = %d, want 1", fb.calls)
	}
	l := s.layoutFor(r.ID)
	if fb.lastDst != l.BaselineDir() {
		t.Errorf("baseliner dst = %q, want %q", fb.lastDst, l.BaselineDir())
	}
	if r.Baseline.Dir != l.BaselineDir() || r.Baseline.Files != 3 || r.Baseline.Bytes != 42 {
		t.Errorf("Baseline = %+v, want dir=%q files=3 bytes=42", r.Baseline, l.BaselineDir())
	}

	// Directory tree exists.
	for _, d := range []string{l.Dir(), l.DocsDir(), l.SubtasksDir(), l.SeniorReviewDir(), l.LogsDir(), l.BaselineDir()} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("expected dir %q: err=%v", d, err)
		}
	}

	// task.md holds the task verbatim.
	taskBytes, err := os.ReadFile(l.TaskFile())
	if err != nil {
		t.Fatalf("read task.md: %v", err)
	}
	if !strings.Contains(string(taskBytes), "Add OAuth2 login") {
		t.Errorf("task.md missing task text:\n%s", taskBytes)
	}

	// config.snapshot.yaml is the real merged config (reuses config marshalling).
	snapBytes, err := os.ReadFile(l.ConfigSnapshotFile())
	if err != nil {
		t.Fatalf("read config snapshot: %v", err)
	}
	snap := string(snapBytes)
	for _, want := range []string{"version: 1", "policy: read-only", "command: claude", "timeout: 30m0s"} {
		if !strings.Contains(snap, want) {
			t.Errorf("config snapshot missing %q:\n%s", want, snap)
		}
	}

	// run.yaml exists and the latest pointer points at this run.
	if _, err := os.Stat(l.RunFile()); err != nil {
		t.Errorf("run.yaml not written: %v", err)
	}
	if latest, err := ReadLatest(s.RunsDir()); err != nil || latest != r.ID {
		t.Errorf("latest = %q (err %v), want %q", latest, err, r.ID)
	}
}

func TestCreateRequiresBaseliner(t *testing.T) {
	s, err := NewStore(t.TempDir(), WithClock(fixedClock{t: fixedAt}))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.Create("x", config.Default()); err == nil {
		t.Fatal("Create without a Baseliner should error")
	} else if !strings.Contains(err.Error(), "Baseliner") {
		t.Errorf("error %q should mention Baseliner", err)
	}
}

func TestCreateBaselinerErrorPropagates(t *testing.T) {
	fb := &fakeBaseliner{err: errors.New("boom")}
	s, err := NewStore(t.TempDir(), WithClock(fixedClock{t: fixedAt}), WithBaseliner(fb))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.Create("x", config.Default()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Create error = %v, want it to wrap baseliner failure", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)

	r, err := s.Create("Build the thing", config.Default())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate into a rich, mid-pipeline state with subtasks, statuses, loops.
	r.Status = StatusExecuting
	r.Subtasks = []Subtask{
		{
			ID:          "st-1",
			Title:       "Schema",
			Description: "define structs",
			Acceptance:  "compiles",
			Deps:        nil,
			Files:       []string{"internal/run/*.go"},
			Status:      SubtaskDone,
			Loops:       2,
		},
		{
			ID:     "st-2",
			Title:  "Store",
			Deps:   []string{"st-1"},
			Files:  []string{"internal/run/store.go"},
			Status: SubtaskImplementing,
			Loops:  1,
			// Carry-forward findings persist (AIX-0011 proceed-flagged path).
			Unresolved: []Finding{
				{Severity: "major", File: "internal/run/store.go", Line: 7, Message: "missing error wrap"},
			},
		},
	}
	r.SeniorReview = SeniorReview{Enabled: true, Status: SeniorReviewRunning, Rounds: 1}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(r.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Status != StatusExecuting || got.Task != "Build the thing" {
		t.Errorf("loaded run mismatch: status=%q task=%q", got.Status, got.Task)
	}
	if len(got.Subtasks) != 2 {
		t.Fatalf("loaded %d subtasks, want 2", len(got.Subtasks))
	}
	st1 := got.Subtasks[0]
	if st1.ID != "st-1" || st1.Status != SubtaskDone || st1.Loops != 2 || st1.Acceptance != "compiles" {
		t.Errorf("subtask 1 round-trip mismatch: %+v", st1)
	}
	st2 := got.Subtasks[1]
	if st2.Status != SubtaskImplementing || len(st2.Deps) != 1 || st2.Deps[0] != "st-1" {
		t.Errorf("subtask 2 round-trip mismatch: %+v", st2)
	}
	if len(st2.Unresolved) != 1 || st2.Unresolved[0].Severity != "major" ||
		st2.Unresolved[0].File != "internal/run/store.go" || st2.Unresolved[0].Line != 7 ||
		st2.Unresolved[0].Message != "missing error wrap" {
		t.Errorf("subtask 2 unresolved findings round-trip mismatch: %+v", st2.Unresolved)
	}
	// A subtask with no carried findings must NOT serialize an unresolved block
	// (omitempty), so a clean run.yaml stays uncluttered.
	if len(st1.Unresolved) != 0 {
		t.Errorf("subtask 1 should have no unresolved findings; got %+v", st1.Unresolved)
	}
	if got.SeniorReview.Status != SeniorReviewRunning || got.SeniorReview.Rounds != 1 {
		t.Errorf("senior review round-trip mismatch: %+v", got.SeniorReview)
	}

	// run.yaml must be human-readable: statuses appear as string enums, not ints.
	raw, err := os.ReadFile(s.layoutFor(r.ID).RunFile())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"schemaVersion: 1",
		"status: executing",
		"status: done",
		"status: implementing",
		"status: running", // senior review
		"loops: 2",
		"unresolved:",        // carried findings block
		"severity: major",    // carried finding fields, human-readable
		"missing error wrap", // the finding message
	} {
		if !strings.Contains(text, want) {
			t.Errorf("run.yaml missing human-readable %q:\n%s", want, text)
		}
	}
}

// TestSaveAtomicCrashSafe proves that a failure during the rename step of Save
// leaves the PRIOR run.yaml fully intact (not truncated/corrupted) and removes
// the temp file — the crash-safety guarantee behind "save after every
// transition".
func TestSaveAtomicCrashSafe(t *testing.T) {
	s, _ := newTestStore(t)

	r, err := s.Create("crash test", config.Default())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Snapshot the good run.yaml written by Create.
	runFile := s.layoutFor(r.ID).RunFile()
	good, err := os.ReadFile(runFile)
	if err != nil {
		t.Fatal(err)
	}

	// Build a second store over the SAME runs dir whose rename always fails,
	// simulating a crash after the temp file is written but before finalize.
	failing, err := NewStore(s.RunsDir(),
		WithClock(fixedClock{t: fixedAt.Add(time.Hour)}),
		WithBaseliner(&fakeBaseliner{}),
		withRename(func(_, _ string) error { return errors.New("simulated crash before finalize") }),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	r.Status = StatusCompleted // a change we will fail to persist
	if err := failing.Save(r); err == nil {
		t.Fatal("expected Save to fail when rename fails")
	}

	// The prior run.yaml is byte-for-byte intact.
	after, err := os.ReadFile(runFile)
	if err != nil {
		t.Fatalf("prior run.yaml unreadable after failed save: %v", err)
	}
	if string(after) != string(good) {
		t.Errorf("prior run.yaml changed after failed save:\nbefore:\n%s\nafter:\n%s", good, after)
	}

	// It still loads and still shows the pre-crash status (created), not the
	// status we failed to persist.
	loaded, err := s.Load(r.ID)
	if err != nil {
		t.Fatalf("Load after failed save: %v", err)
	}
	if loaded.Status != StatusCreated {
		t.Errorf("status after failed save = %q, want %q (change must not have persisted)", loaded.Status, StatusCreated)
	}

	// No stray temp file is left behind.
	if _, err := os.Stat(s.layoutFor(r.ID).runTempFile()); !os.IsNotExist(err) {
		t.Errorf("temp run.yaml should be cleaned up after a failed rename: err=%v", err)
	}
}

func TestSaveUpdatesTimestampAndIsIdempotent(t *testing.T) {
	// A clock that advances by a minute on each call, so UpdatedAt moves.
	tick := fixedAt
	advancing := ClockFunc(func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	})
	s, err := NewStore(t.TempDir(), WithClock(advancing), WithBaseliner(&fakeBaseliner{}))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	r, err := s.Create("x", config.Default())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	firstUpdated := r.UpdatedAt

	if err := s.Save(r); err != nil { // saving again must work (idempotent dirs)
		t.Fatalf("second Save: %v", err)
	}
	if !r.UpdatedAt.After(firstUpdated) {
		t.Errorf("UpdatedAt did not advance on re-save: %v !> %v", r.UpdatedAt, firstUpdated)
	}
}

func TestLoadUnknownRun(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Load("does-not-exist"); err == nil {
		t.Fatal("Load of missing run should error")
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	s, _ := newTestStore(t)
	r, err := s.Create("x", config.Default())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Hand-write a run.yaml claiming a newer schema version.
	runFile := s.layoutFor(r.ID).RunFile()
	if err := os.WriteFile(runFile, []byte("schemaVersion: 999\nid: "+r.ID+"\nstatus: created\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r.ID); err == nil || !strings.Contains(err.Error(), "newer aixecutor") {
		t.Errorf("Load error = %v, want a 'newer aixecutor' schema error", err)
	}
}

func TestLoadRejectsUnknownStatus(t *testing.T) {
	s, _ := newTestStore(t)
	r, err := s.Create("x", config.Default())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	runFile := s.layoutFor(r.ID).RunFile()
	if err := os.WriteFile(runFile, []byte("schemaVersion: 1\nid: "+r.ID+"\nstatus: bananas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r.ID); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Errorf("Load error = %v, want an 'unknown status' error", err)
	}
}

func TestListNewestFirst(t *testing.T) {
	// Distinct, increasing timestamps so each run gets a unique, ordered id.
	base := fixedAt
	n := 0
	clk := ClockFunc(func() time.Time {
		// Create calls Now() twice (id + timestamps) then Save once; keep the id
		// stable within a Create by only advancing per call group is overkill —
		// instead advance every call; ids still order correctly because the id is
		// taken from the first Now() of each Create and we space Creates apart.
		t := base.Add(time.Duration(n) * time.Hour)
		n++
		return t
	})
	s, err := NewStore(t.TempDir(), WithClock(clk), WithBaseliner(&fakeBaseliner{}))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var ids []string
	for i := 0; i < 3; i++ {
		// Space each Create well apart by burning the clock so the per-Create id
		// timestamps differ by hours regardless of intra-Create Now() calls.
		base = base.Add(24 * time.Hour)
		n = 0
		r, err := s.Create("task "+string(rune('a'+i)), config.Default())
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}

	summaries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("List returned %d runs, want 3", len(summaries))
	}
	// Newest first: the last-created id should be first.
	if summaries[0].ID != ids[2] {
		t.Errorf("List[0] = %q, want newest %q (order: %v)", summaries[0].ID, ids[2], ids)
	}
	if summaries[2].ID != ids[0] {
		t.Errorf("List[2] = %q, want oldest %q", summaries[2].ID, ids[0])
	}
	// Summary carries identity + status.
	if summaries[0].Status != StatusCreated || summaries[0].Task == "" {
		t.Errorf("summary missing fields: %+v", summaries[0])
	}
}

func TestListEmptyAndMissingDir(t *testing.T) {
	// A runs dir that does not exist yields an empty list, no error.
	s, err := NewStore(filepath.Join(t.TempDir(), "never-created"), WithBaseliner(&fakeBaseliner{}))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List on missing dir = %v, want empty", got)
	}
}

func TestListSkipsNonRunDirs(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Create("real", config.Default()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A stray directory without run.yaml must be skipped, not error the list.
	if err := os.MkdirAll(filepath.Join(s.RunsDir(), "junk-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("List = %d runs, want 1 (junk dir skipped)", len(got))
	}
}

func TestLoadLatestResolves(t *testing.T) {
	s, _ := newTestStore(t)

	// No runs yet: latest resolution is a clear error.
	if _, err := s.Load("latest"); err == nil {
		t.Error("Load(latest) with no runs should error")
	}
	if _, err := s.Load(""); err == nil {
		t.Error(`Load("") with no runs should error`)
	}

	// Create two runs at different times; latest must resolve to the newest.
	first, err := s.Create("first", config.Default())
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	// Advance the clock for the second run by using a fresh store over the same
	// dir with a later clock.
	s2, err := NewStore(s.RunsDir(),
		WithClock(fixedClock{t: fixedAt.Add(time.Hour)}),
		WithBaseliner(&fakeBaseliner{}),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	second, err := s2.Create("second", config.Default())
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected distinct ids for the two runs")
	}

	got, err := s.Load("latest")
	if err != nil {
		t.Fatalf("Load(latest): %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("Load(latest) = %q, want newest %q", got.ID, second.ID)
	}
}

// TestLatestIsCreateOnlyAndStableAcrossSaves is the AIX-0013 carry-forward fix:
// `latest` deterministically names the most-recently-CREATED run and is NOT
// repointed by subsequent Saves. So Saving (e.g. resuming/advancing) an OLDER run
// must not hijack `latest`, which would make a later bare `status`/`resume` resolve
// the wrong run.
func TestLatestIsCreateOnlyAndStableAcrossSaves(t *testing.T) {
	dir := t.TempDir()

	// Create an older run, then a newer one (distinct ids via distinct clocks).
	sOld, err := NewStore(dir, WithClock(fixedClock{t: fixedAt}), WithBaseliner(&fakeBaseliner{}))
	if err != nil {
		t.Fatalf("NewStore(old): %v", err)
	}
	older, err := sOld.Create("older", config.Default())
	if err != nil {
		t.Fatalf("Create older: %v", err)
	}

	sNew, err := NewStore(dir, WithClock(fixedClock{t: fixedAt.Add(time.Hour)}), WithBaseliner(&fakeBaseliner{}))
	if err != nil {
		t.Fatalf("NewStore(new): %v", err)
	}
	newer, err := sNew.Create("newer", config.Default())
	if err != nil {
		t.Fatalf("Create newer: %v", err)
	}

	// After both Creates, latest is the newest-created run.
	if got, err := ReadLatest(dir); err != nil || got != newer.ID {
		t.Fatalf("after creates, latest = %q (err %v); want newest %q", got, err, newer.ID)
	}

	// Now Save the OLDER run several times (simulating a resume that advances it
	// through phases). Pre-fix, each Save would repoint latest to the older run.
	older.Status = StatusExecuting
	if err := sOld.Save(older); err != nil {
		t.Fatalf("save older 1: %v", err)
	}
	older.Status = StatusSeniorReview
	if err := sOld.Save(older); err != nil {
		t.Fatalf("save older 2: %v", err)
	}

	// latest must STILL be the newest-created run — Save never repointed it.
	if got, err := ReadLatest(dir); err != nil || got != newer.ID {
		t.Errorf("after saving the older run, latest = %q (err %v); want it unchanged at %q",
			got, err, newer.ID)
	}
	// And Load("latest") resolves the newer run, not the just-saved older one.
	if got, err := sNew.Load("latest"); err != nil || got.ID != newer.ID {
		t.Errorf("Load(latest) = %q (err %v); want %q (stable across older-run saves)",
			got.ID, err, newer.ID)
	}

	// List stays consistent with that meaning: newest-created first.
	summaries, err := sNew.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 2 || summaries[0].ID != newer.ID || summaries[1].ID != older.ID {
		t.Errorf("List order = %v; want [newer, older] = [%q, %q]",
			summarize(summaries), newer.ID, older.ID)
	}
}

// summarize extracts ids from a slice of RunSummary for readable failure messages.
func summarize(ss []RunSummary) []string {
	out := make([]string, len(ss))
	for i := range ss {
		out[i] = ss[i].ID
	}
	return out
}

func TestNewStoreFromConfigResolvesRunsDir(t *testing.T) {
	repo := t.TempDir()
	cfg := config.Default() // RunsDir = ".aixecutor/runs" (relative)
	s, err := NewStoreFromConfig(cfg, repo, WithBaseliner(&fakeBaseliner{}), WithClock(fixedClock{t: fixedAt}))
	if err != nil {
		t.Fatalf("NewStoreFromConfig: %v", err)
	}
	want := filepath.Join(repo, ".aixecutor", "runs")
	if s.RunsDir() != want {
		t.Errorf("RunsDir = %q, want %q", s.RunsDir(), want)
	}
	if s.docsSubdir != "docs" {
		t.Errorf("docsSubdir = %q, want docs", s.docsSubdir)
	}
}

func TestRunHelpers(t *testing.T) {
	r := &Run{Subtasks: []Subtask{
		{ID: "a", Status: SubtaskDone},
		{ID: "b", Status: SubtaskImplementing},
		{ID: "c", Status: SubtaskDone},
	}}
	if got, ok := r.SubtaskByID("b"); !ok || got.Status != SubtaskImplementing {
		t.Errorf("SubtaskByID(b) = %+v, ok=%v", got, ok)
	}
	if _, ok := r.SubtaskByID("missing"); ok {
		t.Error("SubtaskByID(missing) should report not found")
	}
	done, total := r.CountSubtasks()
	if done != 2 || total != 3 {
		t.Errorf("CountSubtasks = (%d,%d), want (2,3)", done, total)
	}
}
