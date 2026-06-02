package ticketing

import (
	"os"
	"strings"
	"testing"
)

// T071 / FR-020 — every support list endpoint silently caps page size at 100
// (and defaults to 50) via the single clampLimit chokepoint, so a hostile
// per_page=10000000 returns at most 100 rows instead of the whole table.
//
// Two fast (Docker-free) guards, complementary to the behavioural proof in
// read_integration_test.go's TestListTicketsLimitCappedAt100:
//   - TestClampLimitCaps pins the cap math exhaustively at the boundaries.
//   - TestListEndpointsRouteThroughClampLimit source-pins that EVERY List*
//     method calls clampLimit, so a future endpoint that forgets the cap
//     (re-opening the DoS) fails CI loudly rather than silently shipping.

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

func TestListEndpointsRouteThroughClampLimit(t *testing.T) {
	// Every support list endpoint, mapped to the source file it lives in. The
	// five keyset-paginated list endpoints (FR-020) plus ListAssignableMembers,
	// a capped helper, included so the whole capped surface is guarded.
	endpoints := map[string]string{
		"ListTickets":           "service.go",
		"ListMessages":          "service.go",
		"ListRequesters":        "service.go",
		"ListEmailDomains":      "identity.go",
		"ListInboundAddresses":  "identity.go",
		"ListAssignableMembers": "assignable.go",
	}
	srcCache := map[string]string{}
	for method, file := range endpoints {
		src, ok := srcCache[file]
		if !ok {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			src = string(b)
			srcCache[file] = src
		}
		if body := funcBody(t, src, method); !strings.Contains(body, "clampLimit(") {
			t.Errorf("%s (%s) does not call clampLimit — FR-020 page cap missing on this endpoint", method, file)
		}
	}
}

// funcBody returns the source text of the method named `name` — from its first
// occurrence as `) name(` to the next top-level `\nfunc ` — so a Contains check
// is scoped to that one method body, not the whole file.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, ") "+name+"(")
	if start < 0 {
		t.Fatalf("method %s not found in source — rename? update the T071 endpoint map", name)
	}
	rest := src[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1]
	}
	return rest
}
