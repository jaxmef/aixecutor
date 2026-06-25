package run

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"gopkg.in/yaml.v3"
)

// Baseliner captures the run-start working-tree snapshot into dstDir and returns
// a Baseline describing it. It is the seam that keeps internal/run hermetic: the
// production implementation (GitBaseliner, baseline_git.go) adapts
// *git.Gateway.CaptureBaseline, but tests inject a fake that writes a fake
// .baseline dir and returns a Baseline value, so run tests never touch a real
// git repo or run any git command. The real git path is already covered by
// AIX-0006's tests.
//
// Implementations MUST be read-only with respect to git (CLAUDE.md §2 invariant
// 1); they only read the tree and copy files.
type Baseliner interface {
	// CaptureBaseline snapshots the working tree into dstDir and returns the
	// resulting Baseline. dstDir is created if absent.
	CaptureBaseline(dstDir string) (Baseline, error)
}

// Store reads and writes run artifacts under a single runs base directory. It is
// the only component that touches the on-disk run layout; the CLI and (later) the
// orchestrator go through it. A Store is cheap to construct and safe to reuse.
//
// Construction injects two seams so the store is testable hermetically:
//   - clk (Clock) makes run IDs and timestamps deterministic;
//   - baseliner (Baseliner) decouples Create from real git.
type Store struct {
	// runsDir is the absolute base directory for runs (cfg.Paths.RunsDir made
	// absolute). All run dirs and the latest pointer live directly under it.
	runsDir string
	// docsSubdir is the docs directory name within each run dir
	// (cfg.Paths.DocsSubdir), threaded into the Layout.
	docsSubdir string
	// clk is the time source for IDs and timestamps.
	clk Clock
	// baseliner captures the run-start baseline. May be nil only if Create is
	// never called (e.g. a read-only Store used solely for List/Load/status);
	// Create returns a clear error if it is nil.
	baseliner Baseliner
	// rename finalizes the atomic write (tmp → run.yaml). It defaults to
	// os.Rename and is a seam tests use to inject a rename failure AFTER the temp
	// file is written, proving the prior run.yaml survives a mid-write crash.
	rename func(oldpath, newpath string) error
}

// Option configures a Store at construction. Options keep the constructor small
// while letting callers (and tests) override the clock and baseliner.
type Option func(*Store)

// WithClock sets the Store's time source. Defaults to SystemClock.
func WithClock(clk Clock) Option {
	return func(s *Store) {
		if clk != nil {
			s.clk = clk
		}
	}
}

// WithBaseliner sets the Store's baseline capturer. Required for Create; tests
// pass a fake, the CLI passes a git-backed GitBaseliner.
func WithBaseliner(b Baseliner) Option {
	return func(s *Store) { s.baseliner = b }
}

// WithDocsSubdir overrides the docs subdir name. Normally taken from config via
// NewStoreFromConfig; this option exists for direct/test construction.
func WithDocsSubdir(name string) Option {
	return func(s *Store) {
		if name != "" {
			s.docsSubdir = name
		}
	}
}

// withRename overrides the rename step of the atomic write. It is unexported and
// used only by tests to simulate a finalize (rename) failure after the temp file
// is written, exercising the crash-safety guarantee.
func withRename(fn func(oldpath, newpath string) error) Option {
	return func(s *Store) {
		if fn != nil {
			s.rename = fn
		}
	}
}

