package prompt

import (
	"strings"
	"testing"
)

// sampleContexts returns a representative render context for every built-in role.
// It is the single source of "what a phase would pass", reused across tests so a
// field/template drift surfaces in one place.
func sampleContexts() map[string]any {
	return map[string]any{
		"planner": PlannerContext{
			Task:        "Add a write-through cache to the storage layer.",
			RepoSummary: "internal/storage/*.go; README describes the storage API.",
		},
		"executor": ExecutorContext{
			Task: "Add a write-through cache to the storage layer.",
			Subtask: SubtaskSpec{
				ID:          "st-01",
				Title:       "Implement the cache",
				Description: "Add an LRU write-through cache in internal/cache.",
				Files:       []string{"internal/cache/**"},
				Acceptance:  []string{"Cache evicts the least-recently-used entry.", "Concurrent access is safe."},
				ManualTest:  "Run the cache benchmark and watch the hit rate.",
			},
			ContextExcerpt: "Follow the existing constructor-returns-interface pattern.",
			Baseline:       BaselineInfo{Description: "the working tree as it was when this run started"},
		},
		"subtask-reviewer": SubtaskReviewerContext{
			Subtask: SubtaskSpec{
				ID:          "st-01",
				Title:       "Implement the cache",
				Description: "Add an LRU write-through cache in internal/cache.",
				Files:       []string{"internal/cache/**"},
				Acceptance:  []string{"Cache evicts the least-recently-used entry."},
			},
			Diff: "--- a/internal/cache/lru.go\n+++ b/internal/cache/lru.go\n@@\n+// new code\n",
		},
		"senior-reviewer": SeniorReviewerContext{
			Task:        "Add a write-through cache to the storage layer.",
			PlanSummary: "Add internal/cache and wire it into internal/storage.",
			FullDiff:    "--- a/internal/storage/store.go\n+++ b/internal/storage/store.go\n@@\n+// wired cache\n",
		},
	}
}

// TestBuiltinRolesRenderFromEmbedded covers the acceptance criterion that all
// four role templates render from embedded defaults with no override files
// present. NewRenderer() is constructed with no override dirs.
func TestBuiltinRolesRenderFromEmbedded(t *testing.T) {
	r := NewRenderer()
	ctxs := sampleContexts()

	for _, role := range BuiltinRoles {
		role := role
		t.Run(role, func(t *testing.T) {
			data, ok := ctxs[role]
			if !ok {
				t.Fatalf("no sample context defined for built-in role %q", role)
			}
			out, err := r.Render(role, data)
			if err != nil {
				t.Fatalf("Render(%q) from embedded default: %v", role, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatalf("Render(%q) produced empty output", role)
			}
			if strings.Contains(out, "<no value>") {
				t.Errorf("Render(%q) emitted \"<no value>\" — a context field is unset or misnamed:\n%s", role, out)
			}
		})
	}
}

// TestGitSafetyPreamblePresent covers the acceptance criterion that every worker
// prompt (planner, executor, both reviewers) carries the shared git-safety
// preamble. It asserts a distinctive sentence from _git-safety.tmpl appears in
// each rendered role prompt — proving the partial is shared across all template
// sets, embedded or otherwise.
func TestGitSafetyPreamblePresent(t *testing.T) {
	// A distinctive phrase that only the git-safety partial contains.
	const marker = "never** commits on the user's"
	// The heading the partial defines, as a second, structural check.
	const heading = "## Git safety"

	r := NewRenderer()
	ctxs := sampleContexts()

	for _, role := range BuiltinRoles {
		role := role
		t.Run(role, func(t *testing.T) {
			out, err := r.Render(role, ctxs[role])
			if err != nil {
				t.Fatalf("Render(%q): %v", role, err)
			}
			if !strings.Contains(out, marker) {
				t.Errorf("Render(%q) is missing the git-safety preamble (marker %q not found)", role, marker)
			}
			if !strings.Contains(out, heading) {
				t.Errorf("Render(%q) is missing the git-safety heading %q", role, heading)
			}
			// The embedded defaults reference the partial explicitly, so the
			// guarantee in Render must not append a duplicate copy.
			if n := strings.Count(out, heading); n != 1 {
				t.Errorf("Render(%q) emitted %d git-safety blocks, want exactly 1 (no duplication)", role, n)
			}
		})
	}
}

// TestGitSafetyForbidsMutatingGit guards the substance of invariant #1: the
// preamble must explicitly forbid the mutating git verbs, not merely mention git.
func TestGitSafetyForbidsMutatingGit(t *testing.T) {
	r := NewRenderer()
	out, err := r.Render("executor", sampleContexts()["executor"])
	if err != nil {
		t.Fatalf("Render(executor): %v", err)
	}
	for _, verb := range []string{"git commit", "git push", "git add", "git reset", "git rebase"} {
		if !strings.Contains(out, verb) {
			t.Errorf("git-safety preamble does not forbid %q", verb)
		}
	}
	if !strings.Contains(out, "unstaged") {
		t.Error("git-safety preamble should instruct leaving changes unstaged")
	}
}

// TestPlannerEncodesSubtasksContract guards the prompt↔parser coupling for
// AIX-0009: the planner prompt must spell out the bundle markers and the
// subtasks.yaml schema markers the planning-phase parser depends on. If these
// drift, the planner stops producing what the parser reads.
func TestPlannerEncodesSubtasksContract(t *testing.T) {
	r := NewRenderer()
	out, err := r.Render("planner", sampleContexts()["planner"])
	if err != nil {
		t.Fatalf("Render(planner): %v", err)
	}

	// The bundle marker prefix the parser splits on must be present.
	if !strings.Contains(out, "@@AIXECUTOR_DOC:") {
		t.Errorf("planner prompt does not document the @@AIXECUTOR_DOC: bundle marker")
	}
	// Each document's marker line (exact form) must appear, so the agent emits all
	// four under the names the parser expects, subtasks.yaml last.
	for _, marker := range []string{
		"@@AIXECUTOR_DOC:plan.md@@",
		"@@AIXECUTOR_DOC:context.md@@",
		"@@AIXECUTOR_DOC:manual-testing.md@@",
		"@@AIXECUTOR_DOC:subtasks.yaml@@",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("planner prompt is missing bundle marker %q", marker)
		}
	}
	// Schema markers the AIX-0009 parser unmarshals.
	for _, marker := range []string{"subtasks:", "id:", "title:", "description:", "deps:", "files:", "acceptance:", "manualTest:"} {
		if !strings.Contains(out, marker) {
			t.Errorf("planner prompt is missing subtasks.yaml schema marker %q", marker)
		}
	}
	// The DAG requirement (no cycles) must be stated.
	if !strings.Contains(strings.ToLower(out), "cycle") {
		t.Error("planner prompt does not state the no-cycles (DAG) requirement")
	}
	// The planner must be told it is plan-only and must not write files (the
	// READ-ONLY bundle contract): it should mention not implementing.
	if !strings.Contains(strings.ToLower(out), "do not implement") {
		t.Error("planner prompt does not state the plan-only (do not implement) boundary")
	}
}

