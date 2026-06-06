package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 6, 5, 14, 30, 0, 0, time.UTC)
	monthStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name, win, from, to string
		wantFrom, wantTo    time.Time
		wantErr             bool
	}{
		{name: "default empty", win: "", wantFrom: monthStart, wantTo: now},
		{name: "this_month", win: "this_month", wantFrom: monthStart, wantTo: now},
		{name: "last_month", win: "last_month", wantFrom: prevMonthStart, wantTo: monthStart},
		{name: "last_30_days", win: "last_30_days", wantFrom: now.Add(-30 * 24 * time.Hour), wantTo: now},
		{name: "custom rfc3339", win: "custom", from: "2026-03-01T00:00:00Z", to: "2026-03-31T00:00:00Z",
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)},
		{name: "custom date-only", win: "custom", from: "2026-03-01", to: "2026-03-31",
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)},
		{name: "custom from>to", win: "custom", from: "2026-03-31", to: "2026-03-01", wantErr: true},
		{name: "custom over cap", win: "custom", from: "2024-01-01", to: "2026-01-02", wantErr: true},
		{name: "custom unparseable", win: "custom", from: "nope", to: "2026-03-01", wantErr: true},
		{name: "unknown window", win: "yesterday", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := ResolveWindow(c.win, c.from, c.to, now)
			if c.wantErr {
				if !errors.Is(err, errs.ErrValidation) {
					t.Fatalf("want ErrValidation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !w.From.Equal(c.wantFrom) || !w.To.Equal(c.wantTo) {
				t.Fatalf("got [%s,%s), want [%s,%s)", w.From, w.To, c.wantFrom, c.wantTo)
			}
		})
	}
}
