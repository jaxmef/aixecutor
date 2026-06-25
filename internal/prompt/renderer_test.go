package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplate writes content to <dir>/<name>.tmpl, creating dir, and fails the
// test on error. It returns the file path.
func writeTemplate(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	path := filepath.Join(dir, name+templateExt)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// TestOverridePrecedence covers the acceptance criterion that a local override
// shadows a global override shadows the embedded default, for the same bare name.
// It builds the three tiers incrementally and asserts which content wins at each.
func TestOverridePrecedence(t *testing.T) {
	localDir := t.TempDir()
	globalDir := t.TempDir()

	// Renderer searches local first, then global (the config layering order).
	r := NewRenderer(localDir, globalDir)

	// Use a real built-in name so the embedded tier is meaningful.
	const role = "executor"
	data := sampleContexts()[role]

	// Tier 3: no override files → embedded default. The embedded executor carries
	// the git-safety preamble, which our tiny overrides below will not.
	embedded, err := r.Render(role, data)
	if err != nil {
		t.Fatalf("embedded render: %v", err)
	}
	if !strings.Contains(embedded, "## Git safety") {
		t.Fatalf("embedded executor unexpectedly lacks git-safety preamble")
	}

	// Tier 2: a global override shadows the embedded default. It still includes
	// the partial so that overrides keep working with git-safety.
	writeTemplate(t, globalDir, role, "GLOBAL override\n{{template \"git-safety\" .}}\n")
	out, err := r.Render(role, data)
	if err != nil {
		t.Fatalf("global render: %v", err)
	}
	if !strings.Contains(out, "GLOBAL override") {
		t.Errorf("global override did not shadow embedded default; got:\n%s", out)
	}
	if !strings.Contains(out, "## Git safety") {
		t.Errorf("global override lost the git-safety partial (partial not shared into overrides)")
	}

	// Tier 1: a local override shadows the global override.
	writeTemplate(t, localDir, role, "LOCAL override\n{{template \"git-safety\" .}}\n")
	out, err = r.Render(role, data)
	if err != nil {
		t.Fatalf("local render: %v", err)
	}
	if !strings.Contains(out, "LOCAL override") {
		t.Errorf("local override did not shadow global override; got:\n%s", out)
	}
	if strings.Contains(out, "GLOBAL override") {
		t.Errorf("global override still present after local override added; got:\n%s", out)
	}
}

// TestOverrideForArbitraryName proves a bare name with no embedded default still
// resolves when an override file provides it (override dirs are searched before
// the embedded fallback).
func TestOverrideForArbitraryName(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "custom-role", "custom {{.Task}}\n")
	r := NewRenderer(dir)

	out, err := r.Render("custom-role", map[string]any{"Task": "hello"})
	if err != nil {
		t.Fatalf("Render(custom-role): %v", err)
	}
	if !strings.Contains(out, "custom hello") {
		t.Errorf("unexpected output: %q", out)
	}
}

