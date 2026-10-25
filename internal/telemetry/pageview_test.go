package telemetry

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const testPageviewSession = "550e8400-e29b-41d4-a716-446655440000"

func TestValidSignedPageview(t *testing.T) {
	valid := []string{"/", "/dashboard", "/servers/:id", "/a-b/:item_2"}
	for _, path := range valid {
		t.Run("valid "+path, func(t *testing.T) {
			if !validSignedPageview(path, testPageviewSession) {
				t.Fatalf("validSignedPageview(%q, %q) = false", path, testPageviewSession)
			}
		})
	}

	invalid := map[string]string{
		"empty path":             "",
		"full URL":               "https://example.test/dashboard",
		"query":                  "/dashboard?tab=1",
		"hash":                   "/dashboard#top",
		"percent encoding":       "/dashboard%2Fhome",
		"matrix parameter":       "/dashboard;tab=1",
		"doubled slash":          "/servers//:id",
		"trailing slash":         "/dashboard/",
		"nonASCII":               "/café",
		"control":                "/dashboard\x00",
		"malformed placeholder":  "/servers/:",
		"placeholder in segment": "/servers/id:value",
		"literal session UUID":   "/sessions/" + testPageviewSession,
		"over 512 bytes":         "/" + strings.Repeat("a", 512),
	}
	for name, path := range invalid {
		t.Run(name, func(t *testing.T) {
			if validSignedPageview(path, testPageviewSession) {
				t.Fatalf("validSignedPageview(%q, %q) = true", path, testPageviewSession)
			}
		})
	}

	for name, session := range map[string]string{
		"missing session":   "",
		"email session":     "person@example.test",
		"uppercase UUID":    "550E8400-E29B-41D4-A716-446655440000",
		"noncanonical UUID": "550e8400-e29b-41d4-a716-44665544000",
		"UUID v1":           "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	} {
		t.Run(name, func(t *testing.T) {
			if validSignedPageview("/dashboard", session) {
				t.Fatalf("validSignedPageview with session %q = true", session)
			}
		})
	}
}

func TestSanitizeAnalyticsSignedPageviews(t *testing.T) {
	now := time.Now().UTC()
	enhanced := AnalyticsEvent{
		OccurredAt: now,
		Name:       "pageview",
		Props:      map[string]any{"session_id": testPageviewSession, "secret": "scrub-me"},
		Path:       "/servers/:id",
		SessionID:  testPageviewSession,
	}
	legacy := AnalyticsEvent{OccurredAt: now, Name: "signup", Props: map[string]any{"plan": "pro"}}

	t.Run("verified keeps enhanced and legacy while scrubbing props", func(t *testing.T) {
		out, dropped := sanitizeAnalytics([]AnalyticsEvent{enhanced, legacy}, now, true)
		if dropped != 0 || len(out) != 2 {
			t.Fatalf("got %d events and %d dropped, want 2 and 0", len(out), dropped)
		}
		if out[0].Props != nil {
			t.Fatalf("enhanced Props = %#v, want nil", out[0].Props)
		}
		if !reflect.DeepEqual(out[1].Props, legacy.Props) {
			t.Fatalf("legacy Props = %#v, want %#v", out[1].Props, legacy.Props)
		}
		if out[0].Path != enhanced.Path || out[0].SessionID != enhanced.SessionID {
			t.Fatalf("enhanced identity changed: %#v", out[0])
		}
	})

	t.Run("unsigned drops enhanced and retains legacy", func(t *testing.T) {
		out, dropped := sanitizeAnalytics([]AnalyticsEvent{enhanced, legacy}, now, false)
		if dropped != 1 || len(out) != 1 {
			t.Fatalf("got %d events and %d dropped, want 1 and 1", len(out), dropped)
		}
		if out[0].Name != legacy.Name || !reflect.DeepEqual(out[0].Props, legacy.Props) {
			t.Fatalf("legacy event changed: %#v", out[0])
		}
	})
}

func TestSanitizeAnalyticsMalformedEnhancedShape(t *testing.T) {
	now := time.Now().UTC()
	cases := []AnalyticsEvent{
		{OccurredAt: now, Name: "signup", Path: "/dashboard", SessionID: testPageviewSession},
		{OccurredAt: now, Name: "pageview", Path: "/dashboard"},
		{OccurredAt: now, Name: "signup", SessionID: testPageviewSession},
	}
	out, dropped := sanitizeAnalytics(cases, now, true)
	if len(out) != 0 || dropped != len(cases) {
		t.Fatalf("got %d events and %d dropped, want 0 and %d", len(out), dropped, len(cases))
	}
}
