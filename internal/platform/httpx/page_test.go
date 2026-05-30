package httpx

import "testing"

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultPageSize},
		{-5, DefaultPageSize},
		{25, 25},
		{MaxPageSize, MaxPageSize},
		{MaxPageSize + 1, MaxPageSize},
		{1_000_000, MaxPageSize},
	}
	for _, c := range cases {
		if got := ClampLimit(c.in); got != c.want {
			t.Errorf("ClampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
