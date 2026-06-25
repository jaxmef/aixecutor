package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noInteractiveEnv is a taskInputEnv with no stdin and no editor — the "no way to
// prompt" case (CI/non-TTY), so resolveTaskInput's interactive fallback fails fast
// instead of blocking. Used by tests that exercise the non-interactive paths.
var noInteractiveEnv = taskInputEnv{}

// TestResolveTaskInputInlineString proves the inline-string path is unchanged: a
// single positional arg is returned verbatim, with no trimming or interpretation.
func TestResolveTaskInputInlineString(t *testing.T) {
	got, err := resolveTaskInput([]string{"  build the thing  "}, "", noInteractiveEnv)
	if err != nil {
		t.Fatalf("resolveTaskInput: %v", err)
	}
	if got != "  build the thing  " {
		t.Errorf("inline task altered: got %q", got)
	}
}

// TestResolveTaskInputTaskFile reads the task from --task-file, trims trailing
// whitespace, and preserves leading/internal whitespace and newlines.
func TestResolveTaskInputTaskFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.md")
	content := "# Title\n\nLine one\nLine two\n\n\t \n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveTaskInput(nil, path, noInteractiveEnv)
	if err != nil {
		t.Fatalf("resolveTaskInput: %v", err)
	}
	want := "# Title\n\nLine one\nLine two"
	if got != want {
		t.Errorf("task-file content = %q, want %q", got, want)
	}
}

