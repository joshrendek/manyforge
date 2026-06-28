package coding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
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

func TestAssembleDiffPayload(t *testing.T) {
	files := []connectors.ChangedFile{
		{Path: "doc.md", Patch: "@@ -1,0 +1,1 @@\n+hello\n"},     // non-code → sorts last
		{Path: "a.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+added\n"}, // code → first
		{Path: "bin.png", Patch: ""},                             // no patch → skipped
	}
	payload, skipped, omitted := assembleDiffPayload(files)
	if len(skipped) != 1 || skipped[0] != "bin.png" {
		t.Fatalf("skipped=%v, want [bin.png]", skipped)
	}
	if len(omitted) != 0 {
		t.Fatalf("omitted=%v, want none", omitted)
	}
	ia, id := strings.Index(payload, "=== a.go ==="), strings.Index(payload, "=== doc.md ===")
	if ia < 0 || id < 0 {
		t.Fatalf("payload missing a file header:\n%s", payload)
	}
	if ia > id {
		t.Fatalf("code file must come before non-code; a.go@%d doc.md@%d", ia, id)
	}
	if !strings.Contains(payload, "added") {
		t.Fatalf("payload missing hunk content:\n%s", payload)
	}
}

func TestAssembleDiffPayload_OmitsOverBudget(t *testing.T) {
	big := "@@ -1,0 +1,1 @@\n+" + strings.Repeat("x", localReviewMaxTotalBytes) + "\n"
	_, _, omitted := assembleDiffPayload([]connectors.ChangedFile{{Path: "big.go", Patch: big}})
	if len(omitted) != 1 || omitted[0] != "big.go" {
		t.Fatalf("omitted=%v, want [big.go]", omitted)
	}
}

func TestCommentableMap(t *testing.T) {
	files := []connectors.ChangedFile{{Path: "a.go", Commentable: map[int]bool{3: true}}}
	m := commentableMap(files)
	if !m["a.go"][3] {
		t.Fatalf("commentableMap dropped a.go:3: %v", m)
	}
}
