// Finding MF-017-REPLY-TOKEN-SECRET (doc_support017.go / spec 017 R14).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017ReplyTokenSecretRequiredInProduction(t *testing.T) {
	cfg := mustRead(t, "../platform/config/config.go")
	for _, pin := range []string{"func (cfg Config) ValidateServe() error",
		`cfg.Environment == "production" && len(bytes.TrimSpace(cfg.InboundReplyTokenSecret)) == 0`,
		"MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production"} {
		if !strings.Contains(cfg, pin) {
			t.Errorf("config.go missing %q", pin)
		}
	}
	main := mustRead(t, "../../cmd/manyforge/main.go")
	guard := strings.Index(main, "cfg.ValidateServe()")
	migrate := strings.Index(main, `os.Args[1] == "migrate"`)
	if guard < 0 || migrate < 0 || guard < migrate {
		t.Error("main.go must call cfg.ValidateServe() on the serve path, after the migrate branch")
	}
	chart := mustRead(t, "../../charts/manyforge/templates/deployment.yaml")
	if !strings.Contains(chart, "MANYFORGE_INBOUND_REPLY_TOKEN_SECRET") || !strings.Contains(chart, "inboundReplyTokenKey") {
		t.Error("Helm deployment must wire MANYFORGE_INBOUND_REPLY_TOKEN_SECRET from the master-keys Secret")
	}
}