// TestResolveTaskInputAtFileShorthand reads the task from the @<path> positional
// shorthand, and the @@ escape yields a literal task starting with '@'.
func TestResolveTaskInputAtFileShorthand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(path, []byte("from a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveTaskInput([]string{"@" + path}, "", noInteractiveEnv)
	if err != nil {
		t.Fatalf("resolveTaskInput(@file): %v", err)
	}
	if got != "from a file" {
		t.Errorf("@file task = %q, want %q", got, "from a file")
	}

	got, err = resolveTaskInput([]string{"@@literal @task"}, "", noInteractiveEnv)
	if err != nil {
		t.Fatalf("resolveTaskInput(@@): %v", err)
	}
	if got != "@literal @task" {
		t.Errorf("@@ escape = %q, want %q", got, "@literal @task")
	}
}

// TestResolveTaskInputExclusivity enforces exactly-one-of: neither source and both
// sources each fail with an actionable, exitUsage-classified error.
func TestResolveTaskInputExclusivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(path, []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("neither", func(t *testing.T) {
		_, err := resolveTaskInput(nil, "", noInteractiveEnv)
		assertUsageErr(t, err, "no task provided")
	})

	t.Run("both", func(t *testing.T) {
		_, err := resolveTaskInput([]string{"inline"}, path, noInteractiveEnv)
		assertUsageErr(t, err, "not both")
	})
}

// TestResolveTaskInputFileErrors covers the file guards: missing, a directory,
// blank content, and an over-size file each fail with a clear usage error.
func TestResolveTaskInputFileErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing", func(t *testing.T) {
		_, err := resolveTaskInput(nil, filepath.Join(dir, "nope.md"), noInteractiveEnv)
		assertUsageErr(t, err, "cannot read task file")
	})

	t.Run("directory", func(t *testing.T) {
		_, err := resolveTaskInput(nil, dir, noInteractiveEnv)
		assertUsageErr(t, err, "is a directory")
	})

	t.Run("blank", func(t *testing.T) {
		path := filepath.Join(dir, "blank.md")
		if err := os.WriteFile(path, []byte("  \n\t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := resolveTaskInput(nil, path, noInteractiveEnv)
		assertUsageErr(t, err, "is empty")
	})

	t.Run("oversize", func(t *testing.T) {
		path := filepath.Join(dir, "big.md")
		big := make([]byte, maxTaskFileSize+1)
		for i := range big {
			big[i] = 'x'
		}
		if err := os.WriteFile(path, big, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := resolveTaskInput(nil, path, noInteractiveEnv)
		assertUsageErr(t, err, "too large")
	})
}

// TestResolveTaskInputPipedStdin reads a multi-line task from non-TTY stdin and
// trims trailing whitespace, preserving internal newlines (AIX-0019 precedence 3).
func TestResolveTaskInputPipedStdin(t *testing.T) {
	env := taskInputEnv{stdin: strings.NewReader("line one\nline two\n\n"), stdinIsTTY: false}
	got, err := resolveTaskInput(nil, "", env)
	if err != nil {
		t.Fatalf("resolveTaskInput(stdin): %v", err)
	}
	if want := "line one\nline two"; got != want {
		t.Errorf("piped stdin task = %q, want %q", got, want)
	}
}

// TestResolveTaskInputEditor opens the injected editor, strips comment lines, and
// returns the multi-line task exactly (AIX-0019 precedence 4). It also asserts the
// editor is seeded with the commented header.
func TestResolveTaskInputEditor(t *testing.T) {
	var seenSeed string
	env := taskInputEnv{
		stdinIsTTY: true,
		launchEditor: func(initial string) (string, error) {
			seenSeed = initial
			return initial + "Add OAuth login\n\nUse Google as the provider\n", nil
		},
	}
	got, err := resolveTaskInput(nil, "", env)
	if err != nil {
		t.Fatalf("resolveTaskInput(editor): %v", err)
	}
	if want := "Add OAuth login\n\nUse Google as the provider"; got != want {
		t.Errorf("editor task = %q, want %q", got, want)
	}
	if !strings.Contains(seenSeed, "Lines starting with '#' are ignored") {
		t.Errorf("editor not seeded with the commented header; got %q", seenSeed)
	}
}

// TestResolveTaskInputEditorEmptyAborts proves an empty / all-comment editor buffer
// is a clean abort (errTaskAborted), not a usage error.
func TestResolveTaskInputEditorEmptyAborts(t *testing.T) {
	env := taskInputEnv{
		stdinIsTTY: true,
		launchEditor: func(initial string) (string, error) {
			return initial + "\n   \n", nil // only the seed comments + blanks
		},
	}
	_, err := resolveTaskInput(nil, "", env)
	if !errors.Is(err, errTaskAborted) {
		t.Errorf("empty editor result should abort cleanly; got %v", err)
	}
}

// TestResolveTaskInputNoEditorNonTTY proves the no-hang guard: a TTY with no
// editor launcher fails fast with an actionable usage error.
func TestResolveTaskInputNoEditorNonTTY(t *testing.T) {
	env := taskInputEnv{stdinIsTTY: true, launchEditor: nil}
	_, err := resolveTaskInput(nil, "", env)
	assertUsageErr(t, err, "no editor available")
}

// TestResolveTaskInputNoHang proves that with no positional/file source, no TTY,
// and empty stdin (CI/headless), resolution fails fast rather than blocking.
func TestResolveTaskInputNoHang(t *testing.T) {
	env := taskInputEnv{stdin: strings.NewReader(""), stdinIsTTY: false}
	_, err := resolveTaskInput(nil, "", env)
	assertUsageErr(t, err, "no task provided")
}

// TestResolveTaskInputStdinOversize proves piped stdin is capped: content over the
// limit is rejected rather than buffered without bound.
func TestResolveTaskInputStdinOversize(t *testing.T) {
	env := taskInputEnv{stdin: strings.NewReader(strings.Repeat("x", maxTaskFileSize+1)), stdinIsTTY: false}
	_, err := resolveTaskInput(nil, "", env)
	assertUsageErr(t, err, "too large")
}

// TestIsTTYReader pins the seam the no-hang guarantee rests on: a non-*os.File
// reader (the kind tests inject, and any pipe/redirect) is never a TTY, so the
// non-blocking stdin path is taken instead of the editor.
func TestIsTTYReader(t *testing.T) {
	if isTTYReader(strings.NewReader("")) {
		t.Error("a *strings.Reader must not be detected as a TTY")
	}
	// A regular file is an *os.File but not a character device → not a TTY.
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTTYReader(f) {
		t.Error("a regular file must not be detected as a TTY")
	}
}

// assertUsageErr asserts err is non-nil, mapped to exitUsage, and mentions want.
func assertUsageErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q, got nil", want)
	}
	if code := exitCodeFor(err); code != exitUsage {
		t.Errorf("exit code = %d, want exitUsage (%d); err: %v", code, exitUsage, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should contain %q", err.Error(), want)
	}
}
