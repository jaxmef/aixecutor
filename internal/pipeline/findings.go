package pipeline

import (
	"fmt"
	"strings"

	"github.com/jaxmef/aixecutor/internal/prompt"
	"github.com/jaxmef/aixecutor/internal/run"
	"gopkg.in/yaml.v3"
)

// This file holds the shared findings types and the reviewer-verdict parser used
// by BOTH review loops: the per-subtask loop (AIX-0011, review_subtask.go) and
// the senior-review loop (AIX-0012). Keeping them here, in one place, means the
// findings contract with the reviewer prompts
// (internal/prompt/prompts/{subtask,senior}-reviewer.tmpl) has a single canonical
// parser; the prompt templates and ParseVerdict MUST stay in lockstep.
//
// pipeline.Finding is the canonical in-pipeline shape. prompt.Finding is the leaf
// render type (so the prompt package carries no pipeline import, CLAUDE.md §3.1)
// and run.Finding is the persisted shape (so run.yaml is independent of the
// pipeline); the small mappers below convert between them.

// Finding is one issue raised by a reviewer, parsed from the machine-readable
// verdict block the reviewer prompts emit. It is the canonical pipeline shape; it
// is mapped onto prompt.Finding when injected into a worker prompt and onto
// run.Finding when persisted (carried forward to senior review).
type Finding struct {
	// Severity is one of: blocker, major, minor, nit (validated by ParseVerdict).
	Severity string `yaml:"severity"`
	// File is the path the finding concerns, relative to the repo root. Optional.
	File string `yaml:"file"`
	// Line is the line number in the new file, or 0 when not applicable. Optional.
	Line int `yaml:"line"`
	// Message states the problem concretely. Required.
	Message string `yaml:"message"`
	// Suggestion is an optional concrete remedy.
	Suggestion string `yaml:"suggestion"`
}

// Verdict is a reviewer's structured decision: whether the change is acceptable
// and, if not, the concrete findings to address. It is the parsed form of the
// trailing YAML block in the reviewer's response (the contract enforced by the
// reviewer prompt templates).
type Verdict struct {
	// Approved is true when the reviewer accepts the change as-is.
	Approved bool `yaml:"approved"`
	// Findings are the concrete issues; empty (or nil) when there are none.
	Findings []Finding `yaml:"findings"`
}

// validSeverities is the closed set of finding severities the reviewer prompts
// document; ParseVerdict rejects anything else so a typo or hallucinated severity
// is a clear parse error rather than a silently-accepted value.
var validSeverities = map[string]bool{
	"blocker": true,
	"major":   true,
	"minor":   true,
	"nit":     true,
}

// ParseVerdict extracts and parses the reviewer's machine-readable verdict from
// its free-form response. The contract (AIX-0008, mirrored in the reviewer prompt
// templates) is: the reviewer may write any prose and even other fenced code
// blocks first, then ends with EXACTLY ONE ```yaml block holding the verdict, as
// the LAST fenced block in the response. ParseVerdict therefore locates the LAST
// ```yaml block (tolerating prose and other fences before it), YAML-unmarshals it
// into a Verdict, and validates it:
//
//   - every finding's severity must be one of blocker|major|minor|nit;
//   - every finding must have a non-empty message;
//   - file, line, and suggestion are optional.
//
// A missing yaml block, a block that does not parse as the verdict shape, or a
// finding that fails validation yields a clear, actionable error (so the caller
// can decide whether to re-ask the reviewer or fail). It never panics.
func ParseVerdict(agentText string) (Verdict, error) {
	block, ok := lastYAMLBlock(agentText)
	if !ok {
		return Verdict{}, fmt.Errorf(
			"parsing reviewer verdict: no ```yaml block found in the reviewer output " +
				"(the reviewer must end its response with a single yaml verdict block)")
	}

	var v Verdict
	// KnownFields keeps an obviously-malformed block (e.g. wrong keys) from
	// silently decoding to an empty verdict, which would otherwise read as
	// "approved: false, no findings" and mislead the loop.
	dec := yaml.NewDecoder(strings.NewReader(block))
	dec.KnownFields(true)
	if err := dec.Decode(&v); err != nil {
		return Verdict{}, fmt.Errorf("parsing reviewer verdict: the yaml block did not parse: %w", err)
	}

	for i := range v.Findings {
		f := v.Findings[i]
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if !validSeverities[sev] {
			return Verdict{}, fmt.Errorf(
				"parsing reviewer verdict: finding %d has invalid severity %q (want one of blocker|major|minor|nit)",
				i+1, f.Severity)
		}
		v.Findings[i].Severity = sev
		if strings.TrimSpace(f.Message) == "" {
			return Verdict{}, fmt.Errorf("parsing reviewer verdict: finding %d is missing a message", i+1)
		}
	}
	return v, nil
}

