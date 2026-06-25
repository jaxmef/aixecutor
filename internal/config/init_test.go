package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteScaffold writes a commented local config, refuses to clobber, and
// honors force.
func TestWriteScaffold(t *testing.T) {
	dir := t.TempDir()

	res, err := WriteScaffold(dir, false)
	if err != nil {
		t.Fatalf("WriteScaffold: %v", err)
	}
	if !res.Created {
		t.Fatal("expected Created=true on first write")
	}
	want := filepath.Join(dir, configDirName, configFileName)
	if res.Path != want {
		t.Errorf("path = %q, want %q", res.Path, want)
	}

	body := mustRead(t, res.Path)
	// The scaffold must document the surprising merge rules.
	for _, want := range []string{"REPLACE WHOLESALE", "key-by-key", "precedence"} {
		if !strings.Contains(body, want) {
			t.Errorf("scaffold missing explanation %q", want)
		}
	}
	// And it must be a valid no-op layer (everything commented out) — Load over
	// a repo containing only the scaffold must equal the defaults.
	repo := t.TempDir()
	if _, err := WriteScaffold(repo, false); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(LoadOptions{HomeDir: t.TempDir(), WorkingDir: repo})
	if err != nil {
		t.Fatalf("Load over scaffold: %v", err)
	}
	if cfg.Paths.RunsDir != Default().Paths.RunsDir {
		t.Errorf("scaffold is not a no-op layer: %+v", cfg.Paths)
	}

	// Second write without force must not clobber.
	res2, err := WriteScaffold(dir, false)
	if err != nil {
		t.Fatalf("WriteScaffold (second): %v", err)
	}
	if res2.Created {
		t.Error("expected Created=false when file already exists and force=false")
	}

	// Force overwrites.
	if err := os.WriteFile(want, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	res3, err := WriteScaffold(dir, true)
	if err != nil {
		t.Fatalf("WriteScaffold (force): %v", err)
	}
	if !res3.Created {
		t.Error("expected Created=true with force=true")
	}
	if mustRead(t, want) == "changed" {
		t.Error("force did not overwrite the file")
	}
}
