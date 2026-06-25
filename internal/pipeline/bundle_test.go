package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseBundleBasic parses a well-formed bundle and checks each document's
// content is captured between its marker and the next (text before the first
// marker is ignored).
func TestParseBundleBasic(t *testing.T) {
	const raw = "preamble chatter, ignored\n" +
		"@@AIXECUTOR_DOC:plan.md@@\n" +
		"line 1\nline 2\n" +
		"@@AIXECUTOR_DOC:subtasks.yaml@@\n" +
		"subtasks: []\n"

	docs, err := parseBundle(raw)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}
	// Bodies are normalized to end in exactly one newline.
	if got := docs["plan.md"]; got != "line 1\nline 2\n" {
		t.Errorf("plan.md = %q, want %q", got, "line 1\nline 2\n")
	}
	if got := docs["subtasks.yaml"]; got != "subtasks: []\n" {
		t.Errorf("subtasks.yaml = %q, want %q", got, "subtasks: []\n")
	}
	if _, ok := docs["preamble"]; ok {
		t.Error("preamble text before the first marker should be ignored, not captured")
	}
}

// TestParseBundleNoMarkers errors clearly when the response has no markers at all.
func TestParseBundleNoMarkers(t *testing.T) {
	_, err := parseBundle("just some prose with no markers, like a refusal")
	if err == nil {
		t.Fatal("parseBundle with no markers should error")
	}
	if !strings.Contains(err.Error(), "@@AIXECUTOR_DOC:") {
		t.Errorf("error should mention the marker form; got: %v", err)
	}
}

// TestParseBundleEmpty errors on an empty/whitespace response.
func TestParseBundleEmpty(t *testing.T) {
	if _, err := parseBundle("   \n\t "); err == nil {
		t.Error("parseBundle on empty input should error")
	}
}

// TestParseBundleMarkerRobustness proves a marker-looking string that is NOT alone
// on its line (embedded mid-line in document content) does not split the document.
func TestParseBundleMarkerRobustness(t *testing.T) {
	const raw = "@@AIXECUTOR_DOC:plan.md@@\n" +
		"This sentence mentions @@AIXECUTOR_DOC:context.md@@ inline and must stay in plan.md.\n" +
		"@@AIXECUTOR_DOC:context.md@@\n" +
		"real context\n"

	docs, err := parseBundle(raw)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}
	if !strings.Contains(docs["plan.md"], "inline and must stay in plan.md") {
		t.Errorf("inline marker-looking text was wrongly split out of plan.md: %q", docs["plan.md"])
	}
	if docs["context.md"] != "real context\n" {
		t.Errorf("context.md = %q, want %q", docs["context.md"], "real context\n")
	}
}

// TestParseMarkerLine unit-tests the marker recognizer across exact, padded, and
// non-marker lines.
func TestParseMarkerLine(t *testing.T) {
	cases := []struct {
		line     string
		wantName string
		wantOK   bool
	}{
		{"@@AIXECUTOR_DOC:plan.md@@", "plan.md", true},
		{"  @@AIXECUTOR_DOC:subtasks.yaml@@  ", "subtasks.yaml", true},
		{"@@AIXECUTOR_DOC:@@", "", false}, // empty name
		{"text @@AIXECUTOR_DOC:plan.md@@ more", "", false},
		{"## Git safety", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := parseMarkerLine(c.line)
		if ok != c.wantOK || name != c.wantName {
			t.Errorf("parseMarkerLine(%q) = (%q, %v), want (%q, %v)", c.line, name, ok, c.wantName, c.wantOK)
		}
	}
}

// TestRequireAllDocs reports exactly the missing/empty required documents.
func TestRequireAllDocs(t *testing.T) {
	// All present → nil.
	full := map[string]string{
		planDocName:          "p",
		contextDocName:       "c",
		manualTestingDocName: "m",
		subtasksDocName:      "s",
	}
	if err := requireAllDocs(full); err != nil {
		t.Errorf("requireAllDocs(full) = %v, want nil", err)
	}

	// One missing, one empty → both named.
	partial := map[string]string{
		planDocName:          "p",
		contextDocName:       "   ", // whitespace-only counts as empty
		manualTestingDocName: "m",
	}
	err := requireAllDocs(partial)
	if err == nil {
		t.Fatal("requireAllDocs(partial) = nil, want error")
	}
	for _, want := range []string{contextDocName, subtasksDocName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name missing doc %q", err.Error(), want)
		}
	}
}

// fakeLister is a hermetic fileLister for the summarizer test.
type fakeLister struct {
	files []string
	err   error
}

func (f fakeLister) TrackedFiles(context.Context) ([]string, error) { return f.files, f.err }

// TestGitRepoSummarizer covers the bounded summary: sorted file tree, a README
// excerpt when one is present, and the "… and N more" elision past the file budget.
func TestGitRepoSummarizer(t *testing.T) {
	repo := t.TempDir()
	// A README to excerpt.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Title\nFirst line.\nSecond line.\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	files := []string{"README.md", "z/last.go", "a/first.go", "main.go"}
	s := NewGitRepoSummarizer(fakeLister{files: files}, repo)
	out, err := s.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	// README excerpt present.
	if !strings.Contains(out, "README (excerpt)") || !strings.Contains(out, "First line.") {
		t.Errorf("summary missing README excerpt:\n%s", out)
	}
	// Files listed and sorted (a/first.go before main.go before z/last.go).
	if strings.Index(out, "a/first.go") > strings.Index(out, "main.go") ||
		strings.Index(out, "main.go") > strings.Index(out, "z/last.go") {
		t.Errorf("file tree not sorted:\n%s", out)
	}
}

// TestGitRepoSummarizerNoReadme tolerates a repo with no README: the excerpt is
// simply omitted and the file tree is still produced.
func TestGitRepoSummarizerNoReadme(t *testing.T) {
	s := NewGitRepoSummarizer(fakeLister{files: []string{"main.go"}}, t.TempDir())
	out, err := s.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if strings.Contains(out, "README (excerpt)") {
		t.Errorf("summary should omit the README section when none exists:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("summary missing the file tree:\n%s", out)
	}
}

// TestFileTreeBudget proves the file-count elision past summaryMaxFiles.
func TestFileTreeBudget(t *testing.T) {
	many := make([]string, summaryMaxFiles+5)
	for i := range many {
		many[i] = filepath.Join("pkg", "file"+string(rune('a'+i%26))+itoa(i)+".go")
	}
	out := fileTree(many)
	if !strings.Contains(out, "… and 5 more") {
		t.Errorf("expected elision of 5 extra files; got:\n%s", out)
	}
}

// itoa is a tiny int→string helper to avoid importing strconv just for the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
