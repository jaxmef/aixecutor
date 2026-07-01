package run

import (
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		updated time.Time
		want    string
	}{
		{"normal span", base, base.Add(2*time.Minute + 3*time.Second), "2m3s"},
		{"zero created", time.Time{}, base, "-"},
		{"zero updated", base, time.Time{}, "-"},
		{"negative span", base, base.Add(-time.Minute), "-"},
		{"sub-second rounds down", base, base.Add(1200 * time.Millisecond), "1s"},
		{"sub-second rounds up", base, base.Add(1600 * time.Millisecond), "2s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatElapsed(tt.created, tt.updated); got != tt.want {
				t.Errorf("FormatElapsed() = %q, want %q", got, tt.want)
			}
		})
	}
}
