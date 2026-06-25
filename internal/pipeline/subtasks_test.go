package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaxmef/aixecutor/internal/run"
)

// fixturePath resolves a subtasks fixture under testdata/pipeline/subtasks.
func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "pipeline", "subtasks", name)
}

// readFixture reads a subtasks fixture file or fails the test.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("reading fixture %q: %v", name, err)
	}
	return data
}

// TestParseSubtasks is the table-driven parse + DAG-validation suite over the
// testdata fixtures: one valid case and one for each rejection reason (empty
// list, missing field, duplicate id, dangling dep, self-dep, cycle, unknown key).
func TestParseSubtasks(t *testing.T) {
	cases := []struct {
		name       string
		fixture    string
		wantErr    bool
		errSubstrs []string // all must appear in the error (case-insensitive)
	}{
		{name: "valid", fixture: "valid.yaml", wantErr: false},
		{
			name: "empty list", fixture: "empty-list.yaml", wantErr: true,
			errSubstrs: []string{"empty"},
		},
		{
			name: "missing title", fixture: "missing-title.yaml", wantErr: true,
			errSubstrs: []string{"st-01", "title"},
		},
		{
			name: "duplicate id", fixture: "duplicate-id.yaml", wantErr: true,
			errSubstrs: []string{"duplicate", "st-01"},
		},
		{
			name: "dangling dep", fixture: "dangling-dep.yaml", wantErr: true,
			errSubstrs: []string{"st-01", "unknown id", "st-99"},
		},
		{
			name: "self dep", fixture: "self-dep.yaml", wantErr: true,
			errSubstrs: []string{"st-01", "itself"},
		},
		{
			name: "cycle", fixture: "cycle.yaml", wantErr: true,
			errSubstrs: []string{"cycle", "st-01", "st-02", "st-03"},
		},
		{
			name: "unknown key (strict decode)", fixture: "unknown-key.yaml", wantErr: true,
			errSubstrs: []string{"parsing subtasks.yaml"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSubtasks(readFixture(t, tc.fixture))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSubtasks(%s) = nil error, want error", tc.fixture)
				}
				low := strings.ToLower(err.Error())
				for _, sub := range tc.errSubstrs {
					if !strings.Contains(low, strings.ToLower(sub)) {
						t.Errorf("error %q does not contain %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSubtasks(%s) unexpected error: %v", tc.fixture, err)
			}
			if len(got) == 0 {
				t.Fatal("expected parsed subtasks, got none")
			}
			// Every parsed subtask must be initialized pending.
			for _, s := range got {
				if s.Status != run.SubtaskPending {
					t.Errorf("subtask %q status = %q, want pending", s.ID, s.Status)
				}
			}
		})
	}
}

// TestParseSubtasksMapping verifies the field mapping from the YAML spec onto the
// run model, including the acceptance-list join and the deps/files passthrough.
func TestParseSubtasksMapping(t *testing.T) {
	got, err := ParseSubtasks(readFixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("ParseSubtasks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d subtasks, want 3", len(got))
	}

	first := got[0]
	if first.ID != "st-01" || first.Title != "Add config schema" {
		t.Errorf("first subtask id/title mismatch: %+v", first)
	}
	if len(first.Files) != 1 || first.Files[0] != "internal/config/**" {
		t.Errorf("first subtask files mismatch: %v", first.Files)
	}
	// Acceptance list is joined into a bulleted string.
	if !strings.Contains(first.Acceptance, "- Config struct mirrors") ||
		!strings.Contains(first.Acceptance, "- Default() returns") {
		t.Errorf("acceptance not joined as bullets: %q", first.Acceptance)
	}

	third := got[2]
	if len(third.Deps) != 2 || third.Deps[0] != "st-01" || third.Deps[1] != "st-02" {
		t.Errorf("third subtask deps mismatch: %v", third.Deps)
	}
}

// TestParseSubtasksEmptyInput rejects empty/whitespace input with a clear error,
// independent of YAML decoding.
func TestParseSubtasksEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   \n  \t"} {
		if _, err := ParseSubtasks([]byte(in)); err == nil {
			t.Errorf("ParseSubtasks(%q) = nil error, want error", in)
		}
	}
}

// TestValidateDAGAcceptsDiamond proves a non-trivial DAG with a diamond (two
// independent deps converging) is accepted — the cycle detector must not flag a
// node reachable by multiple paths as a cycle.
func TestValidateDAGAcceptsDiamond(t *testing.T) {
	diamond := []subtaskSpec{
		{ID: "a", Title: "A", Description: "root"},
		{ID: "b", Title: "B", Description: "left", Deps: []string{"a"}},
		{ID: "c", Title: "C", Description: "right", Deps: []string{"a"}},
		{ID: "d", Title: "D", Description: "join", Deps: []string{"b", "c"}},
	}
	if err := ValidateDAG(diamond); err != nil {
		t.Errorf("ValidateDAG(diamond) = %v, want nil (a diamond is a valid DAG)", err)
	}
}
