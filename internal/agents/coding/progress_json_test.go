package coding

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodeReviewProgressJSON(t *testing.T) {
	withProg := CodeReview{Progress: json.RawMessage(`{"phase":"reviewing","tokens":12,"preview":"x"}`)}
	b, _ := json.Marshal(withProg)
	if !strings.Contains(string(b), `"progress":{"phase":"reviewing"`) {
		t.Fatalf("progress not marshalled into the response: %s", b)
	}
	without := CodeReview{}
	b2, _ := json.Marshal(without)
	if strings.Contains(string(b2), "progress") {
		t.Fatalf("empty progress must be omitted (omitempty): %s", b2)
	}
}
