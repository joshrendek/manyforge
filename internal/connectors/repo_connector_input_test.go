package connectors

import (
	"encoding/json"
	"testing"
)

// The repo-connector create handler decodes the request body directly into
// CreateRepoConnectorInput, so its json tags ARE the API contract. This pins that
// a snake_case body (what the OpenAPI spec + the web client send) populates every
// field — a regression to untagged fields silently rejected real requests with
// "display_name required" (manyforge-elo, found dogfooding the Code Review UI).
func TestCreateRepoConnectorInputDecodesSnakeCase(t *testing.T) {
	body := `{
		"type": "github",
		"display_name": "manyforge (dogfood)",
		"base_url": "https://api.github.com",
		"repo": "owner/name",
		"allow_private_base_url": true,
		"api_token": "ghp_secret"
	}`
	var in CreateRepoConnectorInput
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in.Type != "github" {
		t.Errorf("Type: got %q", in.Type)
	}
	if in.DisplayName != "manyforge (dogfood)" {
		t.Errorf("DisplayName not decoded from display_name: got %q", in.DisplayName)
	}
	if in.BaseURL != "https://api.github.com" {
		t.Errorf("BaseURL not decoded from base_url: got %q", in.BaseURL)
	}
	if in.Repo != "owner/name" {
		t.Errorf("Repo: got %q", in.Repo)
	}
	if !in.AllowPrivateBaseURL {
		t.Error("AllowPrivateBaseURL not decoded from allow_private_base_url")
	}
	if in.APIToken != "ghp_secret" {
		t.Errorf("APIToken not decoded from api_token: got %q", in.APIToken)
	}
}
