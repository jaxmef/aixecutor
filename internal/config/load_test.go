package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a config.yaml under <dir>/.aixecutor/, creating dirs.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	cfgDir := filepath.Join(dir, configDirName)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", cfgDir, err)
	}
	path := filepath.Join(cfgDir, configFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// emptyHome returns a temp dir usable as HOME that has no .aixecutor, so the
// global layer is absent.
func emptyHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestLoadNoFilesReturnsDefault proves the tool resolves a complete, valid
// config with zero config files present (invariant #2).
func TestLoadNoFilesReturnsDefault(t *testing.T) {
	cfg, sources, err := Load(LoadOptions{
		HomeDir:    emptyHome(t),
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Version != Default().Version || cfg.Paths.RunsDir != Default().Paths.RunsDir {
		t.Errorf("Load with no files did not return defaults: %+v", cfg)
	}
	// Every source should be the default origin.
	for _, s := range sources {
		if s.Origin != OriginDefault {
			t.Errorf("source %s origin = %s, want default", s.Path, s.Origin)
		}
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("loaded default does not validate: %v", err)
	}
}

// TestLoadDeepMergeProof is the headline deep-merge test: a local file setting
// ONLY roles.executor.harness changes that one field and leaves every sibling
// default intact.
func TestLoadDeepMergeProof(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "roles:\n  executor:\n    harness: pi\n")

	cfg, sources, err := Load(LoadOptions{
		HomeDir:    emptyHome(t),
		WorkingDir: repo,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	d := Default()
	ex := cfg.Roles.Executor
	if ex.Harness != "pi" {
		t.Errorf("executor.harness = %q, want pi", ex.Harness)
	}
	// Siblings must be untouched.
	if ex.Model != d.Roles.Executor.Model {
		t.Errorf("executor.model = %q, want default %q (sibling wiped!)", ex.Model, d.Roles.Executor.Model)
	}
	if ex.PermissionMode != d.Roles.Executor.PermissionMode {
		t.Errorf("executor.permissionMode = %q, want default %q", ex.PermissionMode, d.Roles.Executor.PermissionMode)
	}
	if ex.PromptTemplate != d.Roles.Executor.PromptTemplate {
		t.Errorf("executor.promptTemplate = %q, want default %q", ex.PromptTemplate, d.Roles.Executor.PromptTemplate)
	}
	if ex.Timeout != d.Roles.Executor.Timeout {
		t.Errorf("executor.timeout = %v, want default %v", ex.Timeout, d.Roles.Executor.Timeout)
	}
	// Other roles untouched too.
	if cfg.Roles.Planner != d.Roles.Planner {
		t.Errorf("planner changed: %+v vs default %+v", cfg.Roles.Planner, d.Roles.Planner)
	}

	// Provenance: only executor.harness is local; siblings are default.
	originOf := func(path string) Origin {
		for _, s := range sources {
			if s.Path == path {
				return s.Origin
			}
		}
		return OriginDefault
	}
	if got := originOf("roles.executor.harness"); got != OriginLocal {
		t.Errorf("provenance roles.executor.harness = %s, want local", got)
	}
	if got := originOf("roles.executor.model"); got != OriginDefault {
		t.Errorf("provenance roles.executor.model = %s, want default", got)
	}
}

// TestLoadUpdateDeepMerge proves a local layer setting only update.check flips
// that one key while update.interval keeps its default (per-key map merge).
func TestLoadUpdateDeepMerge(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "update:\n  check: false\n")

	cfg, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Update.Check {
		t.Error("update.check = true, want false (from local)")
	}
	if cfg.Update.Interval.Std() != 24*time.Hour {
		t.Errorf("update.interval = %v, want default 24h (sibling wiped!)", cfg.Update.Interval)
	}
}

// TestLoadListReplaceWholesale proves overriding harnesses.claude.args replaces
// the whole list rather than appending.
func TestLoadListReplaceWholesale(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "harnesses:\n  claude:\n    args:\n      - \"--only\"\n")

	cfg, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Harnesses["claude"].Args
	if len(got) != 1 || got[0] != "--only" {
		t.Errorf("claude.args = %v, want exactly [--only] (list must replace wholesale)", got)
	}
	// Sibling scalar under the same harness must survive the map merge.
	if cfg.Harnesses["claude"].Command != "claude" {
		t.Errorf("claude.command = %q, want default claude", cfg.Harnesses["claude"].Command)
	}
}

// TestLoadPrecedence proves local > global > default, and that absent keys at a
// higher layer fall through to lower ones.
func TestLoadPrecedence(t *testing.T) {
	home := t.TempDir()
	// Global sets planner.model and maxParallel.
	writeConfig(t, home, "roles:\n  planner:\n    model: haiku\npipeline:\n  execution:\n    maxParallel: 8\n")

	repo := t.TempDir()
	// Local overrides maxParallel (wins over global) but not planner.model.
	writeConfig(t, repo, "pipeline:\n  execution:\n    maxParallel: 2\n")

	cfg, sources, err := Load(LoadOptions{
		HomeDir:          home,
		GlobalConfigPath: filepath.Join(home, configDirName, configFileName),
		WorkingDir:       repo,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Roles.Planner.Model != "haiku" {
		t.Errorf("planner.model = %q, want haiku (from global)", cfg.Roles.Planner.Model)
	}
	if cfg.Pipeline.Execution.MaxParallel != 2 {
		t.Errorf("maxParallel = %d, want 2 (local over global)", cfg.Pipeline.Execution.MaxParallel)
	}

	originOf := func(path string) Origin {
		for _, s := range sources {
			if s.Path == path {
				return s.Origin
			}
		}
		return OriginDefault
	}
	if got := originOf("roles.planner.model"); got != OriginGlobal {
		t.Errorf("provenance planner.model = %s, want global", got)
	}
	if got := originOf("pipeline.execution.maxParallel"); got != OriginLocal {
		t.Errorf("provenance maxParallel = %s, want local", got)
	}
}

// TestLoadFlagOverridesAll proves --docs-path (mapped to paths.runsDir) wins
// over every file layer.
func TestLoadFlagOverridesAll(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "paths:\n  runsDir: from-local\n")

	cfg, sources, err := Load(LoadOptions{
		HomeDir:          emptyHome(t),
		WorkingDir:       repo,
		DocsPathOverride: "from-flag",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Paths.RunsDir != "from-flag" {
		t.Errorf("paths.runsDir = %q, want from-flag (flag beats local)", cfg.Paths.RunsDir)
	}
	originOf := func(path string) Origin {
		for _, s := range sources {
			if s.Path == path {
				return s.Origin
			}
		}
		return OriginDefault
	}
	if got := originOf("paths.runsDir"); got != OriginFlag {
		t.Errorf("provenance paths.runsDir = %s, want flag", got)
	}
}

// TestLoadStrictRejectsUnknownKey proves typo protection at the top level and
// nested levels.
func TestLoadStrictRejectsUnknownKey(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown top-level key", "bogusTopKey: 1\n"},
		{"unknown nested key", "pipeline:\n  execution:\n    bogus: 1\n"},
		{"misspelled known key", "paths:\n  runDir: x\n"}, // runsDir typo
		{"unknown key under update", "update:\n  intervl: 1h\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeConfig(t, repo, tc.body)
			_, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.body)
			}
		})
	}
}

// TestLoadValidationError surfaces a semantic validation failure from a loaded
// file (worktree isolation without allow-worktree).
func TestLoadValidationError(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "pipeline:\n  execution:\n    isolation: worktree\n")

	_, sources, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "requires git.policy") {
		t.Errorf("error %q does not explain the worktree requirement", err.Error())
	}
	// Provenance is still returned on validation failure.
	if sources == nil {
		t.Error("expected provenance on validation failure")
	}
}