// TestExplicitPathLoads covers resolving roles.<role>.promptTemplate given as a
// file path rather than a bare name: the exact file is loaded, bypassing the
// override-dir search, and partials are still available.
func TestExplicitPathLoads(t *testing.T) {
	dir := t.TempDir()
	// A path NOT inside any override dir, referenced explicitly.
	path := filepath.Join(dir, "my-prompt.tmpl")
	if err := os.WriteFile(path, []byte("explicit {{.Task}}\n{{template \"git-safety\" .}}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// No override dirs configured at all.
	r := NewRenderer()
	out, err := r.Render(path, map[string]any{"Task": "X"})
	if err != nil {
		t.Fatalf("Render(path): %v", err)
	}
	if !strings.Contains(out, "explicit X") {
		t.Errorf("explicit-path template not rendered: %q", out)
	}
	if !strings.Contains(out, "## Git safety") {
		t.Errorf("explicit-path template lost the git-safety partial")
	}
}

// TestRenderUnknownTemplate covers the acceptance criterion that an unknown
// template name fails clearly, naming the template.
func TestRenderUnknownTemplate(t *testing.T) {
	r := NewRenderer(t.TempDir())
	_, err := r.Render("does-not-exist", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unknown template, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the unknown template; got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown template") {
		t.Errorf("error should say the template is unknown; got: %v", err)
	}
}

// TestRenderExplicitPathMissing covers a missing explicit path producing a clear,
// named error rather than silently falling back to a default.
func TestRenderExplicitPathMissing(t *testing.T) {
	r := NewRenderer()
	missing := filepath.Join(t.TempDir(), "nope.tmpl")
	_, err := r.Render(missing, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing explicit path, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the missing path; got: %v", err)
	}
}

// TestRenderMissingContextField covers the acceptance criterion that a missing
// context field fails clearly. Two cases: a map context missing a key (caught by
// missingkey=error), and a struct context missing a field (caught by
// text/template's field lookup). Both must name the offending field.
func TestRenderMissingContextField(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing map key", func(t *testing.T) {
		writeTemplate(t, dir, "needs-field", "value: {{.Missing}}\n")
		r := NewRenderer(dir)
		_, err := r.Render("needs-field", map[string]any{"Present": "x"})
		if err == nil {
			t.Fatal("expected error for missing map key, got nil")
		}
		if !strings.Contains(err.Error(), "Missing") {
			t.Errorf("error should name the missing key 'Missing'; got: %v", err)
		}
		if !strings.Contains(err.Error(), "needs-field") {
			t.Errorf("error should name the template; got: %v", err)
		}
	})

	t.Run("missing struct field", func(t *testing.T) {
		writeTemplate(t, dir, "needs-struct-field", "value: {{.Nonexistent}}\n")
		r := NewRenderer(dir)
		// Pass a struct that has no field "Nonexistent".
		_, err := r.Render("needs-struct-field", BaselineInfo{Description: "x"})
		if err == nil {
			t.Fatal("expected error for missing struct field, got nil")
		}
		if !strings.Contains(err.Error(), "Nonexistent") {
			t.Errorf("error should name the missing field 'Nonexistent'; got: %v", err)
		}
		if !strings.Contains(err.Error(), "needs-struct-field") {
			t.Errorf("error should name the template; got: %v", err)
		}
	})
}

// TestRenderParseError covers a malformed override template producing a clear
// parse error that names the template (file path) for actionability.
func TestRenderParseError(t *testing.T) {
	dir := t.TempDir()
	path := writeTemplate(t, dir, "broken", "{{.Unclosed \n")
	r := NewRenderer(dir)

	_, err := r.Render("broken", map[string]any{})
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing template") {
		t.Errorf("error should indicate a parse failure; got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the template file %q; got: %v", path, err)
	}
}

