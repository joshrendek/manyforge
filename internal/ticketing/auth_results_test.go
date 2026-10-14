package ticketing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// The *_result fields keep the frozen DnsRecordState enum: anything but
// pass/fail/pending/unknown, "none" included, collapses to unknown (FR-019).
func TestProjectAuthResultsKeepsFrozenEnum(t *testing.T) {
	cases := []struct {
		raw              string
		spf, dkim, dmarc string
	}{
		{`{"spf":"pass","dkim":"fail","dmarc":"none"}`, "pass", "fail", "unknown"},
		{`{"spf":"softfail","dkim":"neutral","dmarc":"temperror"}`, "unknown", "unknown", "unknown"},
		{`{"spf":"pending","dkim":""}`, "pending", "unknown", "unknown"},
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

// manyforge-clk7: a sender domain with no DMARC record yields dmarc=none, which
// auth_verdicts carries verbatim. Only RFC 8601 result tokens are echoed; any
// other stored value is omitted, and the object is nil when nothing survives.
func TestProjectAuthVerdicts(t *testing.T) {
	cases := []struct {
		raw  string
		want *AuthVerdicts
	}{
		{`{"spf":"pass","dkim":"pass","dmarc":"none"}`, &AuthVerdicts{SPF: "pass", DKIM: "pass", DMARC: "none"}},
		{`{"spf":"softfail","dkim":"neutral","dmarc":"permerror"}`, &AuthVerdicts{SPF: "softfail", DKIM: "neutral", DMARC: "permerror"}},
		{`{"spf":"temperror","dkim":"policy","dmarc":"fail"}`, &AuthVerdicts{SPF: "temperror", DKIM: "policy", DMARC: "fail"}},
		{`{"spf":"<script>","dkim":"PASS","dmarc":"none"}`, &AuthVerdicts{DMARC: "none"}},
		{`{"spf":"pending","dkim":"unknown","dmarc":""}`, nil},
		{`{}`, nil},
		{`not json`, nil},
		{``, nil},
	}
	for _, tc := range cases {
		got := projectAuthVerdicts([]byte(tc.raw))
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("projectAuthVerdicts(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestMessageRespAuthVerdictsJSON(t *testing.T) {
	m, err := toMessage(dbgen.TicketMessage{ID: uuid.New(), AuthResults: []byte(`{"spf":"pass","dmarc":"none"}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(toMessageResp(m))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"dmarc_result":"unknown","auth_verdicts":{"spf":"pass","dmarc":"none"}`) {
		t.Fatalf("response JSON = %s", raw)
	}

	m, err = toMessage(dbgen.TicketMessage{ID: uuid.New()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(toMessageResp(m))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "auth_verdicts") {
		t.Fatalf("auth_verdicts must be omitted without stored results: %s", raw)
	}
}
