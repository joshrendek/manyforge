package notify

import (
	"strings"
	"testing"
)

func TestVerpAddress(t *testing.T) {
	token45 := strings.Repeat("a", 22) + "." + strings.Repeat("B", 22)
	cases := []struct {
		name, addr, token, want string
	}{
		{"plus-addresses the local part", "support@support.garden.gg", token45, "support+" + token45 + "@support.garden.gg"},
		{"preserves token case", "support@x.test", "AbC.dEf", "support+AbC.dEf@x.test"},
		{"no @ is unchanged", "support", "tok", "support"},
		// Resend 422s a local part over 64 octets; fall back to the plain address.
		{"over 64 octets falls back", strings.Repeat("l", 19) + "@x.test", token45, strings.Repeat("l", 19) + "@x.test"},
		{"exactly 64 octets is kept", strings.Repeat("l", 18) + "@x.test", token45, strings.Repeat("l", 18) + "+" + token45 + "@x.test"},
	}
	for _, c := range cases {
		if got := verpAddress(c.addr, c.token); got != c.want {
			t.Errorf("%s: verpAddress(%q, %q) = %q, want %q", c.name, c.addr, c.token, got, c.want)
		}
	}
}
