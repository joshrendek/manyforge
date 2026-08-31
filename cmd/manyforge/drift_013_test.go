//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

var inScope013CoreOps = []string{
	"POST /mailing/public/{}/subscribe", "OPTIONS /mailing/public/{}/subscribe",
	"POST /mailing/s2s/{}/subscribers", "DELETE /mailing/s2s/{}/subscribers/{}",
	"POST /inbound/mailing/{}/ses", "POST /inbound/mailing/{}/resend",
	"GET /m/confirm/{}", "POST /m/confirm/{}", "GET /m/u/{}", "POST /m/u/{}",
	"GET /m/o/{}", "GET /m/c/{}",
	"GET /businesses/{}/mailing/lists", "POST /businesses/{}/mailing/lists", "GET /businesses/{}/mailing/lists/{}", "PATCH /businesses/{}/mailing/lists/{}", "DELETE /businesses/{}/mailing/lists/{}",
	"GET /businesses/{}/mailing/lists/{}/subscribers", "POST /businesses/{}/mailing/lists/{}/subscribers", "POST /businesses/{}/mailing/lists/{}/subscribers/from-contacts", "POST /businesses/{}/mailing/lists/{}/subscribers/import", "GET /businesses/{}/mailing/lists/{}/subscribers/export", "GET /businesses/{}/mailing/lists/{}/subscribers/{}", "PATCH /businesses/{}/mailing/lists/{}/subscribers/{}", "DELETE /businesses/{}/mailing/lists/{}/subscribers/{}",
	"GET /businesses/{}/mailing/lists/{}/keys", "POST /businesses/{}/mailing/lists/{}/keys", "DELETE /businesses/{}/mailing/lists/{}/keys/{}",
	"GET /businesses/{}/mailing/sending-profile", "PUT /businesses/{}/mailing/sending-profile", "DELETE /businesses/{}/mailing/sending-profile", "POST /businesses/{}/mailing/sending-profile/verify", "POST /businesses/{}/mailing/sending-profile/test-send",
	"GET /businesses/{}/mailing/templates", "POST /businesses/{}/mailing/templates", "POST /businesses/{}/mailing/templates/preview", "GET /businesses/{}/mailing/templates/{}", "PATCH /businesses/{}/mailing/templates/{}", "DELETE /businesses/{}/mailing/templates/{}",
	"GET /businesses/{}/mailing/campaigns", "POST /businesses/{}/mailing/campaigns", "POST /businesses/{}/mailing/campaigns/preview",
	"GET /businesses/{}/mailing/campaigns/{}", "PATCH /businesses/{}/mailing/campaigns/{}", "DELETE /businesses/{}/mailing/campaigns/{}",
	"POST /businesses/{}/mailing/campaigns/{}/send", "POST /businesses/{}/mailing/campaigns/{}/cancel", "POST /businesses/{}/mailing/campaigns/{}/test-send",
	"GET /businesses/{}/mailing/campaigns/{}/stats", "GET /businesses/{}/mailing/campaigns/{}/deliveries",
	"GET /businesses/{}/mailing/suppressions", "POST /businesses/{}/mailing/suppressions", "DELETE /businesses/{}/mailing/suppressions/{}",
}

func TestOpenAPIDrift013Core(t *testing.T) {
	routes := apiRoutes(t)
	spec := spec013Routes(t)
	for _, op := range inScope013CoreOps {
		if !spec[op] {
			t.Errorf("test bug: %q missing from 013 contract", op)
		}
		if !routes[op] {
			t.Errorf("013 drift: %q documented but not served", op)
		}
	}
	var extra []string
	for op := range routes {
		if strings.Contains(op, "/mailing/") && !spec[op] {
			extra = append(extra, op)
		}
	}
	sort.Strings(extra)
	for _, op := range extra {
		t.Errorf("013 drift: %q served but undocumented", op)
	}
}
