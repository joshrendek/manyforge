package coding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestLocalReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		content := `{"summary":"ok","findings":[{"file":"service.go","line":3,"severity":" Warning ","title":"t","detail":"d"}]}`
		writeFrame := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		// Stream the content split across two delta frames.
		writeFrame(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content[:25]}}}})
		writeFrame(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content[25:]}}}})
		// Terminal usage frame (only sent because stream_options.include_usage=true).
		writeFrame(map[string]any{"choices": []map[string]any{}, "usage": map[string]int64{"prompt_tokens": 1200, "completion_tokens": 80}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "qwen2.5-coder:14b", Provider: "ollama", APIKey: "ollama"}
	payload := "\n=== service.go ===\n@@ 1-1 @@\n    1 + package x\n"
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, payload, nil)
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
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", nil); err == nil {
		t.Fatal("local review must reject a non-loopback base URL (SSRF guard)")
	}
}

func TestLocalReview_StreamUpdatesProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		content := `{"summary":"streamed","findings":[]}`
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content}}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "m", Provider: "ollama", APIKey: "ollama"}
	prog := &Progress{}
	prog.SetPhase("reviewing") // worker sets this in prod; needed so Snapshot is non-nil
	doc, _, out, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", prog)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if doc.Summary != "streamed" {
		t.Fatalf("doc=%+v", doc)
	}
	if out == 0 {
		t.Fatal("completion tokens must fall back to streamed-chunk count when usage absent")
	}
	snap := prog.Snapshot()
	if snap == nil {
		t.Fatal("expected progress snapshot after streaming")
	}
	var s progressSnapshot
	_ = json.Unmarshal(snap, &s)
	if !strings.Contains(s.Preview, "streamed") {
		t.Fatalf("preview missing streamed content: %q", s.Preview)
	}
}

// TestLocalReview_SendsJSONSchemaResponseFormat pins that localReview constrains the
// model output with a json_schema response_format (manyforge-6ax) — without it, chatty
// models emit prose and ParseFindings fails, retrying to terminal failure.
func TestLocalReview_SendsJSONSchemaResponseFormat(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frame, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": `{"summary":"ok","findings":[]}`}}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith:9b", Provider: "ollama", APIKey: "x"}
	if _, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", nil); err != nil {
		t.Fatalf("localReview: %v", err)
	}
	for _, want := range []string{`"response_format"`, `"json_schema"`, "code_review_findings", `"severity"`} {
		if !strings.Contains(string(gotBody), want) {
			t.Fatalf("request body missing %q — JSON output not enforced (manyforge-6ax)\nbody: %s", want, string(gotBody))
		}
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
