package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jaxmef/aixecutor/internal/config"
	"github.com/jaxmef/aixecutor/internal/harness"
	"github.com/jaxmef/aixecutor/internal/log"
	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
)

// docMarkerPrefix and docMarkerSuffix bracket a bundle document marker line. A
// marker is `@@AIXECUTOR_DOC:<filename>@@` ALONE on its own line. This is the
// delimiter the planner template emits and this parser splits on; the two MUST
// stay in lockstep (internal/prompt/prompts/planner.tmpl). The marker is
// deliberately distinctive so it cannot collide with Markdown fences or YAML in
// the documents themselves (CLAUDE.md §3.3 / AIX-0009 "contract option 2").
const (
	docMarkerPrefix = "@@AIXECUTOR_DOC:"
	docMarkerSuffix = "@@"
)

// Bundle document filenames. These are the four documents the planner returns and
// aixecutor writes under <run>/docs/. subtasksDocName is parsed + DAG-validated;
// the other three are human Markdown written verbatim.
const (
	planDocName          = "plan.md"
	contextDocName       = "context.md"
	manualTestingDocName = "manual-testing.md"
	subtasksDocName      = "subtasks.yaml"
)

// rawResponseFileName is where a failed planning attempt's raw agent response is
// kept under <run>/docs/ for inspection, so a missing/invalid bundle is debuggable
// rather than lost (AIX-0009 acceptance: "keep whatever the agent produced").
const rawResponseFileName = "planner-raw.txt"

// requiredDocs is the set of bundle documents that must be present, in the order
// the planner emits them (subtasks.yaml last). Used to validate completeness and
// to write the docs deterministically.
var requiredDocs = []string{planDocName, contextDocName, manualTestingDocName, subtasksDocName}

// Repo-summary budget. The summary handed to the planner prompt is bounded so it
// orients the agent without dominating the prompt (CLAUDE.md notes the budget must
// be documented): at most summaryMaxFiles tracked paths from `git ls-files`
// (sorted, then elided with a "… and N more" line) and the first
// summaryReadmeLines lines of the repo README, truncated to summaryReadmeBytes.
const (
	summaryMaxFiles    = 200
	summaryReadmeLines = 60
	summaryReadmeBytes = 4096
)

// RepoSummarizer produces the bounded repository orientation blob handed to the
// planner prompt. It is an interface so the planner can be tested hermetically
// with a canned summary, while production uses GitRepoSummarizer over the
// read-only git gateway. Implementations MUST be read-only with respect to git
// (CLAUDE.md invariant #1).
type RepoSummarizer interface {
	// Summary returns a compact, bounded orientation blob for repoRoot.
	Summary(ctx context.Context) (string, error)
}

// fileLister is the read-only slice of the git gateway the summarizer needs:
// listing tracked files. *git.Gateway.TrackedFiles satisfies it. Declaring the
// narrow interface keeps the pipeline decoupled from the full gateway and lets
// tests inject a fake without a real repo.
type fileLister interface {
	TrackedFiles(ctx context.Context) ([]string, error)
}

// GitRepoSummarizer builds the repo summary from the read-only git gateway plus
// the repository README. It lists tracked files (bounded) and prepends a short
// README excerpt when one exists.
type GitRepoSummarizer struct {
	lister   fileLister
	repoRoot string
}

// NewGitRepoSummarizer constructs a summarizer over the given tracked-file lister
// (the git gateway) rooted at repoRoot (used to locate the README).
func NewGitRepoSummarizer(lister fileLister, repoRoot string) *GitRepoSummarizer {
	return &GitRepoSummarizer{lister: lister, repoRoot: repoRoot}
}

// Summary returns the bounded orientation blob: a README excerpt (if present)
// followed by a truncated, sorted file tree. It is read-only. A failure to list
// files is returned (the planner needs at least the tree); a missing/unreadable
// README is tolerated (the excerpt is simply omitted).
func (s *GitRepoSummarizer) Summary(ctx context.Context) (string, error) {
	files, err := s.lister.TrackedFiles(ctx)
	if err != nil {
		return "", fmt.Errorf("gathering repo summary: listing tracked files: %w", err)
	}

	var b strings.Builder
	if excerpt := s.readmeExcerpt(files); excerpt != "" {
		b.WriteString("README (excerpt):\n")
		b.WriteString(excerpt)
		b.WriteString("\n\n")
	}
	b.WriteString("Tracked files:\n")
	b.WriteString(fileTree(files))
	return b.String(), nil
}

