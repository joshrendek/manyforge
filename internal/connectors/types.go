// Package connectors stores and resolves per-business external-system credentials
// (Jira, Zendesk) with the secret sealed at rest in the platform secrets vault.
package connectors

import "context"

// knownConnectorTypes gates the type enum at the service boundary so an unknown
// type is a clean validation error, not a later DB enum failure.
var knownConnectorTypes = map[string]bool{"jira": true, "zendesk": true}

// Credential is the secret payload sealed into the vault. For Jira Cloud the auth
// is HTTP Basic email:api_token. WebhookSecret is the HMAC-SHA256 key used to
// verify inbound webhook payloads (X-Hub-Signature: sha256=<hex>).
type Credential struct {
	Email         string `json:"email"`
	APIToken      string `json:"api_token"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

// CreateConnectorInput is the caller-supplied connector-create request.
type CreateConnectorInput struct {
	Type                        string
	DisplayName                 string
	BaseURL                     string
	AllowPrivateBaseURL         bool
	SuppressNativeNotifications bool // when true, connector-linked replies skip the native email
	Email                       string
	APIToken                    string
	WebhookSecret               string
	Config                      map[string]any
}

// ResolvedConnector is a connector with its credential unsealed, returned by Resolve.
type ResolvedConnector struct {
	ID                  string
	Type                string
	BaseURL             string
	AllowPrivateBaseURL bool
	Config              map[string]any
	Credential          Credential
}

// VerifyTarget is what a Verifier inspects for a live test-call at create time,
// before the connector is persisted.
type VerifyTarget struct {
	Type                string
	BaseURL             string
	AllowPrivateBaseURL bool
	Credential          Credential
}

// Verifier optionally performs a live test-call confirming a credential works
// before it is stored. US1 ships no concrete implementation (nil = skip); the
// real Jira verifier lands in US3. Kept as a 1-method seam, not an abstraction.
type Verifier interface {
	Verify(ctx context.Context, t VerifyTarget) error
}
