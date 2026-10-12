package ticketing

import (
	"strings"
	"testing"
)

// validSendAs must reject everything inbound_address_send_as_chk rejects, so a
// bad send_as is a 400 and never a CHECK violation (500).
func TestValidSendAs(t *testing.T) {
	for v, want := range map[string]bool{
		"support@garden.gg":                        true,
		"first.last+tag@mail.garden.gg":            true,
		"a b@garden.gg":                            false,
		"a\tb@garden.gg":                           false,
		"a" + string(rune(0x00a0)) + "b@garden.gg": false, // no-break space
		"a" + string(rune(0x2003)) + "b@garden.gg": false, // em space
		"a\x00b@garden.gg":                         false,
		"a" + string(rune(0x200b)) + "b@garden.gg": false, // zero-width space (format, not graphic)
		"support@garden gg":                        false,
		"support.garden.gg":                        false,
		"support@garden":                           false,
		"a@b@garden.gg":                            false,
		"a@" + strings.Repeat("b", 250) + ".com":   false, // 256 > 254
		"a@" + strings.Repeat("b", 248) + ".com":   true,  // exactly 254
		"é" + strings.Repeat("a", 247) + "@x.com":  false, // 254 chars but 255 bytes: stricter is fine
	} {
		if got := validSendAs(v); got != want {
			t.Errorf("validSendAs(%q) = %v, want %v", v, got, want)
		}
	}
}
