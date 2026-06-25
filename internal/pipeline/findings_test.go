package pipeline

import (
	"strings"
	"testing"
)

// TestParseVerdict covers the reviewer-verdict parser: well-formed verdicts
// (approved true/false, with and without findings), LAST-yaml-block extraction
// when prose and other fenced blocks precede the verdict, severity validation,
// and malformed inputs producing a clear error. This parser is the contract with
// the reviewer prompt templates (AIX-0008), so these cases pin that contract.
func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantErr      bool
		wantApproved bool
		wantFindings int
		// check, when set, runs extra assertions on a successful parse.
		check func(t *testing.T, v Verdict)
	}{
		{
			name: "approved no findings",
			in: "Looks good to me.\n\n" +
				"```yaml\napproved: true\nfindings: []\n```\n",
			wantApproved: true,
			wantFindings: 0,
		},
		{
			name: "not approved with findings",
			in: "I found problems.\n\n" +
				"```yaml\n" +
				"approved: false\n" +
				"findings:\n" +
				"  - severity: blocker\n" +
				"    file: internal/x.go\n" +
				"    line: 12\n" +
				"    message: \"nil deref\"\n" +
				"    suggestion: \"guard the pointer\"\n" +
				"  - severity: nit\n" +
				"    message: \"typo in comment\"\n" +
				"```\n",
			wantApproved: false,
			wantFindings: 2,
			check: func(t *testing.T, v Verdict) {
				if v.Findings[0].Severity != "blocker" || v.Findings[0].File != "internal/x.go" ||
					v.Findings[0].Line != 12 || v.Findings[0].Message != "nil deref" ||
					v.Findings[0].Suggestion != "guard the pointer" {
					t.Errorf("finding[0] mismatch: %+v", v.Findings[0])
				}
				if v.Findings[1].Severity != "nit" || v.Findings[1].File != "" || v.Findings[1].Line != 0 {
					t.Errorf("finding[1] should have optional fields empty: %+v", v.Findings[1])
				}
			},
		},
		{
			name: "last yaml block wins over a preceding example block",
			in: "Here is the schema for reference:\n\n" +
				"```yaml\napproved: true   # EXAMPLE, ignore me\nfindings: []\n```\n\n" +
				"Now my actual verdict:\n\n" +
				"```yaml\napproved: false\nfindings:\n  - severity: major\n    message: \"real issue\"\n```\n",
			wantApproved: false,
			wantFindings: 1,
			check: func(t *testing.T, v Verdict) {
				if v.Findings[0].Message != "real issue" {
					t.Errorf("did not take the LAST yaml block: %+v", v.Findings)
				}
			},
		},
		{
			name: "tolerates a preceding non-yaml fenced block",
			in: "```diff\n+ some code\n```\n\n" +
				"```yaml\napproved: true\nfindings: []\n```\n",
			wantApproved: true,
		},
		{
			name:         "uppercase YAML language tag is accepted",
			in:           "```YAML\napproved: true\nfindings: []\n```\n",
			wantApproved: true,
		},
		{
			name:    "no yaml block is an error",
			in:      "I think this is fine but I forgot the verdict block.",
			wantErr: true,
		},
		{
			name:    "unterminated yaml block is treated as not found",
			in:      "prose\n```yaml\napproved: true\nfindings: []\n",
			wantErr: true,
		},
		{
			name:    "invalid severity is rejected",
			in:      "```yaml\napproved: false\nfindings:\n  - severity: catastrophic\n    message: x\n```\n",
			wantErr: true,
		},
		{
			name:    "missing message is rejected",
			in:      "```yaml\napproved: false\nfindings:\n  - severity: major\n    file: a.go\n```\n",
			wantErr: true,
		},
		{
			name:    "garbage in the yaml block is an error",
			in:      "```yaml\n\tthis: : is not: valid: yaml\n```\n",
			wantErr: true,
		},
		{
			name:    "unknown keys in the verdict are rejected",
			in:      "```yaml\napproved: true\nfindings: []\nbogus: 1\n```\n",
			wantErr: true,
		},
		{
			name: "severity is normalized to lowercase",
			in:   "```yaml\napproved: false\nfindings:\n  - severity: BLOCKER\n    message: x\n```\n",
			check: func(t *testing.T, v Verdict) {
				if v.Findings[0].Severity != "blocker" {
					t.Errorf("severity not normalized: %q", v.Findings[0].Severity)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseVerdict(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got verdict %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Approved != tt.wantApproved {
				t.Errorf("approved = %v; want %v", v.Approved, tt.wantApproved)
			}
			if tt.wantFindings != 0 && len(v.Findings) != tt.wantFindings {
				t.Errorf("findings = %d; want %d", len(v.Findings), tt.wantFindings)
			}
			if tt.check != nil {
				tt.check(t, v)
			}
		})
	}
}

// TestParseVerdictErrorMentionsCause proves the no-block error is actionable
// (names that a yaml verdict block is required), so a misbehaving reviewer is
// debuggable.
func TestParseVerdictErrorMentionsCause(t *testing.T) {
	_, err := ParseVerdict("no block here")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should mention the missing yaml block; got: %v", err)
	}
}
