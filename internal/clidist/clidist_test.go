package clidist

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newServer(t *testing.T, dir, base string) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	(&Handler{Dir: dir, BaseURL: base}).Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

func TestBinaryServesAllowlistedPlatform(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mf-linux-arm64"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, dir, "")

	resp, body := get(t, srv.URL+"/cli/mf/linux/arm64")
	if resp.StatusCode != http.StatusOK || body != "ELF" {
		t.Fatalf("got %d %q, want 200 ELF", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestBinaryRejectsUnknownPlatformsAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	// A file that exists but is not on the allowlist must never be served.
	if err := os.WriteFile(filepath.Join(dir, "mf-windows-amd64"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, dir, "")
	for _, p := range []string{"/cli/mf/windows/amd64", "/cli/mf/linux/amd64", "/cli/mf/..%2f..%2fetc/passwd"} {
		if resp, _ := get(t, srv.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestInstallScriptUsesConfiguredBase(t *testing.T) {
	srv := newServer(t, t.TempDir(), "https://hub.example.com/")
	resp, body := get(t, srv.URL+"/cli/install.sh")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, "base='https://hub.example.com'") {
		t.Fatalf("script missing quoted base:\n%s", body)
	}
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Fatalf("not a sh script")
	}
}

func TestInstallScriptQuotesHostileBase(t *testing.T) {
	s := InstallScript("https://x'; rm -rf ~; echo '")
	if !strings.Contains(s, `base='https://x'\''; rm -rf ~; echo '\'''`) {
		t.Fatalf("base not safely quoted:\n%s", s)
	}
}

func TestInstallScriptFallsBackToRequestHost(t *testing.T) {
	srv := newServer(t, t.TempDir(), "")
	_, body := get(t, srv.URL+"/cli/install.sh")
	if !strings.Contains(body, "base='"+srv.URL+"'") {
		t.Fatalf("script base should be request origin %s:\n%s", srv.URL, body)
	}
}
