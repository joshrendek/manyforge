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
		f, err := parseWindowTime(from)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'from': %w", errs.ErrValidation)
		}
		t, err := parseWindowTime(to)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'to': %w", errs.ErrValidation)
		}
		if t.Before(f) {
			return Window{}, fmt.Errorf("accounting: 'from' must be <= 'to': %w", errs.ErrValidation)
		}
		if t.Sub(f) > maxWindowSpan {
			return Window{}, fmt.Errorf("accounting: window exceeds %d days: %w", int(maxWindowSpan.Hours()/24), errs.ErrValidation)
		}
		return Window{From: f, To: t}, nil
	default:
		return Window{}, fmt.Errorf("accounting: unknown window %q: %w", name, errs.ErrValidation)
	}
}

func parseWindowTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