// TestLoadExplicitPaths honors --config / --global-config pointing at arbitrary
// files (not under .aixecutor).
func TestLoadExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "g.yaml")
	lp := filepath.Join(dir, "l.yaml")
	if err := os.WriteFile(gp, []byte("roles:\n  planner:\n    model: haiku\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("roles:\n  executor:\n    model: opus\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load(LoadOptions{
		HomeDir:          emptyHome(t),
		GlobalConfigPath: gp,
		LocalConfigPath:  lp,
		WorkingDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Roles.Planner.Model != "haiku" {
		t.Errorf("planner.model = %q, want haiku from explicit --global-config", cfg.Roles.Planner.Model)
	}
	if cfg.Roles.Executor.Model != "opus" {
		t.Errorf("executor.model = %q, want opus from explicit --config", cfg.Roles.Executor.Model)
	}
}

// TestLoadUpwardWalk finds a repo-root .aixecutor/config.yaml from a nested CWD.
func TestLoadUpwardWalk(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "paths:\n  runsDir: found-by-walk\n")
	nested := filepath.Join(repo, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: nested})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Paths.RunsDir != "found-by-walk" {
		t.Errorf("runsDir = %q, want found-by-walk (upward walk failed)", cfg.Paths.RunsDir)
	}
}

// TestLoadEmptyLayerFile treats an empty config file as a no-op layer.
func TestLoadEmptyLayerFile(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "")
	cfg, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
	if err != nil {
		t.Fatalf("Load with empty file: %v", err)
	}
	if cfg.Paths.RunsDir != Default().Paths.RunsDir {
		t.Errorf("empty file changed config: %+v", cfg.Paths)
	}
}

// TestDurationRoundTrip confirms the custom Duration parses schema strings and
// survives the marshal→merge→decode round trip.
func TestDurationRoundTrip(t *testing.T) {
	repo := t.TempDir()
	writeConfig(t, repo, "roles:\n  planner:\n    timeout: 45m\n")
	cfg, _, err := Load(LoadOptions{HomeDir: emptyHome(t), WorkingDir: repo})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Roles.Planner.Timeout.Std() != 45*time.Minute {
		t.Errorf("planner.timeout = %v, want 45m", cfg.Roles.Planner.Timeout)
	}
	// Default timeouts still parse from the marshaled default map.
	if cfg.Roles.Executor.Timeout.Std() != 30*time.Minute {
		t.Errorf("executor.timeout = %v, want 30m", cfg.Roles.Executor.Timeout)
	}
}

// TestLocations reports resolved paths and existence for config path.
func TestLocations(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, "version: 1\n")
	repo := t.TempDir() // no local config

	locs, err := LoadOptions{
		HomeDir:          home,
		GlobalConfigPath: filepath.Join(home, configDirName, configFileName),
		WorkingDir:       repo,
	}.Locations()
	if err != nil {
		t.Fatalf("Locations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("got %d locations, want 2", len(locs))
	}
	if locs[0].Origin != OriginGlobal || !locs[0].Exists {
		t.Errorf("global location wrong: %+v", locs[0])
	}
	if locs[1].Origin != OriginLocal || locs[1].Exists {
		t.Errorf("local location should not exist: %+v", locs[1])
	}
}
