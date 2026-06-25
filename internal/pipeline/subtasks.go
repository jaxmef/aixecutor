package pipeline

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jaxmef/aixecutor/internal/run"
	"gopkg.in/yaml.v3"
)

// subtasksDoc is the top-level shape of docs/subtasks.yaml as the planner emits
// it (CLAUDE.md §3.3 / the AIX-0009 schema). It is the parser-facing mirror of
// the planner template's documented schema: the two MUST stay in lockstep
// (internal/prompt/prompts/planner.tmpl). yaml.v3 in strict mode rejects unknown
// keys so a typo in the agent's output is a clear error rather than a silent drop.
type subtasksDoc struct {
	// Subtasks is the non-empty list of planned subtasks forming the DAG.
	Subtasks []subtaskSpec `yaml:"subtasks"`
}

// subtaskSpec is one planner-declared subtask in docs/subtasks.yaml. It carries
// exactly the fields the schema documents; ParseSubtasks maps it onto a
// run.Subtask (adding Status=pending). The acceptance criteria are a list in the
// YAML (concrete checkable items) but the run model stores them as a single
// string, so they are joined when mapped — see toRunSubtask.
type subtaskSpec struct {
	// ID is the unique, stable subtask id (e.g. "st-01").
	ID string `yaml:"id"`
	// Title is the short imperative title (required).
	Title string `yaml:"title"`
	// Description states what the subtask must accomplish (required).
	Description string `yaml:"description"`
	// Deps lists ids of subtasks that must finish first; must reference existing
	// ids and form no cycle.
	Deps []string `yaml:"deps"`
	// Files are the declared ownership globs driving non-overlapping parallelism.
	Files []string `yaml:"files"`
	// Acceptance lists the concrete, verifiable acceptance criteria.
	Acceptance []string `yaml:"acceptance"`
	// ManualTest is an optional note on manually verifying the subtask.
	ManualTest string `yaml:"manualTest"`
}

// ParseSubtasks decodes docs/subtasks.yaml, validates the DAG, and returns the
// subtasks as run.Subtask values with Status initialized to SubtaskPending. It is
// the single authority for turning the planner's machine-readable output into the
// run model; the planning phase persists the result into run.yaml.
//
// Decoding is strict (KnownFields): an unknown key in the agent's YAML is an
// error, so a misnamed field surfaces immediately rather than being silently
// dropped. After a structural decode, ValidateDAG enforces the semantic rules
// (non-empty, required fields, unique ids, existing deps, no cycles). Any error
// is wrapped with actionable context so the caller can show the agent what to fix.
func ParseSubtasks(data []byte) ([]run.Subtask, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("subtasks.yaml is empty (expected a top-level `subtasks:` list)")
	}

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var doc subtasksDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing subtasks.yaml: %w", err)
	}

	if err := ValidateDAG(doc.Subtasks); err != nil {
		return nil, err
	}

	out := make([]run.Subtask, 0, len(doc.Subtasks))
	for _, s := range doc.Subtasks {
		out = append(out, toRunSubtask(s))
	}
	return out, nil
}

// toRunSubtask maps a parsed subtaskSpec onto the run model's Subtask, seeding
// Status=pending and Loops=0. The acceptance list is joined into the run model's
// single Acceptance string (newline-bulleted) so resume and the subtask reviewer
// see the criteria without re-reading subtasks.yaml; an empty list yields an empty
// string (omitted in run.yaml).
func toRunSubtask(s subtaskSpec) run.Subtask {
	return run.Subtask{
		ID:          s.ID,
		Title:       s.Title,
		Description: s.Description,
		Acceptance:  joinAcceptance(s.Acceptance),
		Deps:        s.Deps,
		Files:       s.Files,
		Status:      run.SubtaskPending,
		Loops:       0,
	}
}

// joinAcceptance renders a list of acceptance items as a single string the run
// model can store: one "- item" per line. It returns "" for an empty list so the
// omitempty field stays absent in run.yaml.
func joinAcceptance(items []string) string {
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it) == "" {
			continue
		}
		cleaned = append(cleaned, "- "+strings.TrimSpace(it))
	}
	return strings.Join(cleaned, "\n")
}

