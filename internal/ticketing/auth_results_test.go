package ticketing

import "testing"

// manyforge-clk7: a sender domain with no DMARC record yields dmarc=none, which
// must surface as "none" (no policy published) rather than collapsing into
// "unknown". Unrecognised verdicts still map to unknown (FR-019).
func TestProjectAuthResults(t *testing.T) {
	cases := []struct {
		raw              string
		spf, dkim, dmarc string
	}{
		{`{"spf":"pass","dkim":"pass","dmarc":"none"}`, "pass", "pass", "none"},
		{`{"spf":"none","dkim":"none","dmarc":"fail"}`, "none", "none", "fail"},
		{`{"spf":"softfail","dkim":"neutral","dmarc":"temperror"}`, "unknown", "unknown", "unknown"},
		{`{"spf":"pending","dkim":"","dmarc":"NONE"}`, "pending", "unknown", "unknown"},
		{`{}`, "unknown", "unknown", "unknown"},
		{`not json`, "unknown", "unknown", "unknown"},
		{``, "unknown", "unknown", "unknown"},
	}
	for _, tc := range cases {
		spf, dkim, dmarc := projectAuthResults([]byte(tc.raw))
		if spf != tc.spf || dkim != tc.dkim || dmarc != tc.dmarc {
			t.Errorf("projectAuthResults(%q) = %s/%s/%s, want %s/%s/%s", tc.raw, spf, dkim, dmarc, tc.spf, tc.dkim, tc.dmarc)
		}
	}
}
