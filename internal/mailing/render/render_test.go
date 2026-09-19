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

func TestRenderBrandedGolden(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.RenderInput(Input{
		FromName: "Acme & Sons", Preheader: "A small update",
		PostalAddress: "10 Main St <Suite 2>",
		BodyMarkdown:  "# Hello {{first_name}}\n\nVisit [our site](https://example.com/a?q=1).",
		Brand: Brand{
			Name:      "Acme <Brand>",
			LogoURL:   "https://hub.example/m/b/0b9f3f2e-1c3a-4d8f-9a1e-6f2d3c4b5a60/logo?v=0123456789abcdef",
			LogoWidth: 200,
			Colors: Colors{
				Background: "#101418", Surface: "#1b2229", Text: "#e6edf3",
				Accent: "#ffb454", HeaderBackground: "#0b0e11", HeaderText: "#ffffff",
			},
			FontStack:      FontStackSerif,
			FooterMarkdown: "You get this because you joined [Acme](https://example.com/why).\n\n<script>alert(1)</script>",
		},
	}, Variables{
		FirstName: "Ada", Email: "reader@example.com",
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
	assertGolden(t, "branded.html.golden", out.HTML)
	assertGolden(t, "branded.txt.golden", out.Text+"\n")
}

func TestRenderBrandFallbacks(t *testing.T) {
	r, _ := New()
	out, err := r.RenderInput(Input{
		FromName:     "Acme & Sons",
		BodyMarkdown: "hi",
		Brand: Brand{
			Colors:    Colors{Accent: "red", Background: "#ABCDEF", Surface: "#123456"},
			FontStack: "comic",
		},
	}, Variables{UnsubscribeURL: "#"}, Tracking{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<div class="brand" style="background:#ffffff;color:#17212b">Acme &amp; Sons</div>`,
		`.content a { color: #1769aa; }`,
		`bgcolor="#f4f6f8"`,
		`background:#123456`,
		`font:16px/1.55 system-ui,`,
	} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("HTML missing %q in:\n%s", want, out.HTML)
		}
	}
	for _, reject := range []string{"red", "#ABCDEF", "ZgotmplZ", "<img"} {
		if strings.Contains(out.HTML, reject) {
			t.Errorf("HTML unexpectedly contains %q in:\n%s", reject, out.HTML)
		}
	}
}

func TestRenderBrandFooterIsSafeAndTracked(t *testing.T) {
	r, _ := New()
	out, err := r.RenderInput(Input{
		FromName:     "Acme",
		BodyMarkdown: "hi",
		Brand: Brand{
			Name:           "Brand",
			FooterMarkdown: "<script>alert(1)</script>\n\n[why](https://example.com/why) [evil](javascript:alert(1))",
		},
	}, Variables{UnsubscribeURL: "https://hub.example/m/u/token"}, Tracking{
		ClickURL: func(destination string) (string, error) {
			return "https://hub.example/m/c/token?to=" + destination, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTML, "<script") || strings.Contains(out.HTML, "javascript:") {
		t.Fatalf("raw footer HTML leaked: %s", out.HTML)
	}
	for _, want := range []string{
		`<div class="footer-custom">`,
		`href="https://hub.example/m/c/token?to=https://example.com/why"`,
		`href="https://hub.example/m/u/token"`,
		`<div class="brand" style="background:#ffffff;color:#17212b">Brand</div>`,
		`Unsubscribe from Acme emails`,
	} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("HTML missing %q in:\n%s", want, out.HTML)
		}
	}
}

func TestRenderNoFooterWhenBrandFooterEmpty(t *testing.T) {
	r, _ := New()
	out, err := r.RenderInput(Input{FromName: "Acme", BodyMarkdown: "hi"}, Variables{UnsubscribeURL: "#"}, Tracking{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTML, "footer-custom\">") {
		t.Fatalf("empty footer markdown emitted a footer block: %s", out.HTML)
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
