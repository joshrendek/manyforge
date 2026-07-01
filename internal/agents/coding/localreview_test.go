package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	doc, in, out, err := localReview(context.Background(), srv.Client(), cred, payload, reviewInstructions, nil)
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

// TestLocalReview_RejectsDisallowedHost pins the SSRF guard behaviorally: a public host
// (no trust flag helps) and a private-LAN host WITHOUT the AllowPrivateBaseURL opt-in are
// both refused before any network call. The allow paths (loopback / trusted private) are
// covered exhaustively by TestLocalBaseURLBlocked.
func TestLocalReview_RejectsDisallowedHost(t *testing.T) {
	payload := "=== a.go ===\n@@ 1-1 @@\n    1 + x\n"
	// Public host — rejected regardless of the trust flag.
	pub := AICredential{BaseURL: "https://evil.example.com/v1", Model: "m", Provider: "ollama", APIKey: "k", AllowPrivateBaseURL: true}
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, pub, payload, reviewInstructions, nil); err == nil {
		t.Fatal("local review must reject a public base URL host even with AllowPrivateBaseURL (SSRF guard)")
	}
	// Private LAN WITHOUT the trust opt-in — rejected before dialing.
	priv := AICredential{BaseURL: "http://192.168.2.241:1234/v1", Model: "m", Provider: "vllm", APIKey: "k", AllowPrivateBaseURL: false}
	if _, _, _, err := localReview(context.Background(), http.DefaultClient, priv, payload, reviewInstructions, nil); err == nil {
		t.Fatal("local review must reject a private-LAN base URL host without AllowPrivateBaseURL")
	}
}

// TestLocalBaseURLBlocked exhaustively pins the local-review SSRF policy: loopback always
// allowed, private LAN gated on the trust opt-in, public/metadata/link-local always blocked.
func TestLocalBaseURLBlocked(t *testing.T) {
	cases := []struct {
		host         string
		allowPrivate bool
		wantBlocked  bool
	}{
		// Loopback — always permitted (a model on the same host is never an SSRF pivot).
		{"127.0.0.1", false, false},
		{"127.0.0.1", true, false},
		{"::1", false, false},
		{"localhost", false, false},
		{"LOCALHOST", false, false},
		// Private LAN (RFC1918 / IPv6 ULA) — only with the trust opt-in.
		{"192.168.2.241", true, false},
		{"10.0.0.5", true, false},
		{"172.16.3.4", true, false},
		{"fd00::1", true, false},
		{"192.168.2.241", false, true},
		{"10.0.0.5", false, true},
		// Public — always blocked.
		{"8.8.8.8", true, true},
		{"93.184.216.34", false, true},
		// Cloud-metadata + link-local — blocked even under full trust.
		{"169.254.169.254", true, true},
		{"fd00:ec2::254", true, true},
		{"169.254.1.1", true, true},
		// Non-IP hostnames other than localhost — blocked (we don't resolve them here).
		{"evil.example.com", true, true},
		{"", true, true},
	}
	for _, c := range cases {
		if got := localBaseURLBlocked(c.host, c.allowPrivate); got != c.wantBlocked {
			t.Errorf("localBaseURLBlocked(%q, allowPrivate=%v) = %v, want %v", c.host, c.allowPrivate, got, c.wantBlocked)
		}
	}
}

