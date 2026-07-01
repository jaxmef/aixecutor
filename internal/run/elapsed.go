package run

import "time"

// FormatElapsed renders the wall-clock span from created to updated (a run's
// last checkpoint), so callers can show how long the run has taken / ran for. A
// zero or negative span (missing/foreign timestamps) renders as "-". The duration
// is rounded to the second for readability.
func FormatElapsed(created, updated time.Time) string {
	if created.IsZero() || updated.IsZero() {
		return "-"
	}
	d := updated.Sub(created)
	if d < 0 {
		return "-"
	}
	return d.Round(time.Second).String()
}
