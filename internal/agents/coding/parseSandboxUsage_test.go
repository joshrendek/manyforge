package coding

import "testing"

// parseSandboxUsage parses the in-band usage.json bytes carried on SandboxResult.Outputs
// (the entrypoint's sqlite3 -json output: a one-element array). It must degrade to zero on
// any absence/garbage — a review is never failed for missing usage data.
func TestParseSandboxUsage(t *testing.T) {
	b := []byte(`[{"cost":0.19,"input":9917,"output":506,"cache_read":6336,"cache_write":0}]`)
	u := parseSandboxUsage(b)
	if u.Cost != 0.19 || u.Input != 9917 || u.Output != 506 || u.CacheRead != 6336 || u.CacheWrite != 0 {
		t.Fatalf("parsed usage = %+v", u)
	}
}

func TestParseSandboxUsage_ZeroOnAbsentOrGarbage(t *testing.T) {
	cases := map[string][]byte{
		"nil bytes":   nil,
		"empty bytes": []byte(``),
		"empty array": []byte(`[]`),
		"garbage":     []byte(`{not json`),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseSandboxUsage(b); got != (sandboxUsage{}) {
				t.Fatalf("%s: parseSandboxUsage = %+v, want zero value", name, got)
			}
		})
	}
}
