package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderGolden(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.RenderInput(Input{
		FromName: "Acme & Sons", Preheader: "A small update",
		PostalAddress: "10 Main St <Suite 2>",
		BodyMarkdown:  "# Hello {{first_name}}\n\nVisit [our site](https://example.com/a?q=1), [email us](mailto:hi@example.com), or [bad](javascript:alert(1)).\n\n<div>raw html</div>",
	}, Variables{
		FirstName: `<img src=x onerror=alert(1)>`, Email: "reader@example.com",
		UnsubscribeURL: "https://hub.example/m/u/token", ListName: "Updates",
	}, Tracking{
		ClickURL: func(destination string) (string, error) {
			return "https://hub.example/m/c/token?to=" + destination, nil
		},
		OpenURL: "https://hub.example/m/o/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "message.html.golden", out.HTML)
	assertGolden(t, "message.txt.golden", out.Text+"\n")
}

func TestRenderSafetyAndTrackingToggles(t *testing.T) {
	r, _ := New()
	out, err := r.RenderInput(Input{BodyMarkdown: `[web](https://example.com) [mail](mailto:a@example.com) [anchor](#top) {{unknown}}`}, Variables{UnsubscribeURL: "#"}, Tracking{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="https://example.com"`, `href="mailto:a@example.com"`, `href="#top"`} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("HTML missing %s", want)
		}
	}
	if strings.Contains(out.HTML, "/m/o/") || strings.Contains(out.HTML, "{{unknown}}") {
		t.Fatalf("tracking/unknown variable leaked into preview: %s", out.HTML)
	}
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s; run UPDATE_GOLDEN=1 go test ./internal/mailing/render", name)
	}
}
