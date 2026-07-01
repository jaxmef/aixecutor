package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerbosityGating proves the CONSOLE handler is quiet by default: at Normal it
// suppresses both Debug and Info (only human progress, not raw slog, reaches the
// console), and -v/--verbose restores them. Quiet suppresses both too. The file
// handler's own (looser) level mapping is covered by
// TestAttachRunFileWritesStructuredLog.
func TestVerbosityGating(t *testing.T) {
	cases := []struct {
		name      string
		v         Verbosity
		wantDebug bool
		wantInfo  bool
	}{
		{"normal", Normal, false, false},
		{"verbose", Verbose, true, true},
		{"quiet", Quiet, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := New(tc.v, &buf)
			l.Debug("a debug line", "k", "v")
			l.Info("an info line", "k", "v")
			out := buf.String()
			if got := strings.Contains(out, "a debug line"); got != tc.wantDebug {
				t.Errorf("debug present = %v, want %v\n%s", got, tc.wantDebug, out)
			}
			if got := strings.Contains(out, "an info line"); got != tc.wantInfo {
				t.Errorf("info present = %v, want %v\n%s", got, tc.wantInfo, out)
			}
		})
	}
}

// TestAttachRunFileWritesStructuredLog proves the console and file handlers run at
// independent levels. At Normal, an Info line still lands in the run file but is
// gated off the console (only Warn+ reaches the console by default); -v re-enables
// console structured logs while the file behaviour is unchanged. The Debug-level
// file mapping is checked separately in TestAttachRunFileFileLevelMapping.
func TestAttachRunFileWritesStructuredLog(t *testing.T) {
	cases := []struct {
		name        string
		v           Verbosity
		wantConsole bool
	}{
		{"normal suppresses console info", Normal, false},
		{"verbose restores console info", Verbose, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var console bytes.Buffer
			l := New(tc.v, &console)
			logsDir := t.TempDir()
			if err := l.AttachRunFile(logsDir); err != nil {
				t.Fatalf("AttachRunFile: %v", err)
			}
			l.Info("hello run", "id", "run-1")
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(logsDir, "aixecutor.log"))
			if err != nil {
				t.Fatalf("reading run log: %v", err)
			}
			if !strings.Contains(string(data), "hello run") || !strings.Contains(string(data), "id=run-1") {
				t.Errorf("run log missing the structured line:\n%s", data)
			}
			if got := strings.Contains(console.String(), "hello run"); got != tc.wantConsole {
				t.Errorf("console info present = %v, want %v\n%s", got, tc.wantConsole, console.String())
			}
		})
	}
}

// TestAttachRunFileFileLevelMapping proves the FILE handler keeps the full
// verbosity.level() mapping regardless of the quieter console gating: Normal keeps
// Info but drops Debug; Verbose keeps Debug.
func TestAttachRunFileFileLevelMapping(t *testing.T) {
	cases := []struct {
		name      string
		v         Verbosity
		wantDebug bool
		wantInfo  bool
	}{
		{"normal", Normal, false, true},
		{"verbose", Verbose, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(tc.v, &bytes.Buffer{})
			logsDir := t.TempDir()
			if err := l.AttachRunFile(logsDir); err != nil {
				t.Fatalf("AttachRunFile: %v", err)
			}
			l.Debug("a debug line", "k", "v")
			l.Info("an info line", "k", "v")
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(logsDir, "aixecutor.log"))
			if err != nil {
				t.Fatalf("reading run log: %v", err)
			}
			out := string(data)
			if got := strings.Contains(out, "a debug line"); got != tc.wantDebug {
				t.Errorf("file debug present = %v, want %v\n%s", got, tc.wantDebug, out)
			}
			if got := strings.Contains(out, "an info line"); got != tc.wantInfo {
				t.Errorf("file info present = %v, want %v\n%s", got, tc.wantInfo, out)
			}
		})
	}
}

// TestNilLoggerSafe proves every method is a no-op on a nil *Logger.
func TestNilLoggerSafe(t *testing.T) {
	var l *Logger
	// None of these should panic.
	l.Info("x")
	l.Debug("x")
	l.Warn("x")
	l.Error("x")
	l.Infof("x %d", 1)
	if err := l.AttachRunFile(t.TempDir()); err != nil {
		t.Errorf("nil AttachRunFile: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if l.LogsDir() != "" {
		t.Errorf("nil LogsDir should be empty")
	}
}

// TestRedactedEnvKeys proves env VALUES never appear and secret-looking keys are
// marked redacted, while ordinary keys pass through (names only), sorted.
func TestRedactedEnvKeys(t *testing.T) {
	keys := redactedEnvKeys(map[string]string{
		"ANTHROPIC_API_KEY": "sk-secret-value",
		"AWS_SECRET_TOKEN":  "hunter2",
		"PATH":              "/usr/bin",
		"HOME":              "/home/u",
	})
	joined := strings.Join(keys, ",")
	for _, secret := range []string{"sk-secret-value", "hunter2"} {
		if strings.Contains(joined, secret) {
			t.Errorf("redactedEnvKeys leaked a value %q: %v", secret, keys)
		}
	}
	if !strings.Contains(joined, "ANTHROPIC_API_KEY (redacted)") {
		t.Errorf("secret key not marked redacted: %v", keys)
	}
	if !strings.Contains(joined, "AWS_SECRET_TOKEN (redacted)") {
		t.Errorf("secret key not marked redacted: %v", keys)
	}
	// Ordinary keys appear by name.
	if !strings.Contains(joined, "PATH") || !strings.Contains(joined, "HOME") {
		t.Errorf("ordinary keys missing: %v", keys)
	}
	// Deterministic order.
	if keys[0] != "ANTHROPIC_API_KEY (redacted)" {
		t.Errorf("keys not sorted: %v", keys)
	}
	if redactedEnvKeys(nil) != nil {
		t.Errorf("empty env should yield nil keys")
	}
}

// TestIsTTYOnBuffer proves a non-*os.File writer is never a TTY (so progress
// degrades to plain output), and a regular file is not a char device either.
func TestIsTTYOnBuffer(t *testing.T) {
	if IsTTY(&bytes.Buffer{}) {
		t.Error("a bytes.Buffer must not be a TTY")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTTY(f) {
		t.Error("a regular file must not be a TTY")
	}
}
