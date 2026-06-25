package run

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Artifact file and directory names. These are the on-disk layout from
// README → "Run artifacts" and the ticket. They are constants (not configurable)
// so the layout is stable across runs and resume can find things deterministically;
// the only configurable parts are the runs base dir (cfg.Paths.RunsDir) and the
// docs subdir name (cfg.Paths.DocsSubdir), both threaded in via Layout.
const (
	// RunFileName is the durable run-state file.
	RunFileName = "run.yaml"
	// runTempFileName is the temp file Save writes before renaming to RunFileName
	// (atomic write — see Store.Save).
	runTempFileName = "run.yaml.tmp"
	// TaskFileName holds the original task description, for easy reading.
	TaskFileName = "task.md"
	// ConfigSnapshotFileName holds the exact merged config used for the run.
	ConfigSnapshotFileName = "config.snapshot.yaml"

	// SubtasksDirName is the parent of per-subtask artifact dirs.
	SubtasksDirName = "subtasks"
	// SubtaskReviewsDirName is the per-subtask reviews subdir (round-N.md).
	SubtaskReviewsDirName = "reviews"
	// SubtaskDiffFileName is the per-subtask diff persisted under subtasks/<id>/.
	SubtaskDiffFileName = "diff.patch"
	// reviewRoundFilePattern names a single review round file under a reviews dir.
	// Rounds are 1-based (round-1.md is the first review); the per-subtask loop
	// (AIX-0011) and the senior-review loop (AIX-0012) both use this naming so
	// resume can find prior rounds deterministically.
	reviewRoundFilePattern = "round-%d.md"

	// SeniorReviewDirName holds senior-review round artifacts (round-N.md).
	SeniorReviewDirName = "senior-review"
	// LogsDirName holds structured run logs.
	LogsDirName = "logs"
	// BaselineDirName holds the run-start working-tree snapshot. It is dotted so
	// it reads as metadata and sorts before the human-facing dirs.
	BaselineDirName = ".baseline"

	// ControlDirName holds the run's control channel — files a second invocation
	// writes to signal the running process (AIX-0016). It is dotted so it reads as
	// metadata alongside .baseline.
	ControlDirName = ".control"
	// PauseRequestFileName is the marker whose presence requests a pause-to-review;
	// the running scheduler honors it at the next subtask boundary and clears it.
	PauseRequestFileName = "pause-requested"

	// LatestPointerName is the file under the runs dir naming the most recent
	// run. It is a plain text file (not a symlink) for cross-platform
	// portability — Windows symlink creation needs special privileges, so a file
	// containing the run id is the robust choice (see ResolveLatest).
	LatestPointerName = "latest"

	// LatestSentinel is the id callers pass (or the empty string) to mean "the
	// newest run"; Store.Load resolves it via the latest pointer.
	LatestSentinel = "latest"
)

// dirPerm / filePerm are the permissions used for created run dirs and files.
// 0o755 dirs / 0o644 files match the project's snapshot code and are friendly to
// the user inspecting artifacts.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// Layout computes the absolute paths of a single run's artifacts. It is a pure
// value (no I/O) derived from the runs base dir, the run id, and the docs subdir
// name; EnsureDirs performs the (idempotent) directory creation. Keeping path
// computation separate from creation makes resume safe: it can recompute every
// path and re-create only what's missing without disturbing existing artifacts.
type Layout struct {
	// RunsDir is the absolute base directory for all runs (cfg.Paths.RunsDir,
	// made absolute by the Store).
	RunsDir string
	// ID is the run identifier.
	ID string
	// DocsSubdir is the docs directory name under the run dir (cfg.Paths.DocsSubdir).
	DocsSubdir string
}

// Dir returns the run's root directory: <RunsDir>/<ID>.
func (l Layout) Dir() string { return filepath.Join(l.RunsDir, l.ID) }

// RunFile returns the path to run.yaml.
func (l Layout) RunFile() string { return filepath.Join(l.Dir(), RunFileName) }

// runTempFile returns the path to the atomic-write temp file.
func (l Layout) runTempFile() string { return filepath.Join(l.Dir(), runTempFileName) }

// TaskFile returns the path to task.md.
func (l Layout) TaskFile() string { return filepath.Join(l.Dir(), TaskFileName) }

// ConfigSnapshotFile returns the path to config.snapshot.yaml.
func (l Layout) ConfigSnapshotFile() string {
	return filepath.Join(l.Dir(), ConfigSnapshotFileName)
}

// DocsDir returns the docs directory (<run>/<DocsSubdir>), defaulting the subdir
// name to "docs" when unset so a zero-value DocsSubdir never yields the run root.
func (l Layout) DocsDir() string {
	sub := l.DocsSubdir
	if sub == "" {
		sub = "docs"
	}
	return filepath.Join(l.Dir(), sub)
}

