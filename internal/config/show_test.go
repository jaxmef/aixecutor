package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden, when set via `go test -run TestShowGolden -update`, rewrites the
// golden file from the current Render output. Keep the golden under version
// control; review diffs when it changes.
var updateGolden = flag.Bool("update", false, "update golden files")

// TestShowGolden renders a config with a known global+local overlay and compares
// it to a committed golden file, exercising provenance annotations end to end.
func TestShowGolden(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, mustRead(t, filepath.Join("..", "..", "testdata", "config", "global.yaml")))

	repo := t.TempDir()
	writeConfig(t, repo, mustRead(t, filepath.Join("..", "..", "testdata", "config", "local.yaml")))

	cfg, sources, err := Load(LoadOptions{
		HomeDir:          home,
		GlobalConfigPath: filepath.Join(home, configDirName, configFileName),
		WorkingDir:       repo,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, err := Render(cfg, sources)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The golden file embeds tmp paths from the source files, which differ per
	// run. Normalize them to stable tokens before comparing/writing.
	got = strings.ReplaceAll(got, filepath.Join(home, configDirName, configFileName), "<GLOBAL>")
	got = strings.ReplaceAll(got, filepath.Join(repo, configDirName, configFileName), "<LOCAL>")

	goldenPath := filepath.Join("testdata", "show_golden.yaml")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated golden %s", goldenPath)
		return
	}

	want := mustRead(t, goldenPath)
	if got != want {
		t.Errorf("config show output mismatch.\n--- got ---\n%s\n--- want ---\n%s\n(run: go test ./internal/config -run TestShowGolden -update)", got, want)
	}
}

// TestShowNoFilesIsPlain renders the default with no overlays: there should be
// no provenance annotations at all.
func TestShowNoFilesIsPlain(t *testing.T) {
	cfg, sources, err := Load(LoadOptions{HomeDir: t.TempDir(), WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := Render(cfg, sources)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "# from ") {
		t.Errorf("default-only render should have no provenance comments:\n%s", out)
	}
	// Sanity: it should still contain the schema's top-level keys.
	for _, key := range []string{"version:", "paths:", "harnesses:", "roles:", "pipeline:", "git:", "backlog:", "workspace:"} {
		if !strings.Contains(out, key) {
			t.Errorf("render missing %q", key)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
