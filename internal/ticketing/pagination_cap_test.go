package ticketing

import "testing"

// T071 / FR-020 — every support list endpoint silently caps page size at 100
// (and defaults to 50) via the single clampLimit chokepoint, so a hostile
// per_page=10000000 returns at most 100 rows instead of the whole table.
//
// TestClampLimitCaps covers cap math at the boundaries; the integration test
// TestListTicketsLimitCappedAt100 proves the observable list response is capped.

func TestClampLimitCaps(t *testing.T) {
	const def, max = 50, 100
	cases := []struct {
		name     string
		in, want int
	}{
		{"negative→default", -7, def},
		{"zero→default", 0, def},
		{"one passes through", 1, 1},
		{"just under default", 49, 49},
		{"default", 50, 50},
		{"just under cap", 99, 99},
		{"at cap", 100, max},
		{"just over cap→capped", 101, max},
		{"absurd per_page→capped", 10_000_000, max},
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("%s: clampLimit(%d) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}
