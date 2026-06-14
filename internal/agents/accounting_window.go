package agents

import (
	"fmt"
	"time"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Window is a half-open [From, To) time range in UTC. Reports filter agent_run by
// created_at within this range.
type Window struct {
	From time.Time
	To   time.Time
}

// maxWindowSpan caps a custom range so a report can't trigger an unbounded scan
// (defense in depth alongside the row LIMIT on the runs list).
const maxWindowSpan = 366 * 24 * time.Hour

// ResolveWindow maps a window name (+ optional custom from/to) to an explicit
// [From, To) in UTC. `now` is injected for testability. Presets:
//
//	"" / "this_month" -> [first-of-month, now]
//	"last_month"      -> [first-of-prev-month, first-of-month]
//	"last_30_days"    -> [now-30d, now]
//	"custom"          -> [from, to], parsed as RFC3339 or YYYY-MM-DD
//
// A date-only "to" (YYYY-MM-DD) names a whole day: because the range is half-open, it
// is advanced to the next midnight so that final day is included rather than silently
// excluded by the midnight boundary. An RFC3339 "to" with an explicit time is honored
// exactly. "from" needs no such adjustment — its midnight is the inclusive start.
func ResolveWindow(name, from, to string, now time.Time) (Window, error) {
	n := now.UTC()
	monthStart := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	switch name {
	case "", "this_month":
		return Window{From: monthStart, To: n}, nil
	case "last_month":
		return Window{From: monthStart.AddDate(0, -1, 0), To: monthStart}, nil
	case "last_30_days":
		return Window{From: n.Add(-30 * 24 * time.Hour), To: n}, nil
	case "custom":
		f, _, err := parseWindowTime(from)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'from': %w", errs.ErrValidation)
		}
		t, toDateOnly, err := parseWindowTime(to)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'to': %w", errs.ErrValidation)
		}
		// Validate ordering on the RAW parsed instants, before normalization can mask
		// an inverted range (e.g. to=2026-03-01 normalizing past from=2026-03-31).
		if t.Before(f) {
			return Window{}, fmt.Errorf("accounting: 'from' must be <= 'to': %w", errs.ErrValidation)
		}
		if toDateOnly {
			t = t.AddDate(0, 0, 1) // include the whole final day in the half-open range
		}
		if t.Sub(f) > maxWindowSpan {
			return Window{}, fmt.Errorf("accounting: window exceeds %d days: %w", int(maxWindowSpan.Hours()/24), errs.ErrValidation)
		}
		return Window{From: f, To: t}, nil
	default:
		return Window{}, fmt.Errorf("accounting: unknown window %q: %w", name, errs.ErrValidation)
	}
}

// parseWindowTime parses an RFC3339 instant or a YYYY-MM-DD date (both as UTC). The
// dateOnly return reports the latter, so a caller can treat a bare date as a whole day.
func parseWindowTime(s string) (t time.Time, dateOnly bool, err error) {
	if s == "" {
		return time.Time{}, false, fmt.Errorf("empty")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), false, nil
	}
	t, err = time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false, err
	}
	return t.UTC(), true, nil
}