// lastYAMLBlock returns the contents of the LAST ```yaml fenced code block in s
// (without the fences) and whether one was found. Scanning for the LAST block —
// rather than the first — is the deliberate contract: the reviewer may emit other
// fenced blocks (e.g. a ```diff or an illustrative ```yaml example) in its
// reasoning, and the binding verdict is always the final yaml block.
//
// The opening fence is matched permissively (```yaml or ```YAML, optional
// surrounding spaces, on its own line); the block runs to the next line that is a
// closing ``` fence. A yaml block opened but never closed (no terminating fence)
// is treated as not found, so a truncated response is a clear miss rather than a
// half-read block.
func lastYAMLBlock(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	var (
		body      []string
		bestBody  []string
		bestFound bool
		inBlock   bool
	)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if isYAMLFenceOpen(trimmed) {
				inBlock = true
				body = nil
			}
			continue
		}
		// Inside a yaml block: a bare ``` closes it.
		if trimmed == "```" {
			inBlock = false
			// Keep this as the best (latest) COMPLETE block seen so far; an
			// unterminated final block is intentionally ignored.
			bestBody = body
			bestFound = true
			continue
		}
		body = append(body, line)
	}
	if !bestFound {
		return "", false
	}
	return strings.Join(bestBody, "\n"), true
}

// isYAMLFenceOpen reports whether a trimmed line opens a yaml fenced block:
// "```yaml" (case-insensitive on the language tag), allowing no extra text after
// the tag so a line like "```yaml example" still opens a block while "```yamlish"
// does not.
func isYAMLFenceOpen(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "```") {
		return false
	}
	tag := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
	// The info string's first token is the language; accept "yaml" / "yml".
	lang := tag
	if i := strings.IndexAny(tag, " \t"); i >= 0 {
		lang = tag[:i]
	}
	lang = strings.ToLower(lang)
	return lang == "yaml" || lang == "yml"
}

// toPrompt maps a pipeline Finding onto the leaf prompt.Finding render type, so a
// finding can be injected into ExecutorContext.PriorFindings (remediation) or
// SeniorReviewerContext.CarriedFindings (AIX-0012) without the prompt package
// importing the pipeline.
func (f Finding) toPrompt() prompt.Finding {
	return prompt.Finding{
		Severity:   f.Severity,
		File:       f.File,
		Line:       f.Line,
		Message:    f.Message,
		Suggestion: f.Suggestion,
	}
}

// toRun maps a pipeline Finding onto the persisted run.Finding shape, used when
// carrying unresolved findings forward onto the run model (Subtask.Unresolved).
func (f Finding) toRun() run.Finding {
	return run.Finding{
		Severity:   f.Severity,
		File:       f.File,
		Line:       f.Line,
		Message:    f.Message,
		Suggestion: f.Suggestion,
	}
}

// toPromptFindings maps a slice of pipeline findings to prompt findings (the
// shape worker prompt contexts expect). Returns nil for an empty input so the
// "no findings" case renders as the first-attempt prompt, not a remediation one.
func toPromptFindings(fs []Finding) []prompt.Finding {
	if len(fs) == 0 {
		return nil
	}
	out := make([]prompt.Finding, len(fs))
	for i, f := range fs {
		out[i] = f.toPrompt()
	}
	return out
}

// toRunFindings maps a slice of pipeline findings to the persisted run shape.
// Returns nil for an empty input so a clean subtask records no Unresolved block.
func toRunFindings(fs []Finding) []run.Finding {
	if len(fs) == 0 {
		return nil
	}
	out := make([]run.Finding, len(fs))
	for i, f := range fs {
		out[i] = f.toRun()
	}
	return out
}