// NewStore constructs a Store rooted at runsDir (made absolute). Apply options to
// set the clock, baseliner, and docs subdir. With no options it uses SystemClock,
// a "docs" subdir, and no baseliner (so Create will error until one is supplied).
func NewStore(runsDir string, opts ...Option) (*Store, error) {
	abs, err := filepath.Abs(runsDir)
	if err != nil {
		return nil, fmt.Errorf("run store: resolving runs dir %q: %w", runsDir, err)
	}
	s := &Store{
		runsDir:    abs,
		docsSubdir: "docs",
		clk:        SystemClock{},
		rename:     os.Rename,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// NewStoreFromConfig constructs a Store using the resolved configuration for the
// runs dir and docs subdir, then applies any extra options (typically the
// baseliner). runsDir from config is resolved relative to repoRoot when it is a
// relative path (the default ".aixecutor/runs" is repo-relative), so run
// artifacts land under the repository regardless of the process working dir.
func NewStoreFromConfig(cfg config.Config, repoRoot string, opts ...Option) (*Store, error) {
	base := []Option{WithDocsSubdir(cfg.Paths.DocsSubdir)}
	return NewStore(resolveRunsDir(cfg, repoRoot), append(base, opts...)...)
}

// resolveRunsDir computes the runs base directory from config: the configured
// paths.runsDir (defaulting to ".aixecutor/runs"), joined onto repoRoot when it is
// relative so artifacts land under the repository regardless of the process
// working dir. NewStore makes the result absolute; RepoRelRunsDir reuses the same
// resolution so the baseline/diff exclusion matches exactly where runs are written.
func resolveRunsDir(cfg config.Config, repoRoot string) string {
	runsDir := cfg.Paths.RunsDir
	if runsDir == "" {
		runsDir = ".aixecutor/runs"
	}
	if !filepath.IsAbs(runsDir) && repoRoot != "" {
		runsDir = filepath.Join(repoRoot, runsDir)
	}
	return runsDir
}

// RepoRelRunsDir returns the resolved runs directory expressed RELATIVE to
// repoRoot, suitable as a git exclusion prefix so the run-start baseline and the
// senior-review full diff never snapshot the tool's own output dir (see
// git.Gateway.SetExcludePrefixes). It mirrors the resolution NewStoreFromConfig
// uses, so the exclusion always matches the active runsDir.
//
// It returns "" (no exclusion) when runsDir lies OUTSIDE the repository — a user
// who points runsDir at an absolute path elsewhere already keeps the tool's output
// out of the tree, so there is nothing under repoRoot to exclude. The git layer
// also drops a "../…" prefix defensively, but returning "" here keeps the intent
// explicit. repoRoot must be absolute (the gateway's RepoRoot always is).
func RepoRelRunsDir(cfg config.Config, repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	abs, err := filepath.Abs(resolveRunsDir(cfg, repoRoot))
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return ""
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// runsDir is the repo root itself (degenerate) or outside it: nothing under
		// the repo to exclude.
		return ""
	}
	return rel
}

// RunsDir returns the absolute runs base directory.
func (s *Store) RunsDir() string { return s.runsDir }

// DocsDir returns the docs directory for a run id, resolved through this store's
// Layout so it uses the store's configured docs subdir. The planning phase
// (AIX-0009) writes the planner docs here, and it matches exactly where Create
// and the inspection commands look. The run need not exist on disk for this pure
// path computation.
func (s *Store) DocsDir(id string) string {
	return s.layoutFor(id).DocsDir()
}

// layoutFor builds the Layout for a run id under this store.
func (s *Store) layoutFor(id string) Layout {
	return Layout{RunsDir: s.runsDir, ID: id, DocsSubdir: s.docsSubdir}
}

// Create starts a new run for task: it allocates a run id from the clock, builds
// the full directory tree, writes task.md and config.snapshot.yaml, captures the
// run-start baseline via the injected Baseliner, and saves the initial run.yaml
// (Status=created). It then records this run as the `latest` — and is the ONLY
// place that writes the latest pointer, so `latest` always names the
// most-recently-created run and is not perturbed by later Saves (e.g. resuming an
// older run). Directory creation is idempotent.
//
// cfg is snapshotted so the run is reproducible and so resume uses the exact
// config the run started with, independent of later edits to the user's config
// files. The senior-review phase's enabled flag is seeded from cfg.
func (s *Store) Create(task string, cfg config.Config) (*Run, error) {
	if s.baseliner == nil {
		return nil, errors.New("run store: Create requires a Baseliner (construct the Store WithBaseliner)")
	}

	id := NewID(task, s.clk)
	layout := s.layoutFor(id)
	if err := layout.EnsureDirs(); err != nil {
		return nil, err
	}

	// task.md — the original task, verbatim, for easy reading.
	if err := os.WriteFile(layout.TaskFile(), []byte(taskMarkdown(task)), filePerm); err != nil {
		return nil, fmt.Errorf("run store: writing %s: %w", TaskFileName, err)
	}

	// config.snapshot.yaml — the exact merged config used for this run. Reuses
	// the config package's YAML marshalling (Config's yaml tags +
	// Duration.MarshalYAML), the same encoding `config show` builds on, so the
	// snapshot round-trips and reads identically to the live config.
	snap, err := marshalConfigSnapshot(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(layout.ConfigSnapshotFile(), snap, filePerm); err != nil {
		return nil, fmt.Errorf("run store: writing %s: %w", ConfigSnapshotFileName, err)
	}

	// Baseline — capture the run-start working tree so diffs are relative to the
	// user's starting point (CLAUDE.md §4.4). Goes through the injected seam.
	baseline, err := s.baseliner.CaptureBaseline(layout.BaselineDir())
	if err != nil {
		return nil, fmt.Errorf("run store: capturing baseline: %w", err)
	}

	now := s.clk.Now()
	r := &Run{
		SchemaVersion: CurrentSchemaVersion,
		ID:            id,
		Task:          task,
		Status:        StatusCreated,
		CreatedAt:     now,
		UpdatedAt:     now,
		Baseline:      baseline,
		Subtasks:      nil,
		SeniorReview: SeniorReview{
			Enabled: cfg.Pipeline.SeniorReview.Enabled,
			Status:  SeniorReviewPending,
			Rounds:  0,
		},
		Dir: layout.Dir(),
	}

	if err := s.Save(r); err != nil {
		return nil, err
	}

	// Record this brand-new run as the most recent. This is the sole writer of the
	// latest pointer (Save no longer touches it), so `latest` deterministically
	// means the newest-created run. A failure here does not invalidate the durable
	// run.yaml just written, so it is surfaced but the run already exists on disk.
	if err := WriteLatest(s.runsDir, id); err != nil {
		return nil, fmt.Errorf("run store: recording latest pointer for %q: %w", id, err)
	}
	return r, nil
}

// Save writes r to <Dir>/run.yaml atomically: it marshals to run.yaml.tmp in the
// same directory, then os.Rename()s it over run.yaml. os.Rename is atomic within
// a filesystem, so a crash mid-write can leave a stale .tmp file but NEVER a
// truncated or partially-written run.yaml — the previous good run.yaml stays
// intact until the rename completes. This is what makes "Save after every state
// transition" safe (CLAUDE.md §2 invariant 6): an interrupted Save loses at most
// the in-flight transition, and resume reads the last fully-written checkpoint.
//
// Save refreshes r.UpdatedAt from the clock and re-asserts the run Dir and schema
// version so a Run constructed elsewhere is persisted consistently.
//
// Save does NOT touch the `latest` pointer. The pointer is written once, at
// Create, and deterministically means "the most-recently-CREATED run" (see
// Create / ReadLatest). This is deliberate: the orchestrator Saves run.yaml after
// every transition, including while RESUMING an older run, so repointing `latest`
// on each Save would let `resume <older-id>` hijack `latest` and make a later bare
// `status`/`resume` resolve the wrong run. Keeping `latest` create-only keeps it
// consistent with List (newest-first by creation time).
func (s *Store) Save(r *Run) error {
	if r == nil {
		return errors.New("run store: Save(nil)")
	}
	if r.ID == "" {
		return errors.New("run store: Save: run has no ID")
	}
	layout := s.layoutFor(r.ID)

	r.UpdatedAt = s.clk.Now()
	if r.SchemaVersion == 0 {
		r.SchemaVersion = CurrentSchemaVersion
	}
	r.Dir = layout.Dir()

	// The run dir must exist; creating it here keeps Save usable on its own and
	// is idempotent (no-op when Create already made the tree).
	if err := os.MkdirAll(layout.Dir(), dirPerm); err != nil {
		return fmt.Errorf("run store: ensuring run dir for %q: %w", r.ID, err)
	}

	data, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("run store: marshalling run.yaml for %q: %w", r.ID, err)
	}

	renameFn := s.rename
	if renameFn == nil {
		renameFn = os.Rename
	}

	tmp := layout.runTempFile()
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("run store: writing temp run.yaml for %q: %w", r.ID, err)
	}
	if err := renameFn(tmp, layout.RunFile()); err != nil {
		// Leave no stray temp file behind on a failed finalize; the prior
		// run.yaml (if any) is untouched because the rename did not complete.
		_ = os.Remove(tmp)
		return fmt.Errorf("run store: finalizing run.yaml for %q: %w", r.ID, err)
	}

	// NOTE: the `latest` pointer is intentionally NOT updated here — it is
	// create-only (see Create and the Save godoc) so resuming an older run cannot
	// hijack which run `latest` names.
	return nil
}

