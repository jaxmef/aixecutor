package backlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTicket(t *testing.T) {
	data := "---\nid: AIX-0002\ndependsOn:\n  - AIX-0001\nstatus: pending\n---\n\n# Title\n\nBuild the thing.\n"
	tk, err := ParseTicket("AIX-0002.md", []byte(data))
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if tk.ID != "AIX-0002" {
		t.Errorf("id = %q", tk.ID)
	}
	if len(tk.DependsOn) != 1 || tk.DependsOn[0] != "AIX-0001" {
		t.Errorf("dependsOn = %v", tk.DependsOn)
	}
	if tk.Status != StatusPending {
		t.Errorf("status = %q", tk.Status)
	}
	if tk.Task != "# Title\n\nBuild the thing." {
		t.Errorf("task body = %q", tk.Task)
	}
}

func TestParseTicketDefaults(t *testing.T) {
	// No dependsOn, no status → defaults: no deps, pending.
	tk, err := ParseTicket("x.md", []byte("---\nid: T1\n---\nDo it.\n"))
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if len(tk.DependsOn) != 0 {
		t.Errorf("dependsOn = %v, want none", tk.DependsOn)
	}
	if tk.Status != StatusPending {
		t.Errorf("status = %q, want pending", tk.Status)
	}
}

func TestParseTicketErrors(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "# just markdown\n",
		"unclosed":       "---\nid: T1\n",
		"missing id":     "---\nstatus: pending\n---\nbody\n",
		"empty body":     "---\nid: T1\n---\n\n  \n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTicket("t.md", []byte(data)); err == nil {
				t.Errorf("expected an error for %q", name)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.md", "---\nid: B\n---\nsecond\n")
	write("a.md", "---\nid: A\n---\nfirst\n")
	write("notes.txt", "ignored, not markdown")

	tickets, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("found %d tickets, want 2", len(tickets))
	}
	// Sorted by id: A before B.
	if tickets[0].ID != "A" || tickets[1].ID != "B" {
		t.Errorf("order = %q,%q want A,B", tickets[0].ID, tickets[1].ID)
	}
}

func TestDiscoverRejectsDuplicateID(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"one.md", "two.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("---\nid: DUP\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Discover(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected a duplicate-id error, got %v", err)
	}
}

func TestDiscoverEmptyDir(t *testing.T) {
	_, err := Discover(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no ticket files") {
		t.Errorf("expected a no-tickets error, got %v", err)
	}
}
