package log

import (
	"strings"
	"testing"
)

// TestColorizeDisabled proves Colorize is an identity when disabled or the code is
// empty (so callers can wrap unconditionally).
func TestColorizeDisabled(t *testing.T) {
	if got := Colorize(false, AnsiRed, "x"); got != "x" {
		t.Errorf("disabled Colorize must return input unchanged, got %q", got)
	}
	if got := Colorize(true, "", "x"); got != "x" {
		t.Errorf("empty code must return input unchanged, got %q", got)
	}
}

// TestColorizeEnabled proves an enabled Colorize wraps the string with the code and
// a trailing reset.
func TestColorizeEnabled(t *testing.T) {
	got := Colorize(true, AnsiGreen, "ok")
	if want := AnsiGreen + "ok" + AnsiReset; got != want {
		t.Errorf("Colorize(true, green, ok) = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, AnsiReset) {
		t.Errorf("coloured output must end with reset: %q", got)
	}
}
