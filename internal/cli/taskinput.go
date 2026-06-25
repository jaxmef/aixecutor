package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// maxTaskFileSize caps how much of a --task-file (or @file), piped stdin, or an
// edited task we ingest. The whole input becomes prompt text fed to the planner,
// so an accidentally huge source (a log, a binary, the wrong path) should fail
// fast with a clear error rather than balloon the prompt. 1 MiB is far larger than
// any real task spec yet small enough to reject obvious mistakes.
const maxTaskFileSize = 1 << 20 // 1 MiB

// editorSeed pre-fills the temp file opened in the interactive editor with a
// commented, git-commit-style header. Lines starting with '#' are stripped after
// the editor exits, so this guidance never becomes part of the task.
const editorSeed = "# Write the task below. Lines starting with '#' are ignored\n" +
	"# (including Markdown '# headings' — indent or rephrase them to keep them).\n" +
	"# Save and close to submit; an empty task aborts.\n"

// errTaskAborted signals a clean, user-initiated abort with no task (an empty
// editor buffer). The commands treat it as success: print the message, create no
// run, exit zero. It is distinct from a usage error so callers can branch on it.
var errTaskAborted = errors.New("aborted: empty task, no run created")

// taskInputEnv carries the interactive-input seams so resolveTaskInput stays
// hermetically testable: stdin + its TTY-ness decide between the piped-read and
// editor paths, and launchEditor is injected so tests simulate "the editor wrote
// X" without spawning a real editor or needing a terminal.
type taskInputEnv struct {
	stdin io.Reader
	// stdinIsTTY reports whether stdin is an interactive terminal. False for a
	// pipe/redirect/CI (→ read stdin) or absent stdin; true → open the editor.
	stdinIsTTY bool
	// launchEditor seeds a temp file with initial, opens the user's editor on it,
	// and returns the file's final contents. Nil disables the editor path.
	launchEditor func(initial string) (string, error)
}

// resolveTaskInput resolves the task string for `run`/`plan` from the positional
// args and the --task-file flag, with this precedence (AIX-0017 + AIX-0019):
//  1. a positional task string (args[0]); a leading '@' reads from a file
//     (`run @spec.md`), and a literal leading '@' is escaped by doubling (`@@text`).
//  2. the --task-file flag (an explicit, unambiguous path).
//  3. piped/redirected stdin (non-TTY): read all of stdin as the task.
//  4. interactive editor (stdin is a TTY): open $VISUAL/$EDITOR on a seeded temp
//     file; the saved, comment-stripped content becomes the task.
//
// A positional task and --task-file are mutually exclusive (both → usage error).
// When no source is present and there is no way to prompt (non-TTY with empty
// stdin, e.g. CI), it fails fast rather than blocking. File/stdin/editor content
// is trimmed of trailing whitespace and rejected if blank or over maxTaskFileSize.
// Error classes map to exitUsage (AIX-0014); an empty editor returns errTaskAborted.
func resolveTaskInput(args []string, taskFile string, env taskInputEnv) (string, error) {
	hasPositional := len(args) == 1 && args[0] != ""

	switch {
	case hasPositional && taskFile != "":
		return "", withExit(exitUsage, fmt.Errorf(
			"provide the task either as an argument or via --task-file, not both"))
	case taskFile != "":
		return readTaskFile(taskFile)
	case hasPositional:
		return resolvePositionalTask(args[0])
	default:
		return resolveInteractiveTask(env)
	}
}

// resolvePositionalTask interprets a positional argument: an unescaped leading
// '@' makes the remainder a file path (the @file shorthand), a doubled leading
// '@@' is the escape for a literal task that starts with '@', and anything else
// is the task verbatim (unchanged inline-string behavior).
func resolvePositionalTask(arg string) (string, error) {
	switch {
	case strings.HasPrefix(arg, "@@"):
		return arg[1:], nil
	case strings.HasPrefix(arg, "@"):
		return readTaskFile(arg[1:])
	default:
		return arg, nil
	}
}

// resolveInteractiveTask obtains the task when no positional/flag source was given:
// from piped stdin (non-TTY) or the editor (TTY). With neither available it fails
// fast with an actionable error — it never blocks waiting on input.
func resolveInteractiveTask(env taskInputEnv) (string, error) {
	// A TTY stdin means the user is at a terminal: open their editor. (Reading a
	// TTY stdin would block; the editor is the interactive path.)
	if env.stdinIsTTY {
		return resolveEditorTask(env)
	}

	// Non-TTY: stdin is a pipe/redirect (or /dev/null in CI). Reading never blocks —
	// a pipe yields its content then EOF, an empty/closed stdin yields EOF at once.
	if env.stdin != nil {
		data, err := readCapped(env.stdin)
		if err != nil {
			return "", withExit(exitUsage, fmt.Errorf("cannot read task from stdin: %w", err))
		}
		task := strings.TrimRight(string(data), " \t\r\n")
		if strings.TrimSpace(task) != "" {
			return task, nil
		}
	}

	// No source and no way to prompt → fail fast (never hang on input).
	return "", withExit(exitUsage, fmt.Errorf(
		"no task provided: pass the task as an argument, with --task-file <path>, "+
			"as @<path>, via stdin, or run interactively to open an editor"))
}

