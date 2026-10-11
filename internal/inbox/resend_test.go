package inbox

import (
	"fmt"
	"testing"
)

func TestResendCandidatesDedupesAndBounds(t *testing.T) {
	got := resendCandidates([]string{
		" Support+Tok@Example.com ", "support+Tok@example.com", "", "not-an-address", "other@example.com",
	})
	want := []string{"Support+Tok@Example.com", "other@example.com"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v (deduped on routing key + token, junk dropped, order kept)", got, want)
	}
	if got := resendCandidates([]string{"support+A@x.test", "support+B@x.test"}); len(got) != 2 {
		t.Fatalf("distinct tokens on one address collapsed: %v", got)
	}
	many := make([]string, maxResendCandidates+30)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@x.test", i)
	}
	if got := resendCandidates(many); len(got) != maxResendCandidates {
		t.Fatalf("len = %d, want the %d cap", len(got), maxResendCandidates)
	}
}