// readmeExcerpt returns the first lines of the repository README (case-insensitive
// match on a top-level README* file), bounded by the summary budget. It returns ""
// when no README is tracked or it cannot be read — the excerpt is optional.
func (s *GitRepoSummarizer) readmeExcerpt(files []string) string {
	name := findReadme(files)
	if name == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.repoRoot, filepath.FromSlash(name)))
	if err != nil {
		return ""
	}
	return excerptLines(string(data), summaryReadmeLines, summaryReadmeBytes)
}

// findReadme returns the path of a top-level README file (README, README.md,
// readme.txt, …), case-insensitively, or "" if none is tracked. Only top-level
// files (no slash) are considered so a nested doc's README is not mistaken for the
// project one.
func findReadme(files []string) string {
	for _, f := range files {
		if strings.Contains(f, "/") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(f), "readme") {
			return f
		}
	}
	return ""
}

// excerptLines returns at most maxLines lines of s, truncated to maxBytes, with a
// trailing "…" marker when content was dropped (so the planner knows the excerpt
// is partial).
func excerptLines(s string, maxLines, maxBytes int) string {
	lines := strings.Split(s, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		truncated = true
	}
	if truncated {
		out = strings.TrimRight(out, "\n") + "\n…"
	}
	return out
}

// fileTree renders a bounded, sorted list of repo-relative paths, one per line. At
// most summaryMaxFiles are shown; the remainder is summarized as "… and N more".
// Sorting keeps the summary stable across runs (and across the nondeterministic
// order git can return).
func fileTree(files []string) string {
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	if len(sorted) == 0 {
		return "(no tracked files)"
	}
	shown := sorted
	extra := 0
	if len(sorted) > summaryMaxFiles {
		shown = sorted[:summaryMaxFiles]
		extra = len(sorted) - summaryMaxFiles
	}
	var b strings.Builder
	for _, f := range shown {
		b.WriteString("  ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	if extra > 0 {
		fmt.Fprintf(&b, "  … and %d more\n", extra)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Planner runs the planning phase (CLAUDE.md §3.3 step 1): it renders the planner
// prompt with the task and a bounded repo summary, invokes the planner harness in
// PLAN MODE (read-only on the repo), parses the returned document bundle, writes
// the four docs under <run>/docs/, parses + DAG-validates subtasks.yaml, persists
// the subtasks into the run as pending, and prints the docs path. It does NOT
// proceed to execution (that is AIX-0010/0013).
//
// All collaborators are injected so the phase is hermetically testable: the
// planner harness (a mock in tests), the prompt renderer, the run store, the
// repo summarizer, the planner role config, and the output sink. Production wiring
// lives in internal/cli.
type Planner struct {
	// harness drives the planner agent (resolved from the registry by role).
	harness harness.Harness
	// renderer renders the planner prompt template (with overrides honored).
	renderer *prompt.Renderer
	// store persists the run after subtasks are parsed (Save).
	store *run.Store
	// summarizer builds the bounded repo summary for the prompt.
	summarizer RepoSummarizer
	// role is the planner role config (model, permissionMode, promptTemplate,
	// timeout). Everything is config-driven (invariant #4).
	role config.Role
	// repoRoot is the agent's working directory (the repository root). The planner
	// is read-only there.
	repoRoot string
	// dryRun marks that the harness is the dry-run wrapper, so its placeholder
	// result is not a real bundle: validation is skipped and placeholder docs are
	// written with a clear "[dry-run]" notice instead of failing.
	dryRun bool
	// progress renders the human-facing planning notices (phase banner, docs path).
	// Defaults to a stdout-backed Progress when nil.
	progress *log.Progress
}

// PlannerOption configures a Planner at construction.
type PlannerOption func(*Planner)

// WithDryRun marks the planner as running against the dry-run harness wrapper, so
// the placeholder result is handled gracefully (placeholder docs, no strict bundle
// validation) instead of erroring.
func WithDryRun(dryRun bool) PlannerOption {
	return func(p *Planner) { p.dryRun = dryRun }
}

// WithOutput sets where the planner prints its human-facing docs-path notice, by
// building a Progress over w. Defaults to os.Stdout. Kept for callers that have
// only a writer (e.g. the `plan` command and tests); WithProgress is preferred
// when a shared Progress already exists.
func WithOutput(w io.Writer) PlannerOption {
	return func(p *Planner) {
		if w != nil {
			p.progress = log.NewProgress(w)
		}
	}
}

// WithProgress sets the shared Progress the planner emits semantic events through.
// Defaults to a stdout-backed Progress.
func WithProgress(pr *log.Progress) PlannerOption {
	return func(p *Planner) {
		if pr != nil {
			p.progress = pr
		}
	}
}

// NewPlanner constructs a Planner from its collaborators. harness, renderer,
// store, and summarizer are required; role supplies the model/permissionMode/
// timeout/template; repoRoot is the agent's working dir.
func NewPlanner(h harness.Harness, renderer *prompt.Renderer, store *run.Store, summarizer RepoSummarizer, role config.Role, repoRoot string, opts ...PlannerOption) *Planner {
	p := &Planner{
		harness:    h,
		renderer:   renderer,
		store:      store,
		summarizer: summarizer,
		role:       role,
		repoRoot:   repoRoot,
		progress:   log.NewProgress(nil),
	}
	for _, o := range opts {
		o(p)
	}
	if p.progress == nil {
		p.progress = log.NewProgress(nil)
	}
	return p
}

// Plan runs the planning phase for r. It transitions the run to planning, invokes
// the planner (with one optional re-prompt on validation failure), writes the
// docs, persists the parsed subtasks as pending, and transitions the run to
// planned. The run is Saved on each transition so an interrupted planning step is
// resumable (planning is idempotent — resume re-runs it).
//
// On a validation failure that survives the single retry, Plan keeps the raw agent
// response under <run>/docs/planner-raw.txt and returns a clear, actionable error;
// the run is left in the planning state with whatever was produced, so the user
// can inspect it.
func (p *Planner) Plan(ctx context.Context, r *run.Run) error {
	if r == nil {
		return errors.New("pipeline: Plan(nil run)")
	}

	p.progress.PhaseStarted("Planning")
	r.Status = run.StatusPlanning
	if err := p.store.Save(r); err != nil {
		return fmt.Errorf("pipeline: marking run %q planning: %w", r.ID, err)
	}

	summary, err := p.summarizer.Summary(ctx)
	if err != nil {
		return err
	}

	// Dry-run short-circuit: the dry-run harness returns a placeholder, not a real
	// bundle, so we never invoke a real agent and we write placeholder docs instead
	// of failing strict validation (AIX-0009 dry-run behavior).
	if p.dryRun {
		return p.planDryRun(ctx, r, summary)
	}

	// First attempt, then a single re-prompt feeding the validation error back.
	var lastErr error
	var lastRaw string
	for attempt := 1; attempt <= 2; attempt++ {
		priorErr := ""
		if attempt == 2 {
			priorErr = lastErr.Error()
		}

		raw, perr := p.invoke(ctx, r.Task, summary, priorErr)
		if perr != nil {
			// A harness/transport failure is not a validation problem; do not retry
			// it as if the agent produced a bad bundle — surface it directly.
			return fmt.Errorf("pipeline: planner invocation failed for run %q: %w", r.ID, perr)
		}
		lastRaw = raw

		subtasks, verr := p.processResponse(r, raw)
		if verr == nil {
			// Success: persist subtasks as pending and mark the run planned.
			r.Subtasks = subtasks
			r.Status = run.StatusPlanned
			if err := p.store.Save(r); err != nil {
				return fmt.Errorf("pipeline: saving planned run %q: %w", r.ID, err)
			}
			p.announce(r)
			return nil
		}
		lastErr = verr
	}

	// Both attempts failed validation. Keep the raw response for inspection and
	// return an actionable error. The run stays in `planning`.
	rawPath := p.writeRaw(r, lastRaw)
	return fmt.Errorf("pipeline: planner output for run %q did not validate after a retry: %w (raw response kept at %s)",
		r.ID, lastErr, rawPath)
}

// invoke renders the planner prompt (with an optional prior-error for the retry)
// and runs the planner harness, returning the agent's final text. The agent runs
// with WorkDir at the repo root and the role's model/permissionMode/timeout.
func (p *Planner) invoke(ctx context.Context, task, summary, priorErr string) (string, error) {
	promptText, err := p.renderer.Render(p.role.PromptTemplate, prompt.PlannerContext{
		Task:        task,
		RepoSummary: summary,
		PriorError:  priorErr,
	})
	if err != nil {
		return "", fmt.Errorf("rendering planner prompt: %w", err)
	}

	res, err := p.harness.Run(ctx, harness.Request{
		Prompt:         promptText,
		Role:           "planner",
		Model:          p.role.Model,
		WorkDir:        p.repoRoot,
		PermissionMode: p.role.PermissionMode,
		Timeout:        p.role.Timeout.Std(),
	})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// processResponse parses the bundle, writes the four docs, and parses +
// DAG-validates subtasks.yaml. On success it returns the run.Subtasks (pending).
// On any failure it returns a clear error; the docs that WERE parsed are written
// regardless so a partially-correct response is inspectable, but a missing or
// invalid subtasks.yaml still fails the attempt.
func (p *Planner) processResponse(r *run.Run, raw string) ([]run.Subtask, error) {
	docs, err := parseBundle(raw)
	if err != nil {
		return nil, err
	}
	if err := requireAllDocs(docs); err != nil {
		return nil, err
	}

	// Write the docs we have (all four are present at this point).
	if err := p.writeDocs(r, docs); err != nil {
		return nil, err
	}

	subtasks, err := ParseSubtasks([]byte(docs[subtasksDocName]))
	if err != nil {
		return nil, err
	}
	return subtasks, nil
}

// writeDocs writes each bundle document to <run>/docs/<name>. The docs dir is
// created if absent (idempotent). Errors are wrapped with the destination path.
func (p *Planner) writeDocs(r *run.Run, docs map[string]string) error {
	docsDir := p.docsDir(r)
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return fmt.Errorf("creating docs dir %q: %w", docsDir, err)
	}
	for _, name := range requiredDocs {
		dst := filepath.Join(docsDir, name)
		if err := os.WriteFile(dst, []byte(docs[name]), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
	}
	return nil
}

// planDryRun handles the --dry-run path: it writes clearly-marked placeholder docs
// (including a minimal valid subtasks.yaml so downstream tooling has something to
// read), persists a single placeholder pending subtask, marks the run planned, and
// prints a notice that planning was skipped. It never invokes a real agent and
// never fails strict validation.
func (p *Planner) planDryRun(ctx context.Context, r *run.Run, summary string) error {
	// Still exercise prompt rendering + the harness wrapper so the dry-run path is
	// representative, but ignore the placeholder result for content.
	if _, err := p.invoke(ctx, r.Task, summary, ""); err != nil {
		return fmt.Errorf("pipeline: dry-run planner invocation failed for run %q: %w", r.ID, err)
	}

	docs := dryRunDocs(r.Task)
	if err := p.writeDocs(r, docs); err != nil {
		return err
	}
	subtasks, err := ParseSubtasks([]byte(docs[subtasksDocName]))
	if err != nil {
		// dryRunDocs is a constant we control, so this is a programming error.
		return fmt.Errorf("pipeline: dry-run placeholder subtasks invalid: %w", err)
	}

	r.Subtasks = subtasks
	r.Status = run.StatusPlanned
	if err := p.store.Save(r); err != nil {
		return fmt.Errorf("pipeline: saving dry-run planned run %q: %w", r.ID, err)
	}

	p.progress.Logf("[dry-run] planning skipped the real agent; wrote placeholder docs to %s", p.docsDir(r))
	p.progress.PlanningDone(p.docsDir(r), len(r.Subtasks))
	return nil
}

// writeRaw persists the raw agent response under <run>/docs/planner-raw.txt for
// inspection and returns the path. A write failure is swallowed (best-effort) but
// the returned path still points where it would have been; the caller's error
// already describes the validation failure.
func (p *Planner) writeRaw(r *run.Run, raw string) string {
	docsDir := p.docsDir(r)
	_ = os.MkdirAll(docsDir, 0o755)
	dst := filepath.Join(docsDir, rawResponseFileName)
	_ = os.WriteFile(dst, []byte(raw), 0o644)
	return dst
}

// announce prints the human-facing notice that planning is done and where the
// docs are, prominently (CLAUDE.md §3.3: "Print the docs path"). It is a semantic
// progress event so AIX-0015's TUI can render it its own way.
func (p *Planner) announce(r *run.Run) {
	p.progress.PlanningDone(p.docsDir(r), len(r.Subtasks))
}

// docsDir returns the run's docs directory via the store, so the planner writes
// exactly where Create / status / resume look (the store owns the docs subdir
// from config).
func (p *Planner) docsDir(r *run.Run) string {
	return p.store.DocsDir(r.ID)
}

// parseBundle splits an agent response into its named documents using the
// @@AIXECUTOR_DOC:<name>@@ marker lines. A document's content is the text from
// after its marker line up to the next marker line (or end of input). Each body
// is normalized to end in exactly one newline (trailing blank lines, whether they
// precede the next marker or the end of input, are collapsed) so a document reads
// the same regardless of its position in the bundle and is a well-formed text file
// on disk. Text before the first marker is ignored.
//
// It returns a map from filename to content. Unknown document names are ignored
// (forward-compatible), and completeness is checked separately by requireAllDocs
// so the error can name exactly which required docs are missing.
func parseBundle(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("planner returned an empty response (expected the @@AIXECUTOR_DOC bundle)")
	}

	docs := make(map[string]string)
	lines := strings.Split(raw, "\n")

	var curName string
	var curBody []string
	flush := func() {
		if curName == "" {
			return
		}
		docs[curName] = normalizeDoc(curBody)
	}

	sawMarker := false
	for _, line := range lines {
		if name, ok := parseMarkerLine(line); ok {
			sawMarker = true
			flush()
			curName = name
			curBody = curBody[:0]
			continue
		}
		if curName != "" {
			curBody = append(curBody, line)
		}
	}
	flush()

	if !sawMarker {
		return nil, fmt.Errorf("planner response contained no %s<name>%s markers (cannot locate the four documents)",
			docMarkerPrefix, docMarkerSuffix)
	}
	return docs, nil
}

// parseMarkerLine reports whether line is a bundle marker and, if so, the document
// name it introduces. A marker is exactly `@@AIXECUTOR_DOC:<name>@@` with only
// surrounding whitespace allowed, so a marker-looking string embedded mid-line in
// a document does not falsely split it.
func parseMarkerLine(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, docMarkerPrefix) || !strings.HasSuffix(t, docMarkerSuffix) {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(t, docMarkerPrefix), docMarkerSuffix)
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return "", false
	}
	return inner, true
}

// normalizeDoc joins a document's body lines and normalizes its trailing
// whitespace: trailing blank lines are dropped and exactly one trailing newline is
// appended for a non-empty body. This makes a document independent of where it sat
// in the bundle (a middle doc loses the blank line before the next marker; a final
// doc loses the trailing EOF newline — both end up identical) and yields a
// well-formed text file. An all-blank body normalizes to "".
func normalizeDoc(body []string) string {
	joined := strings.Join(body, "\n")
	trimmed := strings.TrimRight(joined, "\n")
	if trimmed == "" {
		return ""
	}
	return trimmed + "\n"
}

// requireAllDocs verifies every required document is present and non-empty in the
// parsed bundle, returning a clear error naming the missing/empty ones so the
// planner (on retry) knows exactly what to add.
func requireAllDocs(docs map[string]string) error {
	var missing []string
	for _, name := range requiredDocs {
		if strings.TrimSpace(docs[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("planner bundle is missing or empty for: %s (expected all of: %s)",
			strings.Join(missing, ", "), strings.Join(requiredDocs, ", "))
	}
	return nil
}

// dryRunDocs returns clearly-marked placeholder documents for the --dry-run path,
// including a minimal valid subtasks.yaml (one pending subtask) so the run model
// and any downstream tooling have something well-formed to read. The content
// states plainly that no real planning happened.
func dryRunDocs(task string) map[string]string {
	notice := "# [dry-run] No real planning was performed\n\n" +
		"This document is a placeholder produced by `aixecutor --dry-run`. No AI agent\n" +
		"was invoked. Task:\n\n    " + task + "\n"
	return map[string]string{
		planDocName:          notice,
		contextDocName:       notice,
		manualTestingDocName: notice,
		subtasksDocName: "" +
			"subtasks:\n" +
			"  - id: st-01\n" +
			"    title: \"[dry-run] placeholder subtask\"\n" +
			"    description: \"Placeholder subtask produced by --dry-run; no real planning occurred.\"\n" +
			"    deps: []\n" +
			"    files: []\n" +
			"    acceptance:\n" +
			"      - \"n/a (dry-run placeholder)\"\n",
	}
}
