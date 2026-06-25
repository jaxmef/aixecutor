package run

import (
	"path/filepath"
	"testing"

	"github.com/jaxmef/aixecutor/internal/config"
)

// TestRepoRelRunsDir verifies the repo-relative exclusion derived from
// paths.runsDir — the value the CLI hands to git.Gateway.SetExcludePrefixes so the
// baseline and the senior-review full diff skip the tool's own output dir. It
// covers the default, a custom relative runsDir, an absolute runsDir inside the
// repo, and an absolute runsDir OUTSIDE the repo (which yields no exclusion).
func TestRepoRelRunsDir(t *testing.T) {
	repo := t.TempDir()

	tests := []struct {
		name    string
		runsDir string
		want    string // "/"-separated; "" means no exclusion
	}{
		{
			name:    "default repo-relative runsDir",
			runsDir: "", // config.Default() leaves it as ".aixecutor/runs"
			want:    ".aixecutor/runs",
		},
		{
			name:    "custom relative runsDir",
			runsDir: "build/aix-runs",
			want:    "build/aix-runs",
		},
		{
			name:    "absolute runsDir inside the repo",
			runsDir: filepath.Join(repo, "out", "runs"),
			want:    "out/runs",
		},
		{
			name:    "absolute runsDir outside the repo yields no exclusion",
			runsDir: t.TempDir(), // a different temp dir, not under repo
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			if tc.runsDir != "" {
				cfg.Paths.RunsDir = tc.runsDir
			}
			got := RepoRelRunsDir(cfg, repo)
			want := filepath.FromSlash(tc.want)
			if got != want {
				t.Errorf("RepoRelRunsDir = %q; want %q", got, want)
			}
		})
	}
}

// TestRepoRelRunsDirEmptyRepoRoot confirms an empty repo root yields no exclusion
// rather than a bogus relative path (the CLI then simply sets no prefix).
func TestRepoRelRunsDirEmptyRepoRoot(t *testing.T) {
	if got := RepoRelRunsDir(config.Default(), ""); got != "" {
		t.Errorf("RepoRelRunsDir with empty repoRoot = %q; want \"\"", got)
	}
}