// Load reads and unmarshals <Dir>/run.yaml for the run identified by id and
// returns the reconstructed Run. The id "latest" or "" resolves to the most
// recent run via the latest pointer (see ReadLatest / LatestSentinel).
//
// # Resume contract (AIX-0013 relies on this)
//
// The Run that Load returns is the authoritative record of progress; its
// statuses tell the orchestrator EXACTLY what is done and what to redo. The rules
// the orchestrator MUST follow:
//
//   - A subtask whose Status is SubtaskDone is NEVER re-run. Its persisted
//     artifacts under subtasks/<id>/ (diff.patch, reviews/round-N.md) let resume
//     rebuild context for downstream subtasks without re-invoking the finished
//     executor or reviewer.
//   - A subtask interrupted mid-step — Status SubtaskImplementing or
//     SubtaskReviewing (SubtaskStatus.IsInterrupted) — is rewound to
//     SubtaskPending and re-run FROM EXECUTION: the scheduler re-invokes the
//     executor (regenerating subtasks/<id>/diff.patch) and then re-reviews that
//     fresh diff. This is the actual, safe behavior — an interrupted reviewer is
//     NOT resumed as a review-only step, because the on-disk diff may not match
//     what the (possibly partially-applied) executor left in the tree, so the diff
//     is regenerated rather than trusted. Loops is PRESERVED across the
//     interruption, so seniorReview/subtaskReview.maxLoops still bounds the total
//     cycles and a resume cannot reset the budget. (A true re-review-only
//     optimization is intentionally out of scope.)
//   - A SubtaskPending subtask becomes ready only when all its Deps are
//     SubtaskDone (the normal scheduling rule).
//   - SubtaskBlocked / SubtaskFailed are surfaced to the user; resume does not
//     silently retry them.
//   - The run-level Status names the phase to resume into:
//     created/planning → (re)run planning (a single idempotent step that
//     rewrites the docs); planned/executing → schedule remaining subtasks;
//     seniorReview → resume the senior-review loop, whose own progress is in
//     SeniorReview.Status/Rounds (SeniorReviewRunning restarts the current
//     round). A terminal Status (Status.IsTerminal) means there is nothing to
//     resume.
//
// Because the baseline is persisted (Baseline.Dir) and never recaptured, diffs
// on resume remain relative to the original run-start tree, exactly as in the
// first run.
func (s *Store) Load(id string) (*Run, error) {
	resolved, err := s.resolveID(id)
	if err != nil {
		return nil, err
	}
	layout := s.layoutFor(resolved)

	data, err := os.ReadFile(layout.RunFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("run store: no run %q (no %s under %s)", resolved, RunFileName, layout.Dir())
		}
		return nil, fmt.Errorf("run store: reading run.yaml for %q: %w", resolved, err)
	}

	var r Run
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("run store: parsing run.yaml for %q: %w", resolved, err)
	}
	if r.SchemaVersion > CurrentSchemaVersion {
		return nil, fmt.Errorf(
			"run store: run %q was written by a newer aixecutor (schemaVersion %d > supported %d); upgrade aixecutor to resume it",
			resolved, r.SchemaVersion, CurrentSchemaVersion)
	}
	if r.Status != "" && !r.Status.IsValid() {
		return nil, fmt.Errorf("run store: run %q has unknown status %q in run.yaml", resolved, r.Status)
	}

	// Recompute Dir from this store's runsDir so a relocated runs tree still
	// resolves; the persisted Dir is advisory.
	r.Dir = layout.Dir()
	// Ensure the id matches the directory we loaded from (defends against a
	// hand-edited run.yaml with a mismatched id).
	r.ID = resolved
	return &r, nil
}

