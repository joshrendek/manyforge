package security_regression

import (
	"strings"
	"testing"
)

// TestCodexRefreshTokenNeverInSandboxEnv pins that the sandbox env builder never emits the sealed
// or plaintext refresh token — only the short-lived access token (LLM_API_KEY) + account id.
func TestCodexRefreshTokenNeverInSandboxEnv(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	// sandboxEnv must not reference any oauth refresh field.
	for _, forbidden := range []string{"OauthRefreshToken", "oauth_refresh_token", "RefreshToken"} {
		// scope to the sandboxEnv function body
		start := strings.Index(src, "func sandboxEnv(")
		if start == -1 {
			t.Fatal("could not find sandboxEnv — pin broken, update in the same change if intentional")
		}
		body := src[start:]
		if end := strings.Index(body[1:], "\nfunc "); end != -1 {
			body = body[:end]
		}
		if strings.Contains(body, forbidden) {
			t.Errorf("sandboxEnv must NOT reference %q — the refresh token must never enter the sandbox; pin broken, update in the same change if intentional", forbidden)
		}
	}
}

// TestCodexOAuthClientTargetsOpenAIAuth pins the OAuth client host + refresh grant so a refactor
// can't silently repoint it or drop the refresh flow.
func TestCodexOAuthClientTargetsOpenAIAuth(t *testing.T) {
	src := mustRead(t, "../codexoauth/oauth.go")
	for _, lit := range []string{`"https://auth.openai.com"`, `"refresh_token"`, `app_EMoamEEZ73f0CkXaXp7hrann`} {
		if !strings.Contains(src, lit) {
			t.Errorf("codexoauth/oauth.go must contain %q — pin broken, update in the same change if intentional", lit)
		}
	}
}

// TestCodexDefinerFunctionsRevokePublic pins that every Codex refresh-sweep SECURITY DEFINER
// function (RLS-exempt, cross-tenant credential access) has PUBLIC execute revoked. These
// functions run as their owner regardless of the calling role's RLS policies, so leaving them
// EXECUTE-able by PUBLIC would let any DB principal read/rotate/clear another tenant's Codex
// OAuth tokens.
func TestCodexDefinerFunctionsRevokePublic(t *testing.T) {
	sql := mustRead(t, "../../migrations/0096_codex_refresh_sweep.up.sql")
	for _, fn := range []string{"codex_claim_for_refresh", "codex_apply_refresh", "codex_disconnect_system"} {
		if !strings.Contains(sql, "REVOKE ALL ON FUNCTION "+fn) {
			t.Errorf("codex definer functions must REVOKE ALL FROM PUBLIC (RLS-exempt cross-tenant credential access) — pin broken, update in the same change if intentional: missing REVOKE for %s", fn)
		}
	}
}