// TestLocalReview_RetriesWithoutSchemaOnEmptyStream pins the LM-Studio fallback: the first
// attempt sends response_format=json_schema (Ollama needs it); when that yields an EMPTY
// stream (LM Studio's behavior under json_schema), localReview retries ONCE without the
// constraint, and the plain-mode fenced JSON then parses.
func TestLocalReview_RetriesWithoutSchemaOnEmptyStream(t *testing.T) {
	var sawSchema []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasSchema := strings.Contains(string(body), `"response_format"`)
		sawSchema = append(sawSchema, hasSchema)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if hasSchema {
			// json_schema attempt → empty stream (no delta.content), like LM Studio.
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
			return
		}
		// Plain retry → valid findings wrapped in a ```json fence (LM Studio plain output).
		fenced := "```json\n" + `{"summary":"ok","findings":[{"file":"a.go","line":1,"severity":"warning","title":"t","detail":"d"}]}` + "\n```"
		frame, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": fenced}}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith-1.0-9b", Provider: "vllm", APIKey: "lmstudio"}
	doc, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil)
	if err != nil {
		t.Fatalf("localReview: %v", err)
	}
	if len(sawSchema) != 2 || !sawSchema[0] || sawSchema[1] {
		t.Fatalf("sawSchema=%v, want [true false] (json_schema first, then a plain retry)", sawSchema)
	}
	if doc.Summary != "ok" || len(doc.Findings) != 1 || doc.Findings[0].Severity != "warning" {
		t.Fatalf("doc=%+v — fenced JSON from the plain retry must parse", doc)
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
	doc, _, out, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, prog)
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
	if _, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil); err != nil {
		t.Fatalf("localReview: %v", err)
	}
	for _, want := range []string{`"response_format"`, `"json_schema"`, "code_review_findings", `"severity"`} {
		if !strings.Contains(string(gotBody), want) {
			t.Fatalf("request body missing %q — JSON output not enforced (manyforge-6ax)\nbody: %s", want, string(gotBody))
		}
	}
}

// TestLocalReview_UsesProvidedPrompt pins spec 008 per-dimension prompt plumbing: the
// prompt localReview is given (a dimension's instructions) becomes the model's system
// message — REPLACING the default reviewInstructions — with reviewSchemaLine still appended
// so the JSON-only output contract holds regardless of which dimension's prompt is used.
func TestLocalReview_UsesProvidedPrompt(t *testing.T) {
	const sentinel = "SENTINEL_DIMENSION_PROMPT_review_only_widgets"
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &b)
		for _, m := range b.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
		}
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

	cred := AICredential{BaseURL: srv.URL, Model: "m", Provider: "ollama", APIKey: "x"}
	if _, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", sentinel, nil); err != nil {
		t.Fatalf("localReview: %v", err)
	}
	if !strings.Contains(gotSystem, sentinel) {
		t.Fatalf("system message must use the provided dimension prompt; got %q", gotSystem)
	}
	if !strings.Contains(gotSystem, reviewSchemaLine) {
		t.Fatalf("reviewSchemaLine must still be appended so JSON-only output holds; got %q", gotSystem)
	}
	if strings.Contains(gotSystem, "Surface every plausible") {
		t.Fatalf("default reviewInstructions must be REPLACED by the provided prompt, not present; got %q", gotSystem)
	}
}

// TestLocalReview_LogsDroppedFrames pins that an unparseable stream frame is counted
// and logged (not silently dropped), while valid content after it still parses —
// addressing the gemini-flagged silent-failure risk in the SSE loop.
func TestLocalReview_LogsDroppedFrames(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {not valid json\n\n")) // malformed frame — must be logged, not silently dropped
		good, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": `{"summary":"ok","findings":[]}`}}}})
		_, _ = w.Write([]byte("data: " + string(good) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith:9b", Provider: "ollama", APIKey: "x"}
	doc, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil)
	if err != nil {
		t.Fatalf("localReview: %v", err)
	}
	if doc.Summary != "ok" {
		t.Fatalf("valid content after a dropped frame must still parse; doc=%+v", doc)
	}
	if !strings.Contains(logBuf.String(), "dropped unparseable stream frames") {
		t.Fatalf("a dropped frame must be logged (not silent); log=%q", logBuf.String())
	}
}

// TestLocalReview_ReasoningContentNotParsedButPreviewed pins reasoning-model handling:
// delta.reasoning_content (chain-of-thought) is surfaced in the live preview but NEVER
// parsed as findings — only delta.content is. A decoy findings object placed in the
// reasoning stream must be ignored; the real findings come from content.
func TestLocalReview_ReasoningContentNotParsedButPreviewed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		// Reasoning stream carries a DECOY findings object that must NOT be parsed.
		write(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"reasoning_content": `Thinking... maybe {"summary":"DECOY","findings":[]} — but let me check the real code`}}}})
		// The final answer arrives in content.
		write(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": `{"summary":"real","findings":[{"file":"a.go","line":2,"severity":"error","title":"t","detail":"d"}]}`}}}})
		write(map[string]any{"choices": []map[string]any{}, "usage": map[string]int64{"prompt_tokens": 10, "completion_tokens": 20}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith-1.0-9b", Provider: "vllm", APIKey: "lmstudio"}
	prog := &Progress{}
	prog.SetPhase("reviewing")
	doc, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, prog)
	if err != nil {
		t.Fatalf("localReview: %v", err)
	}
	if doc.Summary != "real" || len(doc.Findings) != 1 {
		t.Fatalf("findings must come from content, not the reasoning decoy; doc=%+v", doc)
	}
	var s progressSnapshot
	_ = json.Unmarshal(prog.Snapshot(), &s)
	if !strings.Contains(s.Preview, "real") {
		t.Fatalf("preview should show the final content once it arrives; got %q", s.Preview)
	}
}

// TestLocalReview_LogsTruncationOnLengthFinish pins that a finish_reason=="length" cutoff
// (truncated, unparseable JSON) is logged with its real cause — not left as a mysterious
// ParseFindings failure.
func TestLocalReview_LogsTruncationOnLengthFinish(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Partial (truncated) JSON, then the model reports it was cut at the token limit.
		frame, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": `{"summary":"partial","findings":[`}, "finish_reason": "length"}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith-1.0-9b", Provider: "vllm", APIKey: "lmstudio"}
	// Truncated JSON → ParseFindings fails (expected); we assert the cause was logged.
	if _, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil); err == nil {
		t.Fatal("truncated JSON should fail to parse")
	}
	if !strings.Contains(logBuf.String(), "truncated at token limit") {
		t.Fatalf("a length-finish truncation must be logged with its cause; log=%q", logBuf.String())
	}
}

