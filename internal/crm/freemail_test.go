package crm

import "testing"

func TestIsFreeEmailDomain(t *testing.T) {
	free := []string{
		"gmail.com", "GMAIL.COM", "outlook.com", "yahoo.com",
		"icloud.com", "proton.me", "hotmail.com",
	}
	for _, d := range free {
		if !IsFreeEmailDomain(d) {
			t.Errorf("IsFreeEmailDomain(%q) = false, want true", d)
		}
	}
	notFree := []string{"acme.com", "atlassian.net", "manyforge.test"}
	for _, d := range notFree {
		if IsFreeEmailDomain(d) {
			t.Errorf("IsFreeEmailDomain(%q) = true, want false", d)
		}
	}
}

func TestDomainFromEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ada@acme.com", "acme.com"},
		{"Ada@ACME.com", "acme.com"},
		{"bogus", ""},
		{"x@", ""},
	}
	for _, c := range cases {
		if got := DomainFromEmail(c.in); got != c.want {
			t.Errorf("DomainFromEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
