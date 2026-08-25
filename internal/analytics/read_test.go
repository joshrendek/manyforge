package analytics

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func TestCompleteDailySeriesFillsGapsInCalendarOrder(t *testing.T) {
	from := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.July, 5, 0, 0, 0, 0, time.UTC)

	got := completeDailySeries([]DayPoint{
		{Date: "2026-07-05", Pageviews: 7, Visitors: 3},
		{Date: "2026-07-01", Pageviews: 2, Visitors: 1},
	}, from, to)

	if len(got) != 5 {
		t.Fatalf("len(series) = %d, want 5", len(got))
	}
	for i, want := range []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04", "2026-07-05"} {
		if got[i].Date != want {
			t.Errorf("series[%d].date = %q, want %q", i, got[i].Date, want)
		}
	}
	if got[0].Pageviews != 2 || got[4].Pageviews != 7 {
		t.Errorf("traffic points were not preserved: %+v", got)
	}
	for _, i := range []int{1, 2, 3} {
		if got[i].Pageviews != 0 || got[i].Visitors != 0 {
			t.Errorf("gap at index %d is not zero-filled: %+v", i, got[i])
		}
	}
}

func TestCompleteDailySeriesSupportsEveryDashboardRange(t *testing.T) {
	to := time.Date(2026, time.July, 31, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{7, 30, 90, maxRangeDays} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			from := to.AddDate(0, 0, -(days - 1))
			got := completeDailySeries(nil, from, to)
			if len(got) != days {
				t.Fatalf("len(series) = %d, want %d", len(got), days)
			}
			if got[0].Date != from.Format("2006-01-02") || got[len(got)-1].Date != to.Format("2006-01-02") {
				t.Errorf("series boundaries = %s..%s, want %s..%s",
					got[0].Date, got[len(got)-1].Date,
					from.Format("2006-01-02"), to.Format("2006-01-02"))
			}
		})
	}
}

func TestSeriesMetricsIncludesZeroDaysInVisitorAverage(t *testing.T) {
	pageviews, peak, average := seriesMetrics([]DayPoint{
		{Pageviews: 10, Visitors: 4},
		{},
		{Pageviews: 5, Visitors: 2},
	})
	if pageviews != 15 || peak != 4 {
		t.Fatalf("metrics = pageviews %d, peak %d; want 15, 4", pageviews, peak)
	}
	if math.Abs(average-2) > 0.0001 {
		t.Errorf("average = %f, want 2 (including the zero day)", average)
	}
}

func TestPercentChangeHasNoValueWithoutPriorBaseline(t *testing.T) {
	if got := percentChange(10, 0); got != nil {
		t.Fatalf("change from zero = %v, want nil", *got)
	}
	got := percentChange(15, 10)
	if got == nil || math.Abs(*got-50) > 0.0001 {
		t.Fatalf("change = %v, want 50", got)
	}
}
