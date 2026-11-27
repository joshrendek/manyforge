package mailing

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBrandLogoURLIgnoresPortalBase(t *testing.T) {
	s := &Service{
		PublicBaseURL: "https://hub.example.test/",
		PortalBaseURL: func(context.Context, uuid.UUID) (string, bool) { return "https://portal.example.test", true },
	}
	got := s.brandLogoURL(uuid.New(), []byte{1, 2, 3, 4})
	if !strings.HasPrefix(got, "https://hub.example.test/m/b/") {
		t.Fatalf("logo URL = %q, want instance base", got)
	}
}

func TestConfirmBaseURL(t *testing.T) {
	id := uuid.New()
	portal := func(base string, ok bool) func(context.Context, uuid.UUID) (string, bool) {
		return func(_ context.Context, got uuid.UUID) (string, bool) {
			if got != id {
				t.Fatalf("resolver got business %s, want %s", got, id)
			}
			return base, ok
		}
	}
	cases := []struct {
		name string
		fn   func(context.Context, uuid.UUID) (string, bool)
		want string
	}{
		{"nil", nil, "https://hub.example.test"},
		{"not found", portal("", false), "https://hub.example.test"},
		{"empty", portal("", true), "https://hub.example.test"},
		{"found", portal("https://portal.example.test/", true), "https://portal.example.test"},
	}
	for _, tc := range cases {
		s := &Service{PublicBaseURL: "https://hub.example.test/", PortalBaseURL: tc.fn}
		if got := s.confirmBaseURL(context.Background(), id); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
