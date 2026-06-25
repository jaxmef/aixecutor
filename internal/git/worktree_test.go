package git

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeRefusedUnderReadOnly is acceptance criterion 5 (refusal half): the
// Worktree constructor refuses under read-only policy, so no manager — and thus
// no object capable of a mutating git command — is ever produced. We also prove
// NO git is attempted by injecting a runner that fails the test if called.
func TestWorktreeRefusedUnderReadOnly(t *testing.T) {
	g := newGatewayWithRunner("/repo", func(context.Context, string, ...string) ([]byte, []byte, error) {
		t.Fatal("no git may be run while the worktree gate refuses")
		return nil, nil, nil
	})

	for _, policy := range []string{PolicyReadOnly, "", "bogus"} {
		t.Run("policy="+policy, func(t *testing.T) {
			m, err := g.Worktree(policy)
			if err == nil {
				t.Fatalf("Worktree(%q) = nil error; want refusal", policy)
			}
			if m != nil {
				t.Fatalf("Worktree(%q) returned a non-nil manager despite refusal", policy)
			}
			if !strings.Contains(err.Error(), PolicyAllowWorktree) {
				t.Errorf("error %q should tell the user to set %q", err.Error(), PolicyAllowWorktree)
			}
		})
	}
}

// TestWorktreeAddRemoveCommandSurface is acceptance criterion 5 (allow half) and
// part of 6: under allow-worktree, Add runs `git worktree add <path>` and Remove
// runs `git worktree remove --force <path>`. The injected runner RECORDS args and
// never executes git, so we assert the exact command surface without creating a
// real worktree.
func TestWorktreeAddRemoveCommandSurface(t *testing.T) {
	rr := &recordingRunner{}
	g := newGatewayWithRunner("/home/user/repo", rr.run)

	m, err := g.Worktree(PolicyAllowWorktree)
	if err != nil {
		t.Fatalf("Worktree(allow-worktree): %v", err)
	}

	path, err := m.Add(context.Background(), "sub1")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Sibling of the repo root, never inside it or .git.
	wantPath := filepath.Join("/home/user", "repo-wt-sub1")
	if path != wantPath {
		t.Errorf("Add path = %q; want %q", path, wantPath)
	}
	if len(rr.calls) != 1 || callArgs(rr.calls[0]) != "worktree add "+wantPath {
		t.Fatalf("Add git call = %v; want [worktree add %s]", rr.calls, wantPath)
	}

	if err := m.Remove(context.Background(), path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if callArgs(rr.calls[1]) != "worktree remove --force "+wantPath {
		t.Errorf("Remove git call = %q; want %q", callArgs(rr.calls[1]), "worktree remove --force "+wantPath)
	}
	// After Remove, the manager no longer tracks it.
	if len(m.Created()) != 0 {
		t.Errorf("Created() = %v after Remove; want empty", m.Created())
	}
}

// TestWorktreeRemoveAllCleansUpEvenOnError is acceptance criterion 6: every
// worktree created in a run is cleaned up, INCLUDING when an intermediate removal
// errors. We add three worktrees, make the runner fail the remove of the middle
// one, and assert RemoveAll still attempts to remove ALL three and surfaces the
// error. Nothing is left tracked. No real worktrees are created.
func TestWorktreeRemoveAllCleansUpEvenOnError(t *testing.T) {
	failPath := filepath.Join("/r", "repo-wt-b")
	rr := &recordingRunner{
		failOn: func(args []string) bool {
			// Fail only the `worktree remove --force <failPath>` call.
			return len(args) >= 4 && args[0] == "worktree" && args[1] == "remove" && args[len(args)-1] == failPath
		},
		failErr: errors.New("simulated remove failure"),
	}
	g := newGatewayWithRunner("/r/repo", rr.run)

	m, err := g.Worktree(PolicyAllowWorktree)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := m.Add(context.Background(), name); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}

	err = m.RemoveAll(context.Background())
	if err == nil {
		t.Fatal("RemoveAll = nil; want the simulated mid-sequence error surfaced")
	}
	if !strings.Contains(err.Error(), "simulated remove failure") {
		t.Errorf("RemoveAll error = %q; want it to include the failure", err.Error())
	}

	// All three removes must have been ATTEMPTED despite the middle failure.
	var removeCalls []string
	for _, c := range rr.calls {
		if len(c) >= 2 && c[0] == "worktree" && c[1] == "remove" {
			removeCalls = append(removeCalls, c[len(c)-1])
		}
	}
	wantRemoved := map[string]bool{
		filepath.Join("/r", "repo-wt-a"): true,
		filepath.Join("/r", "repo-wt-b"): true,
		filepath.Join("/r", "repo-wt-c"): true,
	}
	if len(removeCalls) != 3 {
		t.Fatalf("attempted %d removes (%v); want 3 (all created worktrees, even after one fails)", len(removeCalls), removeCalls)
	}
	for _, p := range removeCalls {
		if !wantRemoved[p] {
			t.Errorf("unexpected remove of %q", p)
		}
	}
	// Tracking list is cleared even though one removal failed.
	if len(m.Created()) != 0 {
		t.Errorf("Created() = %v after RemoveAll; want empty", m.Created())
	}
}

// TestWorktreeAddRecordsPathBeforeRunForCleanup proves that a worktree whose
// `git worktree add` FAILS is still tracked for cleanup (the path is recorded
// before git runs), so a partial failure cannot leak an un-tracked worktree.
func TestWorktreeAddRecordsPathBeforeRunForCleanup(t *testing.T) {
	rr := &recordingRunner{
		failOn:  func(args []string) bool { return len(args) >= 2 && args[0] == "worktree" && args[1] == "add" },
		failErr: errors.New("add failed"),
	}
	g := newGatewayWithRunner("/r/repo", rr.run)
	m, err := g.Worktree(PolicyAllowWorktree)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := m.Add(context.Background(), "x"); err == nil {
		t.Fatal("Add should have failed")
	}
	created := m.Created()
	if len(created) != 1 || created[0] != filepath.Join("/r", "repo-wt-x") {
		t.Fatalf("failed Add must still track the path for cleanup; got %v", created)
	}
}

// TestWorktreeRemoveUnknownPathErrors ensures removing a path the manager never
// created is an error (guards against removing arbitrary directories).
func TestWorktreeRemoveUnknownPathErrors(t *testing.T) {
	rr := &recordingRunner{}
	g := newGatewayWithRunner("/r/repo", rr.run)
	m, _ := g.Worktree(PolicyAllowWorktree)
	if err := m.Remove(context.Background(), "/some/other/path"); err == nil {
		t.Fatal("Remove of an untracked path should error")
	}
	if len(rr.calls) != 0 {
		t.Errorf("no git should run for an untracked Remove; got %v", rr.calls)
	}
}

// TestWorktreeNameValidation rejects names that are not single safe segments.
func TestWorktreeNameValidation(t *testing.T) {
	rr := &recordingRunner{}
	g := newGatewayWithRunner("/r/repo", rr.run)
	m, _ := g.Worktree(PolicyAllowWorktree)
	for _, bad := range []string{"", ".", "..", "a/b", "../escape"} {
		if _, err := m.Add(context.Background(), bad); err == nil {
			t.Errorf("Add(%q) should be rejected", bad)
		}
	}
	if len(rr.calls) != 0 {
		t.Errorf("no git should run for invalid names; got %v", rr.calls)
	}
}
