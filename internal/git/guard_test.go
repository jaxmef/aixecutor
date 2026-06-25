package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyGitPackageInvokesGit is acceptance criterion 7 — the architectural
// guard for invariant #1. It walks all non-test Go source under internal/ and
// asserts that the ONLY package that invokes the git binary via os/exec is
// internal/git. Any exec.Command / exec.CommandContext whose command resolves to
// "git" outside this package means some code bypassed the gateway, which would
// break the single-chokepoint guarantee (and could run a mutating git command).
//
// We parse the AST (not a naive grep) so that doc comments, string mentions of
// "git" in messages, or the word git in identifiers do not cause false
// positives: we key strictly on call expressions to exec.Command(Context) whose
// first argument is the string literal "git" (or a const that obviously is).
func TestOnlyGitPackageInvokesGit(t *testing.T) {
	root := repoInternalDir(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkgDir := filepath.Dir(path)
		// internal/git is the sanctioned home of git invocations.
		if filepath.Base(pkgDir) == "git" && filepath.Base(filepath.Dir(pkgDir)) == "internal" {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		if execName := execAlias(file); execName != "" {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isExecCommandCall(call, execName) {
					return true
				}
				if callInvokesGit(call) {
					pos := fset.Position(call.Pos())
					offenders = append(offenders, pos.String())
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("git is invoked via os/exec outside internal/git (all git access must go through the gateway):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestGuardCatchesAGitExec is a meta-test: it feeds the detector a synthetic
// source file that shells out to git and asserts the detector flags it. This
// proves the guard above would actually catch a bypass, rather than silently
// passing because the detection logic is broken.
func TestGuardCatchesAGitExec(t *testing.T) {
	src := `package bad
import "os/exec"
func f() { _ = exec.Command("git", "commit", "-m", "x") }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bad.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	alias := execAlias(file)
	if alias == "" {
		t.Fatal("execAlias did not detect the os/exec import")
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isExecCommandCall(call, alias) && callInvokesGit(call) {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("guard detector failed to flag a git exec.Command call")
	}

	// And the negative: a non-git exec call must NOT be flagged.
	srcOK := `package ok
import "os/exec"
func f() { _ = exec.Command("ls", "-la") }
`
	fileOK, _ := parser.ParseFile(token.NewFileSet(), "ok.go", srcOK, 0)
	flagged := false
	ast.Inspect(fileOK, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isExecCommandCall(call, execAlias(fileOK)) && callInvokesGit(call) {
			flagged = true
		}
		return true
	})
	if flagged {
		t.Fatal("guard detector incorrectly flagged a non-git exec call")
	}
}

// repoInternalDir returns the absolute path to the repo's internal/ directory by
// walking up from this test file's package until it finds go.mod.
func repoInternalDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return filepath.Join(dir, "internal")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

// execAlias returns the local name under which a file imports "os/exec" (usually
// "exec"), or "" if the file does not import it. Honors import aliases.
func execAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "os/exec" {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				// Blank/dot imports cannot be used as `exec.Command`; treat as
				// not-an-exec-call source for our purposes.
				return ""
			}
			return imp.Name.Name
		}
		return "exec"
	}
	return ""
}

// isExecCommandCall reports whether call is `<alias>.Command(...)` or
// `<alias>.CommandContext(...)`.
func isExecCommandCall(call *ast.CallExpr, alias string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != alias {
		return false
	}
	return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
}

// callInvokesGit reports whether an exec.Command(Context) call's command argument
// is the literal "git". For Command the command is arg 0; for CommandContext it
// is arg 1 (arg 0 is the context). We conservatively also flag a literal whose
// basename is "git" (e.g. "/usr/bin/git") so a fully-qualified path cannot slip
// past. Non-literal command args (a variable) are NOT flagged here — the only
// such code is in internal/git, which this guard already exempts; flagging
// arbitrary variables would produce false positives without catching a real
// bypass.
func callInvokesGit(call *ast.CallExpr) bool {
	sel := call.Fun.(*ast.SelectorExpr) // safe: caller checked isExecCommandCall
	var cmdArgIdx int
	switch sel.Sel.Name {
	case "Command":
		cmdArgIdx = 0
	case "CommandContext":
		cmdArgIdx = 1
	default:
		return false
	}
	if len(call.Args) <= cmdArgIdx {
		return false
	}
	lit, ok := call.Args[cmdArgIdx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	cmd := strings.Trim(lit.Value, "`\"")
	return cmd == "git" || filepath.Base(cmd) == "git"
}
