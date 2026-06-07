package connectors

import (
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestValidate(t *testing.T) {
	base := CreateConnectorInput{
		Type: "jira", DisplayName: "Acme Jira",
		BaseURL: "https://acme.atlassian.net", Email: "a@b.com", APIToken: "tok",
	}
	valid := func(m func(*CreateConnectorInput)) CreateConnectorInput {
		in := base
		m(&in)
		return in
	}
	cases := []struct {
		name    string
		in      CreateConnectorInput
		wantErr bool
	}{
		{"ok", base, false},
		{"unknown type", valid(func(i *CreateConnectorInput) { i.Type = "github" }), true},
		{"missing display_name", valid(func(i *CreateConnectorInput) { i.DisplayName = "" }), true},
		{"missing base_url", valid(func(i *CreateConnectorInput) { i.BaseURL = "" }), true},
		{"not http(s)", valid(func(i *CreateConnectorInput) { i.BaseURL = "ftp://x" }), true},
		{"missing email", valid(func(i *CreateConnectorInput) { i.Email = "" }), true},
		{"missing token", valid(func(i *CreateConnectorInput) { i.APIToken = "" }), true},
		{"blocked literal IP no trust", valid(func(i *CreateConnectorInput) { i.BaseURL = "http://10.0.0.1" }), true},
		{"loopback no trust blocked", valid(func(i *CreateConnectorInput) { i.BaseURL = "http://127.0.0.1" }), true},
		{"private IP with trust ok", valid(func(i *CreateConnectorInput) {
			i.BaseURL = "http://10.0.0.1"
			i.AllowPrivateBaseURL = true
		}), false},
		{"metadata IP blocked even with trust", valid(func(i *CreateConnectorInput) {
			i.BaseURL = "http://169.254.169.254"
			i.AllowPrivateBaseURL = true
		}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if tc.wantErr && !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