// resolveEditorTask opens the injected editor on a seeded temp file, strips the
// comment lines from the result, and returns the task. An empty result (blank or
// all-comments) is a clean abort (errTaskAborted), not a failure.
func resolveEditorTask(env taskInputEnv) (string, error) {
	if env.launchEditor == nil {
		return "", withExit(exitUsage, fmt.Errorf(
			"no task provided and no editor available; set $EDITOR or pass a task argument"))
	}
	raw, err := env.launchEditor(editorSeed)
	if err != nil {
		return "", withExit(exitUsage, fmt.Errorf("could not open an editor for the task: %w", err))
	}
	task := stripCommentLines(raw)
	if len(task) > maxTaskFileSize {
		return "", withExit(exitUsage, fmt.Errorf(
			"task is too large (%d bytes; limit %d bytes)", len(task), maxTaskFileSize))
	}
	if task == "" {
		return "", errTaskAborted
	}
	return task, nil
}

// stripCommentLines drops whole lines whose first non-whitespace char is '#'
// (the seeded guidance), then trims surrounding whitespace while preserving the
// task's internal lines exactly.
func stripCommentLines(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// readTaskFile reads path as the task: it guards the size before reading, trims
// trailing whitespace, and rejects a missing/unreadable path or blank content —
// each with an actionable, exitUsage-classified error.
func readTaskFile(path string) (string, error) {
	if path == "" {
		return "", withExit(exitUsage, fmt.Errorf("task file path is empty"))
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", withExit(exitUsage, fmt.Errorf("cannot read task file %q: %w", path, err))
	}
	if info.IsDir() {
		return "", withExit(exitUsage, fmt.Errorf("task file %q is a directory, not a file", path))
	}
	if info.Size() > maxTaskFileSize {
		return "", withExit(exitUsage, fmt.Errorf(
			"task file %q is too large (%d bytes; limit %d bytes)", path, info.Size(), maxTaskFileSize))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", withExit(exitUsage, fmt.Errorf("cannot read task file %q: %w", path, err))
	}

	task := strings.TrimRight(string(data), " \t\r\n")
	if strings.TrimSpace(task) == "" {
		return "", withExit(exitUsage, fmt.Errorf("task file %q is empty", path))
	}
	return task, nil
}

// readCapped reads up to maxTaskFileSize bytes from r and errors if more remains,
// so a giant or unbounded stdin is rejected instead of buffered without limit.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxTaskFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTaskFileSize {
		return nil, fmt.Errorf("input is too large (limit %d bytes)", maxTaskFileSize)
	}
	return data, nil
}

// resolveTaskForCommand resolves the task for a cobra command, wiring the real
// interactive seams (the command's stdin + its TTY-ness, the $VISUAL/$EDITOR
// launcher) and mapping a clean editor abort onto (("", true, nil)) so the command
// exits zero with a printed message and creates no run. Stdin comes from the cobra
// command (c.InOrStdin(), os.Stdin by default) so tests can inject it without
// touching the process's real stdin.
func resolveTaskForCommand(c *cobra.Command, args []string, taskFile string) (task string, aborted bool, err error) {
	in := c.InOrStdin()
	env := taskInputEnv{
		stdin:        in,
		stdinIsTTY:   isTTYReader(in),
		launchEditor: launchEditor,
	}
	task, err = resolveTaskInput(args, taskFile, env)
	if errors.Is(err, errTaskAborted) {
		c.Println(err.Error())
		return "", true, nil
	}
	return task, false, err
}

// isTTYReader reports whether r is an interactive terminal — true only for an
// *os.File that is a character device. A pipe, a redirect, or an injected reader
// (a bytes/strings reader in tests) is never a TTY, so the non-blocking stdin-read
// path is taken instead of the editor.
func isTTYReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// launchEditor seeds a temp file with initial, opens $VISUAL → $EDITOR → vi on it
// (via `sh -c` so an editor command with flags like `code --wait` works), wired to
// the real terminal, then returns the file's final contents. The temp file is
// always cleaned up.
func launchEditor(initial string) (string, error) {
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")

	f, err := os.CreateTemp("", "aixecutor-task-*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	// Pass the path as a positional arg ($1) rather than interpolating it into the
	// script, so the shell never re-parses it — `$`, backticks, and spaces in the
	// temp path are inert. The editor word itself is left unquoted so `EDITOR="code
	// --wait"` (command + flags) still splits as the user intends.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor %q exited with error: %w", editor, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
