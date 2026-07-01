package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
	claudeharness "github.com/jaxmef/aixecutor/internal/harness/claude"
	piharness "github.com/jaxmef/aixecutor/internal/harness/pi"
)

// TestPlanTaskInputArgCount covers the MaximumNArgs(1) + resolveTaskInput wiring
// (AIX-0017): zero args is a "no task" usage error and two args is rejected by
// cobra — both before any work (no git, no run) happens.
func TestPlanTaskInputArgCount(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		out, err := runCLI(t, missingConfigArgs(t, "plan")...)
		if err == nil {
			t.Fatalf("plan with no task should error; output:\n%s", out)
		}
		if !strings.Contains(err.Error(), "no task provided") {
			t.Errorf("error should explain the missing task; got: %v", err)
		}
	})

	t.Run("too many args", func(t *testing.T) {
		out, err := runCLI(t, missingConfigArgs(t, "plan", "task one", "task two")...)
		if err == nil {
			t.Fatalf("plan with two tasks should error; output:\n%s", out)
		}
		if !strings.Contains(err.Error(), "accepts at most 1 arg") {
			t.Errorf("error should explain the arg count; got: %v", err)
		}
	})
}

// TestPlanPrintsResumeHint proves the standalone `plan` command ends a successful
// run by telling the user how to execute the plan, citing the same run id it used
// elsewhere in its output. Driven hermetically under --dry-run.
func TestPlanPrintsResumeHint(t *testing.T) {
	t.Chdir(t.TempDir())

	out, err := runCLI(t, missingConfigArgs(t, "--dry-run", "plan", "add a flag")...)
	if err != nil {
		t.Fatalf("dry-run plan should succeed: %v\n%s", err, out)
	}

	const prefix = "Resume execution with: aixecutor resume "
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("output missing resume hint %q:\n%s", prefix, out)
	}
	rest := out[idx+len(prefix):]
	id := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	if id == "" {
		t.Fatalf("resume hint carried no run id:\n%s", out)
	}
	if strings.Count(out, id) < 2 {
		t.Errorf("resume hint id %q should also appear elsewhere in the output:\n%s", id, out)
	}
}

// TestPresetFactoriesWiresClaudeAndPi proves the registry is wired with both the
// claude and pi presets (the AIX-0004/0005 loose end this ticket closes): building
// the registry from the default config yields preset-backed harnesses for both
// names, and the registry exposes them.
func TestPresetFactoriesWiresClaudeAndPi(t *testing.T) {
	factories := presetFactories()
	for _, name := range []string{claudeharness.Name, piharness.Name} {
		if _, ok := factories[name]; !ok {
			t.Errorf("presetFactories() missing %q", name)
		}
	}

	reg, err := newRegistry(config.Default(), false, nil)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	names := reg.Names()
	for _, want := range []string{claudeharness.Name, piharness.Name} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("registry missing harness %q; has %v", want, names)
		}
	}
}

// TestNewRegistryDryRunBuilds confirms the registry builds under --dry-run (every
// harness wrapped) without error, so the plan command's dry-run path has a valid
// registry.
func TestNewRegistryDryRunBuilds(t *testing.T) {
	reg, err := newRegistry(config.Default(), true, nil)
	if err != nil {
		t.Fatalf("newRegistry(dryRun): %v", err)
	}
	if _, ok := reg.Get(claudeharness.Name); !ok {
		t.Errorf("dry-run registry missing the claude harness")
	}
}

// TestPromptDirFor maps a config path to its sibling prompts override dir, and
// tolerates an empty path.
func TestPromptDirFor(t *testing.T) {
	if got := promptDirFor(""); got != "" {
		t.Errorf("promptDirFor(\"\") = %q, want \"\"", got)
	}
	cfgPath := filepath.Join("home", ".aixecutor", "config.yaml")
	want := filepath.Join("home", ".aixecutor", promptOverrideSubdir)
	if got := promptDirFor(cfgPath); got != want {
		t.Errorf("promptDirFor(%q) = %q, want %q", cfgPath, got, want)
	}
}

// TestNewRendererDerivesOverrideDirs proves the renderer is built with the prompts
// dirs derived from the resolved config locations (honoring --config /
// --global-config), local before global.
func TestNewRendererDerivesOverrideDirs(t *testing.T) {
	dir := t.TempDir()
	localCfg := filepath.Join(dir, "local", ".aixecutor", "config.yaml")
	globalCfg := filepath.Join(dir, "global", ".aixecutor", "config.yaml")

	opts := &GlobalOptions{ConfigPath: localCfg, GlobalConfigPath: globalCfg}
	r, err := newRenderer(opts)
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}
	// The renderer falls back to embedded defaults when the override dirs are empty
	// or absent (these are), so it must still render a built-in role.
	out, err := r.Render("planner", map[string]any{"Task": "t", "RepoSummary": "s", "PriorError": ""})
	if err != nil {
		t.Fatalf("Render(planner) via derived renderer: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("expected embedded planner output via the derived renderer")
	}
}

// TestPreflightHarness checks the preset preflight dispatch: unknown names get a
// nil (no preset preflight), and the known names return whatever Available()
// reports (we only assert the dispatch wiring, not the binary's presence).
func TestPreflightHarness(t *testing.T) {
	if err := preflightHarness("some-generic-harness"); err != nil {
		t.Errorf("preflightHarness(unknown) = %v, want nil", err)
	}
	// claude/pi dispatch to the preset Available(); the result depends on PATH, so
	// just confirm the call routes (it must equal the preset's own result).
	if got, want := preflightHarness(claudeharness.Name), claudeharness.Available(); (got == nil) != (want == nil) {
		t.Errorf("preflightHarness(claude) routing mismatch: got %v, preset %v", got, want)
	}
	if got, want := preflightHarness(piharness.Name), piharness.Available(); (got == nil) != (want == nil) {
		t.Errorf("preflightHarness(pi) routing mismatch: got %v, preset %v", got, want)
	}
}