// List enumerates the run directories under runsDir and returns a summary for
// each, newest-first (by CreatedAt, then id as a tiebreaker). Directories without
// a readable run.yaml are skipped (they are not yet runs, or are corrupt); the
// latest pointer file is ignored. An absent runs dir yields an empty slice, not
// an error, so `list` on a fresh checkout simply shows nothing.
func (s *Store) List() ([]RunSummary, error) {
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("run store: reading runs dir %q: %w", s.runsDir, err)
	}

	var out []RunSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue // skip the latest pointer and any stray files.
		}
		id := e.Name()
		r, err := s.Load(id)
		if err != nil {
			continue // not a valid run dir; skip rather than fail the whole list.
		}
		out = append(out, RunSummary{
			ID:        r.ID,
			Task:      r.Task,
			Status:    r.Status,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Dir:       r.Dir,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		// Stable, newest-first: ids embed a sortable timestamp, so a reverse id
		// sort breaks ties deterministically (useful when the clock is fixed in
		// tests and every run shares a CreatedAt).
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// resolveID maps the user-facing id onto a concrete run id. "" and "latest" both
// resolve via the latest pointer; any other value is returned as-is (its
// existence is checked by the caller when it reads run.yaml). A missing latest
// pointer yields a clear "no runs" error.
func (s *Store) resolveID(id string) (string, error) {
	if id != "" && id != LatestSentinel {
		return id, nil
	}
	latest, err := ReadLatest(s.runsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("run store: no runs found (no latest run recorded)")
		}
		return "", fmt.Errorf("run store: resolving latest run: %w", err)
	}
	return latest, nil
}

// marshalConfigSnapshot serializes cfg to YAML for config.snapshot.yaml. It uses
// the config package's own marshalling contract (the Config struct's yaml tags
// and Duration.MarshalYAML) via yaml.Marshal, so the snapshot is the same shape
// `config show` renders (minus the provenance annotations, which are not part of
// the persisted config). A leading comment marks the file as a generated, exact
// snapshot.
func marshalConfigSnapshot(cfg config.Config) ([]byte, error) {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("run store: marshalling config snapshot: %w", err)
	}
	header := "# aixecutor config snapshot — the exact merged config used for this run.\n" +
		"# Generated at run start; edits here do not affect the run.\n"
	return append([]byte(header), body...), nil
}

// taskMarkdown renders the task description as task.md. It is a minimal markdown
// document (a heading plus the task body) so the file reads well in an editor
// while preserving the task verbatim.
func taskMarkdown(task string) string {
	return "# Task\n\n" + task + "\n"
}
