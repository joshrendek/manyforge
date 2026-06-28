package coding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		content := `{"summary":"ok","findings":[{"file":"service.go","line":3,"severity":" Warning ","title":"t","detail":"d"}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
			"usage":   map[string]int64{"prompt_tokens": 1200, "completion_tokens": 80},
		})
	}))
	defer srv.Close()

	// httptest serves on 127.0.0.1 → passes the loopback gate.
	cred := AICredential{BaseURL: srv.URL, Model: "qwen2.5-coder:14b", Provider: "ollama", APIKey: "ollama"}
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, map[string]string{"service.go": "package x"})
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if doc.Summary != "ok" || len(doc.Findings) != 1 || doc.Findings[0].Severity != "warning" {
		t.Fatalf("doc=%+v", doc)
	}
	if in != 1200 || out != 80 {
		t.Fatalf("tokens in=%d out=%d, want 1200/80", in, out)
	}
}

func TestLocalReview_RejectsNonLoopback(t *testing.T) {
	cred := AICredential{BaseURL: "https://evil.example.com/v1", Model: "m", Provider: "ollama", APIKey: "k"}
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, cred, map[string]string{"a.go": "x"}); err == nil {
		t.Fatal("local review must reject a non-loopback base URL (SSRF guard)")
	}
}

func TestReadChangedFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "cmd"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "cmd", "main.go"), []byte("package main"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "empty.go"), []byte(""), 0o644)
	// Includes path-traversal + absolute + missing entries that must be skipped.
	files := readChangedFiles(dir, []string{"cmd/main.go", "empty.go", "../escape.go", "/etc/passwd", "missing.go"})
	if len(files) != 1 || files["cmd/main.go"] != "package main" {
		t.Fatalf("files=%v", files)
	}
}

func TestIsLocalProvider(t *testing.T) {
	for _, p := range []string{"ollama", "vllm"} {
		if !isLocalProvider(p) {
			t.Fatalf("%q should be local", p)
		}
	}
	for _, p := range []string{"openrouter", "anthropic", "openai"} {
		if isLocalProvider(p) {
			t.Fatalf("%q should NOT be local", p)
		}
	}
}