// ValidateDAG enforces the semantic rules on a parsed subtask list (CLAUDE.md
// §3.3 / the AIX-0009 schema):
//
//   - the list is non-empty;
//   - every subtask has a non-empty id, title, and description;
//   - ids are unique;
//   - every dep references an id that exists in the list (no dangling deps), and
//     a subtask does not depend on itself;
//   - the dependency graph has NO cycles (it is a DAG).
//
// Cycle detection is a depth-first search with a three-color marking (white =
// unvisited, gray = on the current DFS stack, black = fully explored): finding a
// gray node on a dep edge means a back edge, i.e. a cycle, and the error names the
// cycle path (e.g. "st-01 -> st-02 -> st-01") so the planner can fix it.
//
// It is exported and standalone so tests can validate fixtures directly and so a
// future caller can re-validate a run's subtasks without re-parsing YAML.
func ValidateDAG(subtasks []subtaskSpec) error {
	if len(subtasks) == 0 {
		return errors.New("subtasks.yaml: the `subtasks` list is empty (the planner must produce at least one subtask)")
	}

	// Index by id, checking required fields and uniqueness in one pass.
	byID := make(map[string]subtaskSpec, len(subtasks))
	for i, s := range subtasks {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("subtasks.yaml: subtask #%d has an empty id", i+1)
		}
		if strings.TrimSpace(s.Title) == "" {
			return fmt.Errorf("subtasks.yaml: subtask %q has an empty title", s.ID)
		}
		if strings.TrimSpace(s.Description) == "" {
			return fmt.Errorf("subtasks.yaml: subtask %q has an empty description", s.ID)
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("subtasks.yaml: duplicate subtask id %q (ids must be unique)", s.ID)
		}
		byID[s.ID] = s
	}

	// Validate dep references (existing + no self-dep) before cycle detection so a
	// dangling dep gives a precise error rather than surfacing as a traversal miss.
	for _, s := range subtasks {
		for _, dep := range s.Deps {
			if dep == s.ID {
				return fmt.Errorf("subtasks.yaml: subtask %q lists itself in deps", s.ID)
			}
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("subtasks.yaml: subtask %q depends on unknown id %q", s.ID, dep)
			}
		}
	}

	return detectCycle(subtasks, byID)
}

// dfsColor marks a node's state during cycle detection.
type dfsColor int

const (
	colorWhite dfsColor = iota // unvisited
	colorGray                  // on the current DFS stack
	colorBlack                 // fully explored
)

// detectCycle runs the three-color DFS over the dependency graph and returns a
// cycle error (naming the path) if a back edge is found, or nil for a DAG. Nodes
// are visited in list order for a deterministic error across runs.
func detectCycle(subtasks []subtaskSpec, byID map[string]subtaskSpec) error {
	color := make(map[string]dfsColor, len(subtasks))
	// path tracks the current DFS stack so a detected cycle can be reported as a
	// readable chain of ids.
	var path []string

	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case colorGray:
			// Back edge: id is already on the stack — report the cycle slice from
			// the first occurrence of id to the end, then back to id.
			return fmt.Errorf("subtasks.yaml: dependency cycle detected: %s", formatCycle(path, id))
		case colorBlack:
			return nil
		}
		color[id] = colorGray
		path = append(path, id)
		for _, dep := range byID[id].Deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		color[id] = colorBlack
		return nil
	}

	for _, s := range subtasks {
		if color[s.ID] == colorWhite {
			if err := visit(s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// formatCycle renders the cycle as "a -> b -> c -> a": the slice of path from the
// first occurrence of the repeated id onward, with the repeated id appended to
// close the loop.
func formatCycle(path []string, repeated string) string {
	start := 0
	for i, id := range path {
		if id == repeated {
			start = i
			break
		}
	}
	loop := append([]string{}, path[start:]...)
	loop = append(loop, repeated)
	return strings.Join(loop, " -> ")
}