// TestLocalReview_RetriesPlainOnUnparseable pins manyforge-87a: a local model's plain
// output is non-deterministic and occasionally malformed (e.g. an unescaped quote from an
// embedded code snippet). localReview retries plain generation IN-LINE — nudging the
// temperature up so the retry actually varies — and returns the first output that parses,
// instead of failing the whole job (which would trigger an expensive worker re-clone).
func TestLocalReview_RetriesPlainOnUnparseable(t *testing.T) {
	var plainTemps []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		emit := func(content string) {
			if content != "" {
				frame, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": content}}}})
				_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		if strings.Contains(string(body), `"response_format"`) {
			emit("") // json_schema attempt → empty, like LM Studio
			return
		}
		temp, _ := b["temperature"].(float64)
		plainTemps = append(plainTemps, temp)
		if len(plainTemps) == 1 {
			emit(`{"summary":"s" "findings":[]}`) // malformed (missing comma) → ParseFindings fails
			return
		}
		emit(`{"summary":"ok","findings":[{"file":"a.go","line":1,"severity":"warning","title":"t","detail":"d"}]}`)
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith-1.0-9b", Provider: "vllm", APIKey: "lmstudio"}
	doc, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil)
	if err != nil {
		t.Fatalf("localReview should recover via in-line retry: %v", err)
	}
	if doc.Summary != "ok" || len(doc.Findings) != 1 {
		t.Fatalf("expected the retry's parseable output; doc=%+v", doc)
	}
	if len(plainTemps) < 2 {
		t.Fatalf("expected >=2 plain attempts (retry on unparseable), got %d", len(plainTemps))
	}
	if plainTemps[0] != 0 || plainTemps[1] <= plainTemps[0] {
		t.Fatalf("retry temperature must increase to vary the output: %v", plainTemps)
	}
}

// TestLocalReview_FailsAfterMaxPlainAttempts pins that when every in-line plain retry is
// unparseable, localReview gives up (returning the parse error) rather than looping forever.
func TestLocalReview_FailsAfterMaxPlainAttempts(t *testing.T) {
	var plainAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if strings.Contains(string(body), `"response_format"`) {
			_, _ = w.Write([]byte("data: [DONE]\n\n")) // schema empty
			if fl != nil {
				fl.Flush()
			}
			return
		}
		plainAttempts++
		frame, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": `{"summary":"s" bad}`}}}})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cred := AICredential{BaseURL: srv.URL, Model: "ornith-1.0-9b", Provider: "vllm", APIKey: "lmstudio"}
	if _, _, _, err := localReview(context.Background(), srv.Client(), cred, "=== a.go ===\n@@ 1-1 @@\n    1 + x\n", reviewInstructions, nil); err == nil {
		t.Fatal("localReview should fail when every plain retry is unparseable")
	}
	if plainAttempts != localReviewMaxPlainAttempts {
		t.Fatalf("expected exactly %d plain attempts, got %d", localReviewMaxPlainAttempts, plainAttempts)
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
		{Path: "config.yaml", Patch: "@@ -1,0 +1,1 @@\n+key: v\n"}, // non-code but reviewable → sorts after code
		{Path: "a.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+added\n"},   // code → first
		{Path: "bin.png", Patch: ""},                              // no patch → skipped
	}
	payload, skipped, omitted, filtered := assembleDiffPayload(files, reviewMaxTotalBytes)
	if len(skipped) != 1 || skipped[0] != "bin.png" {
		t.Fatalf("skipped=%v, want [bin.png]", skipped)
	}
	if len(omitted) != 0 {
		t.Fatalf("omitted=%v, want none", omitted)
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered=%v, want none (no docs in this set)", filtered)
	}
	ia, ic := strings.Index(payload, "=== a.go ==="), strings.Index(payload, "=== config.yaml ===")
	if ia < 0 || ic < 0 {
		t.Fatalf("payload missing a file header:\n%s", payload)
	}
	if ia > ic {
		t.Fatalf("code file must come before non-code; a.go@%d config.yaml@%d", ia, ic)
	}
	if !strings.Contains(payload, "added") {
		t.Fatalf("payload missing hunk content:\n%s", payload)
	}
}

func TestAssembleDiffPayload_OmitsOverBudget(t *testing.T) {
	big := "@@ -1,0 +1,1 @@\n+" + strings.Repeat("x", reviewMaxTotalBytes) + "\n"
	_, _, omitted, _ := assembleDiffPayload([]connectors.ChangedFile{{Path: "big.go", Patch: big}}, reviewMaxTotalBytes)
	if len(omitted) != 1 || omitted[0] != "big.go" {
		t.Fatalf("omitted=%v, want [big.go]", omitted)
	}
}

func TestIsNonReviewableDoc(t *testing.T) {
	for _, p := range []string{
		"docs/superpowers/plans/2026-06-30-frontend-performance.md",
		"README.md", "notes.markdown", "guide.mdx", "spec.rst", "x.adoc",
		".beads/issues.jsonl", "frontend/docs/guide.md",
	} {
		if !isNonReviewableDoc(p) {
			t.Errorf("%q should be non-reviewable (prose/plan/tracker)", p)
		}
	}
	for _, p := range []string{
		"internal/agents/coding/service.go", "frontend/src/app/app.component.ts",
		"config.yaml", "Dockerfile", "deploy/values.json", "scripts/run.sh",
		"docsite.go", // "docs" is a filename substring, not a path segment → reviewable
	} {
		if isNonReviewableDoc(p) {
			t.Errorf("%q should be reviewable code/config", p)
		}
	}
}

func TestAssembleDiffPayload_FiltersDocs(t *testing.T) {
	files := []connectors.ChangedFile{
		{Path: "internal/svc.go", Patch: "@@ -1,1 +1,2 @@\n ctx\n+code\n"},
		{Path: "docs/plans/perf.md", Patch: "@@ -1,0 +1,1 @@\n+# big plan doc\n"},
		{Path: ".beads/issues.jsonl", Patch: "@@ -1,0 +1,1 @@\n+{}\n"},
		{Path: "config.yaml", Patch: "@@ -1,0 +1,1 @@\n+k: v\n"},
	}
	payload, _, _, filtered := assembleDiffPayload(files, reviewMaxTotalBytes)
	if len(filtered) != 2 {
		t.Fatalf("filtered=%v, want the .md plan + .beads tracker", filtered)
	}
	if strings.Contains(payload, "perf.md") || strings.Contains(payload, "issues.jsonl") {
		t.Fatalf("prose/plan/tracker docs must NOT reach the review payload:\n%s", payload)
	}
	if !strings.Contains(payload, "=== internal/svc.go ===") || !strings.Contains(payload, "=== config.yaml ===") {
		t.Fatalf("reviewable code/config must be in the payload:\n%s", payload)
	}
}

func TestCommentableMap(t *testing.T) {
	files := []connectors.ChangedFile{{Path: "a.go", Commentable: map[int]bool{3: true}}}
	m := commentableMap(files)
	if !m["a.go"][3] {
		t.Fatalf("commentableMap dropped a.go:3: %v", m)
	}
}