// TestPlannerPriorErrorFeedback proves the single-re-prompt path: when
// PlannerContext.PriorError is set, the rendered prompt surfaces it so the agent
// can correct its previous output. When empty, the corrective section is absent.
func TestPlannerPriorErrorFeedback(t *testing.T) {
	r := NewRenderer()

	const priorErr = "subtasks.yaml: dependency cycle: st-01 -> st-02 -> st-01"
	out, err := r.Render("planner", PlannerContext{
		Task:        "Add a flag.",
		RepoSummary: "main.go",
		PriorError:  priorErr,
	})
	if err != nil {
		t.Fatalf("Render(planner) with PriorError: %v", err)
	}
	if !strings.Contains(out, priorErr) {
		t.Errorf("planner prompt did not surface the PriorError feedback; got:\n%s", out)
	}

	// Without a PriorError the corrective heading must not appear.
	clean, err := r.Render("planner", PlannerContext{Task: "Add a flag.", RepoSummary: "main.go"})
	if err != nil {
		t.Fatalf("Render(planner) clean: %v", err)
	}
	if strings.Contains(clean, "previous attempt was rejected") {
		t.Errorf("planner prompt showed the corrective section with no PriorError set; got:\n%s", clean)
	}
}

// TestReviewerEncodesFindingsContract guards the prompt↔parser coupling for
// AIX-0011/0012: both reviewer prompts must spell out the findings verdict block
// the findings parser reads (approved + severity vocabulary).
func TestReviewerEncodesFindingsContract(t *testing.T) {
	r := NewRenderer()
	ctxs := sampleContexts()

	for _, role := range []string{"subtask-reviewer", "senior-reviewer"} {
		role := role
		t.Run(role, func(t *testing.T) {
			out, err := r.Render(role, ctxs[role])
			if err != nil {
				t.Fatalf("Render(%q): %v", role, err)
			}
			for _, marker := range []string{"approved:", "findings:", "severity:", "message:"} {
				if !strings.Contains(out, marker) {
					t.Errorf("%s prompt is missing findings-block marker %q", role, marker)
				}
			}
			// The full severity vocabulary the parser accepts.
			for _, sev := range []string{"blocker", "major", "minor", "nit"} {
				if !strings.Contains(out, sev) {
					t.Errorf("%s prompt does not list severity %q", role, sev)
				}
			}
			// It must demand a single, final yaml block.
			if !strings.Contains(out, "```yaml") {
				t.Errorf("%s prompt does not require a fenced ```yaml verdict block", role)
			}
		})
	}
}
