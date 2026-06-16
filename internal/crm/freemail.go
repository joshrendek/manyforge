package crm

import "strings"

// freeEmailDomains is the denylist of public/free mailbox providers. A sender on one of
// these domains must NOT auto-create a company (every gmail.com sender would otherwise
// collapse into one bogus "gmail.com" company); the inbound seam excludes them via
// IsFreeEmailDomain before the crm_link_inbound_sender DEFINER upserts a company.
var freeEmailDomains = map[string]struct{}{
	"gmail.com": {}, "googlemail.com": {}, "outlook.com": {}, "hotmail.com": {},
	"live.com": {}, "msn.com": {}, "yahoo.com": {}, "ymail.com": {}, "icloud.com": {},
	"me.com": {}, "mac.com": {}, "aol.com": {}, "proton.me": {}, "protonmail.com": {},
	"gmx.com": {}, "gmx.net": {}, "mail.com": {}, "zoho.com": {}, "yandex.com": {},
}

// IsFreeEmailDomain reports whether domain is a public/free mailbox provider (so it should
// NOT auto-create a company). Comparison is case-insensitive and trim-tolerant.
func IsFreeEmailDomain(domain string) bool {
	_, ok := freeEmailDomains[strings.ToLower(strings.TrimSpace(domain))]
	return ok
}

// DomainFromEmail returns the lowercased domain part of an email, or "" if malformed
// (no '@', or nothing after the last '@').
func DomainFromEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}