// SubtasksDir returns the parent directory of per-subtask artifact dirs.
func (l Layout) SubtasksDir() string { return filepath.Join(l.Dir(), SubtasksDirName) }

// SubtaskDir returns the artifact directory for a single subtask. The id is
// sanitized so a hostile/odd subtask id cannot escape the run dir.
func (l Layout) SubtaskDir(subtaskID string) string {
	return filepath.Join(l.SubtasksDir(), safeSegment(subtaskID))
}

// SubtaskDiffFile returns the diff.patch path for a subtask.
func (l Layout) SubtaskDiffFile(subtaskID string) string {
	return filepath.Join(l.SubtaskDir(subtaskID), SubtaskDiffFileName)
}

// SubtaskReviewsDir returns the reviews subdir for a subtask.
func (l Layout) SubtaskReviewsDir(subtaskID string) string {
	return filepath.Join(l.SubtaskDir(subtaskID), SubtaskReviewsDirName)
}

// SubtaskReviewRoundFile returns the path of a single review round file
// (round-N.md, 1-based) under a subtask's reviews dir. The subtask review loop
// (AIX-0011) writes one per round so resume can re-enter at the right round.
func (l Layout) SubtaskReviewRoundFile(subtaskID string, round int) string {
	return filepath.Join(l.SubtaskReviewsDir(subtaskID), fmt.Sprintf(reviewRoundFilePattern, round))
}

// SeniorReviewDir returns the senior-review artifact directory.
func (l Layout) SeniorReviewDir() string { return filepath.Join(l.Dir(), SeniorReviewDirName) }

// LogsDir returns the logs directory.
func (l Layout) LogsDir() string { return filepath.Join(l.Dir(), LogsDirName) }

// BaselineDir returns the run-start baseline snapshot directory.
func (l Layout) BaselineDir() string { return filepath.Join(l.Dir(), BaselineDirName) }

// ControlDir returns the run's control-channel directory.
func (l Layout) ControlDir() string { return filepath.Join(l.Dir(), ControlDirName) }

// PauseRequestFile returns the pause-request marker path.
func (l Layout) PauseRequestFile() string {
	return filepath.Join(l.ControlDir(), PauseRequestFileName)
}

// EnsureDirs creates the run's directory tree, idempotently. It is safe to call
// on a fresh run (Create) and again on resume: os.MkdirAll does not error when a
// directory already exists, and EnsureDirs never removes or truncates anything.
// The per-subtask dirs are created lazily by the executor, not here, since the
// subtask set is unknown until planning; this creates only the run-level dirs.
func (l Layout) EnsureDirs() error {
	for _, d := range []string{
		l.Dir(),
		l.DocsDir(),
		l.SubtasksDir(),
		l.SeniorReviewDir(),
		l.LogsDir(),
		l.BaselineDir(),
	} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return fmt.Errorf("run layout: creating %q: %w", d, err)
		}
	}
	return nil
}

// LatestPointerPath returns the path of the latest pointer under runsDir.
func LatestPointerPath(runsDir string) string {
	return filepath.Join(runsDir, LatestPointerName)
}

// WriteLatest records id as the most recent run by writing it to the latest
// pointer file. It is written atomically (temp + rename) and idempotently: a
// repeated call with the same id is a harmless rewrite. A plain file (not a
// symlink) is used for Windows portability.
func WriteLatest(runsDir, id string) error {
	if err := os.MkdirAll(runsDir, dirPerm); err != nil {
		return fmt.Errorf("run layout: creating runs dir %q: %w", runsDir, err)
	}
	dst := LatestPointerPath(runsDir)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), filePerm); err != nil {
		return fmt.Errorf("run layout: writing latest pointer: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("run layout: finalizing latest pointer: %w", err)
	}
	return nil
}

// ReadLatest returns the run id recorded in the latest pointer. It returns
// ("", os.ErrNotExist) when no pointer exists yet (no runs / never written), so
// callers can distinguish "no latest" from a read error.
func ReadLatest(runsDir string) (string, error) {
	data, err := os.ReadFile(LatestPointerPath(runsDir))
	if err != nil {
		return "", err // includes os.ErrNotExist, which callers check for.
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("run layout: latest pointer in %q is empty", runsDir)
	}
	return id, nil
}

// safeSegment sanitizes a single path segment (a run or subtask id) so it cannot
// contain separators or traverse upward. It strips any directory components and
// rejects nothing — instead it reduces the value to its base name and replaces a
// "." or ".." result with "_", guaranteeing the segment stays inside its parent.
// Run IDs from NewID are already safe; this defends against ids supplied from
// elsewhere (e.g. a subtask id from planning).
func safeSegment(s string) string {
	s = filepath.Base(filepath.Clean("/" + s))
	if s == "." || s == ".." || s == string(filepath.Separator) || s == "" {
		return "_"
	}
	return s
}