// TestRenderEmptyName rejects an empty template name with a clear error.
func TestRenderEmptyName(t *testing.T) {
	r := NewRenderer()
	_, err := r.Render("", nil)
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
	if !strings.Contains(err.Error(), "empty template name") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestEmptyAndMissingOverrideDirsTolerated confirms empty-string and
// non-existent override dirs are skipped (not errors), so callers can pass paths
// unconditionally and still fall through to the embedded default.
func TestEmptyAndMissingOverrideDirsTolerated(t *testing.T) {
	r := NewRenderer("", filepath.Join(t.TempDir(), "does-not-exist"))
	out, err := r.Render("planner", sampleContexts()["planner"])
	if err != nil {
		t.Fatalf("Render with empty/missing override dirs should fall back to embedded: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("expected embedded default output")
	}
}

// gitSafetyMarker is a distinctive sentence that only the git-safety partial
// contains; its presence proves the preamble was emitted.
const gitSafetyMarker = "never** commits on the user's"

// TestGitSafetyGuaranteedWhenOverrideOmitsInclude is the regression for the
// blocking issue: an override template that does NOT reference the git-safety
// partial must still render with the full preamble (invariant #1). It also checks
// the forbidden verbs are present, not just the marker sentence.
func TestGitSafetyGuaranteedWhenOverrideOmitsInclude(t *testing.T) {
	dir := t.TempDir()
	// An executor override with NO {{template "git-safety" .}} line at all.
	writeTemplate(t, dir, "executor", "Custom executor prompt for {{.Task}}. No safety include here.\n")
	r := NewRenderer(dir)

	out, err := r.Render("executor", sampleContexts()["executor"])
	if err != nil {
		t.Fatalf("Render(executor override): %v", err)
	}
	if !strings.Contains(out, "Custom executor prompt") {
		t.Fatalf("override body was not used; got:\n%s", out)
	}
	if !strings.Contains(out, gitSafetyMarker) {
		t.Errorf("git-safety preamble was NOT appended to an override that omitted the include; got:\n%s", out)
	}
	if !strings.Contains(out, "## Git safety") {
		t.Errorf("git-safety heading missing from override render")
	}
	for _, verb := range []string{"git commit", "git push", "git add", "git reset", "git rebase"} {
		if !strings.Contains(out, verb) {
			t.Errorf("appended git-safety preamble does not forbid %q", verb)
		}
	}
	if !strings.Contains(out, "unstaged") {
		t.Errorf("appended git-safety preamble should instruct leaving changes unstaged")
	}
}

// TestGitSafetyGuaranteedForAllRolesOnOverride extends the guarantee to every
// built-in role: an include-less override for each of the four roles still emits
// the preamble.
func TestGitSafetyGuaranteedForAllRolesOnOverride(t *testing.T) {
	for _, role := range BuiltinRoles {
		role := role
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			writeTemplate(t, dir, role, "Override for "+role+", no safety include.\n")
			r := NewRenderer(dir)
			// The override ignores context, so any data renders; use an empty map.
			out, err := r.Render(role, map[string]any{})
			if err != nil {
				t.Fatalf("Render(%q override): %v", role, err)
			}
			if !strings.Contains(out, gitSafetyMarker) {
				t.Errorf("role %q: git-safety preamble missing from include-less override", role)
			}
		})
	}
}

// TestGitSafetyNoDuplicationWhenIncludePresent proves the dedupe: a template that
// DOES reference the partial gets exactly one copy (the guarantee does not append
// a second). It covers the whitespace variants text/template allows.
func TestGitSafetyNoDuplicationWhenIncludePresent(t *testing.T) {
	variants := map[string]string{
		"plain":        "Body.\n{{template \"git-safety\" .}}\n",
		"spaced":       "Body.\n{{ template \"git-safety\" . }}\n",
		"trim markers": "Body.\n{{- template \"git-safety\" . -}}\n",
	}
	for name, body := range variants {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTemplate(t, dir, "executor", body)
			r := NewRenderer(dir)

			out, err := r.Render("executor", sampleContexts()["executor"])
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if n := strings.Count(out, "## Git safety"); n != 1 {
				t.Errorf("expected exactly 1 git-safety block, got %d; output:\n%s", n, out)
			}
		})
	}
}

// TestGitSafetyGuaranteedForExplicitPath confirms the guarantee also covers an
// explicit file-path template (roles.<role>.promptTemplate given as a path) that
// omits the include.
func TestGitSafetyGuaranteedForExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.tmpl")
	if err := os.WriteFile(path, []byte("Explicit-path body, no include. {{.Task}}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := NewRenderer()
	out, err := r.Render(path, map[string]any{"Task": "X"})
	if err != nil {
		t.Fatalf("Render(path): %v", err)
	}
	if !strings.Contains(out, gitSafetyMarker) {
		t.Errorf("explicit-path template without include did not get the git-safety preamble; got:\n%s", out)
	}
}

// TestReferencesGitSafety unit-tests the dedupe detector directly across the
// whitespace forms text/template permits and a couple of negatives.
func TestReferencesGitSafety(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`{{template "git-safety" .}}`, true},
		{`{{ template "git-safety" . }}`, true},
		{`{{-  template   "git-safety"  . -}}`, true},
		{"prefix\n{{template \"git-safety\" .}}\nsuffix", true},
		{`no include here`, false},
		{`mentions "git-safety" but not as a template action`, false},
		{`{{template "other" .}}`, false},
	}
	for _, c := range cases {
		if got := referencesGitSafety(c.src); got != c.want {
			t.Errorf("referencesGitSafety(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}
