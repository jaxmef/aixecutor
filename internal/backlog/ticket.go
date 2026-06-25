// Package backlog implements the driver loop (AIX-0018): it discovers ticket
// files, resolves their dependency DAG, and runs the next ready ticket through the
// pipeline, gating advancement on the run's structured outcome. It is deliberately
// decoupled from internal/pipeline and internal/run: the orchestration is injected
// as a TicketRunner returning a plain Outcome, so the whole package is testable
// without real agents, git, or the pipeline.
package backlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TicketStatus is the author-declared lifecycle in a ticket's frontmatter. Only a
// small, generic set is recognized; an unrecognized value is treated as Pending.
type TicketStatus string

const (
	// StatusPending marks work still to do (the default when status is absent).
	StatusPending TicketStatus = "pending"
	// StatusDone marks an already-satisfied ticket: it is never run, and it
	// satisfies dependents' dependencies.
	StatusDone TicketStatus = "done"
	// StatusBlocked marks a ticket the author has parked: it is never selected.
	StatusBlocked TicketStatus = "blocked"
)

// Ticket is one backlog item parsed from a Markdown file with YAML frontmatter.
// The frontmatter carries the machine-readable id/deps/status; the body (after the
// frontmatter) is the task fed verbatim to the pipeline.
type Ticket struct {
	ID        string
	DependsOn []string
	Status    TicketStatus
	Task      string
	Path      string
}

// frontmatter is the YAML schema at the top of each ticket file. Only these keys
// are read; the rest of the file is the task body.
type frontmatter struct {
	ID        string   `yaml:"id"`
	DependsOn []string `yaml:"dependsOn"`
	Status    string   `yaml:"status"`
}

// errNoFrontmatter is returned when a candidate file lacks the leading `---`
// frontmatter block. Discover surfaces it with the offending path.
var errNoFrontmatter = errors.New("missing YAML frontmatter (a `---` block with at least an id)")

// ParseTicket parses a single ticket from its file contents. It requires a leading
// YAML frontmatter block declaring at least an id; dependsOn defaults to none and
// status to pending. The body after the frontmatter is trimmed and used as the
// task, and must be non-empty.
func ParseTicket(path string, data []byte) (Ticket, error) {
	fmText, body, err := splitFrontmatter(string(data))
	if err != nil {
		return Ticket{}, fmt.Errorf("%s: %w", path, err)
	}

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return Ticket{}, fmt.Errorf("%s: invalid frontmatter: %w", path, err)
	}

	id := strings.TrimSpace(fm.ID)
	if id == "" {
		return Ticket{}, fmt.Errorf("%s: frontmatter is missing required field 'id'", path)
	}

	task := strings.TrimSpace(body)
	if task == "" {
		return Ticket{}, fmt.Errorf("%s: ticket %q has no task body below the frontmatter", path, id)
	}

	status := TicketStatus(strings.TrimSpace(fm.Status))
	if status == "" {
		status = StatusPending
	}

	return Ticket{
		ID:        id,
		DependsOn: trimAll(fm.DependsOn),
		Status:    status,
		Task:      task,
		Path:      path,
	}, nil
}

// Discover reads every `*.md` file directly under dir, parses each as a ticket,
// and returns them sorted by id. It errors on an unreadable dir, a malformed
// ticket, or a duplicate id (which would make selection ambiguous).
func Discover(dir string) ([]Ticket, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read backlog directory %q: %w", dir, err)
	}

	var tickets []Ticket
	seen := make(map[string]string) // id -> path, for duplicate detection
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read ticket %q: %w", path, err)
		}
		t, err := ParseTicket(path, data)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[t.ID]; dup {
			return nil, fmt.Errorf("duplicate ticket id %q in %q and %q", t.ID, prev, path)
		}
		seen[t.ID] = path
		tickets = append(tickets, t)
	}

	if len(tickets) == 0 {
		return nil, fmt.Errorf("no ticket files (*.md with frontmatter) found in %q", dir)
	}

	sort.Slice(tickets, func(i, j int) bool { return tickets[i].ID < tickets[j].ID })
	return tickets, nil
}

// splitFrontmatter separates a leading `---`-delimited YAML block from the body.
// It returns the frontmatter text (between the delimiters) and the remaining body.
func splitFrontmatter(s string) (fmText, body string, err error) {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", "", errNoFrontmatter
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", "", errors.New("frontmatter `---` block is not closed")
	}
	return strings.Join(lines[1:end], "\n"), strings.Join(lines[end+1:], "\n"), nil
}

// trimAll trims whitespace from each element and drops empties, normalizing a
// dependsOn list that may carry stray spaces.
func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
