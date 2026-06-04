package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"` // nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func rpcOK(id int, result interface{}) string {
	r, _ := json.Marshal(result)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, r)
}

// newTestServer returns an httptest.Server that speaks JSON-RPC 2.0.
// It asserts that: the session id is echoed on every non-initialize call,
// and records every request body for inspection.
func newTestServer(t *testing.T) (*httptest.Server, *[]rpcReq, *string) {
	t.Helper()
	var reqs []rpcReq
	sessionID := "sess-test-42"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") {
			t.Errorf("Accept = %q, must include application/json", accept)
		}

		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		reqs = append(reqs, req)

		// Notifications have no id — just 204 and no body.
		if req.ID == nil {
			// Verify session id is echoed on notifications too.
			if got := r.Header.Get("Mcp-Session-Id"); got != sessionID {
				t.Errorf("notification: Mcp-Session-Id = %q, want %q", got, sessionID)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		id := *req.ID

		switch req.Method {
		case "initialize":
			// session id established here; NOT required on this first call.
			w.Header().Set("Mcp-Session-Id", sessionID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, rpcOK(id, map[string]interface{}{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]interface{}{},
				"serverInfo":      map[string]interface{}{"name": "testserver", "version": "0"},
			}))

		case "tools/list":
			// session id must be echoed.
			if got := r.Header.Get("Mcp-Session-Id"); got != sessionID {
				t.Errorf("tools/list: Mcp-Session-Id = %q, want %q", got, sessionID)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, rpcOK(id, map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name":        "echo",
						"description": "echoes input",
						"inputSchema": map[string]interface{}{"type": "object"},
					},
					map[string]interface{}{
						"name":        "fail_tool",
						"description": "always errors",
						"inputSchema": map[string]interface{}{"type": "object"},
					},
				},
			}))

		case "tools/call":
			// session id must be echoed.
			if got := r.Header.Get("Mcp-Session-Id"); got != sessionID {
				t.Errorf("tools/call: Mcp-Session-Id = %q, want %q", got, sessionID)
			}
			// Decode params to decide which tool.
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      struct {
					IdempotencyKey string `json:"idempotencyKey"`
				} `json:"_meta"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				t.Errorf("decode tools/call params: %v", err)
				http.Error(w, "bad params", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			switch p.Name {
			case "echo":
				_, _ = fmt.Fprint(w, rpcOK(id, map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "echoed"},
					},
					"isError": false,
				}))
			case "fail_tool":
				_, _ = fmt.Fprint(w, rpcOK(id, map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "it broke"},
					},
					"isError": true,
				}))
			default:
				_, _ = fmt.Fprint(w, rpcOK(id, map[string]interface{}{
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "unknown tool"},
					},
					"isError": true,
				}))
			}

		default:
			t.Errorf("unexpected method %q", req.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))

	return srv, &reqs, &sessionID
}

// ----------------------------------------------------------------------------
// tests
// ----------------------------------------------------------------------------

func TestClient_Initialize_CapturesSessionID(t *testing.T) {
	srv, reqs, wantSessID := newTestServer(t)
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.sessionID != *wantSessID {
		t.Errorf("sessionID = %q, want %q", c.sessionID, *wantSessID)
	}

	// Requests: initialize + notifications/initialized notification.
	if len(*reqs) < 2 {
		t.Fatalf("want at least 2 requests (initialize + notification), got %d", len(*reqs))
	}
	if (*reqs)[0].Method != "initialize" {
		t.Errorf("first request method = %q, want initialize", (*reqs)[0].Method)
	}
	if (*reqs)[1].Method != "notifications/initialized" {
		t.Errorf("second request method = %q, want notifications/initialized", (*reqs)[1].Method)
	}
	// Notification must have no id.
	if (*reqs)[1].ID != nil {
		t.Errorf("notification must have no id, got %v", (*reqs)[1].ID)
	}
}

func TestClient_ListTools(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "echo" || tools[0].Description != "echoes input" {
		t.Errorf("tool[0] = %+v", tools[0])
	}
	if tools[1].Name != "fail_tool" {
		t.Errorf("tool[1] = %+v", tools[1])
	}
}

func TestClient_CallTool_HappyPath(t *testing.T) {
	srv, reqs, _ := newTestServer(t)
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	args := json.RawMessage(`{"msg":"hello"}`)
	res, err := c.CallTool(context.Background(), "echo", args, "idem-key-1")
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Content != "echoed" {
		t.Errorf("Content = %q, want %q", res.Content, "echoed")
	}
	if res.IsError {
		t.Errorf("IsError should be false")
	}

	// Verify that the tools/call request included the idempotency key.
	var callReq *rpcReq
	for i := range *reqs {
		if (*reqs)[i].Method == "tools/call" {
			callReq = &(*reqs)[i]
			break
		}
	}
	if callReq == nil {
		t.Fatal("no tools/call request found")
	}
	var p struct {
		Name string `json:"name"`
		Meta struct {
			IdemKey string `json:"idempotencyKey"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(callReq.Params, &p); err != nil {
		t.Fatalf("decode call params: %v", err)
	}
	if p.Name != "echo" {
		t.Errorf("params.name = %q, want echo", p.Name)
	}
	if p.Meta.IdemKey != "idem-key-1" {
		t.Errorf("_meta.idempotencyKey = %q, want idem-key-1", p.Meta.IdemKey)
	}
}

func TestClient_CallTool_IsError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.CallTool(context.Background(), "fail_tool", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("CallTool: unexpected error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError should be true")
	}
	if res.Content != "it broke" {
		t.Errorf("Content = %q, want %q", res.Content, "it broke")
	}
}

func TestClient_Non200_ReturnsErrServerError_NoBodyLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"secret":"this must not leak"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("want error on non-200, got nil")
	}
	if !errors.Is(err, ErrServerError) {
		t.Errorf("want ErrServerError, got %v", err)
	}
	// The raw upstream body must never appear in the error message.
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "this must not leak") {
		t.Errorf("upstream body leaked into error: %v", err)
	}
}

func TestClient_AuthHeader(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		var req rpcReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			// Notification: no session header echo needed for this test.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		id := *req.ID
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, id)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "Bearer tok-abc", srv.Client())
	// We only need to check the first request (initialize).
	_ = c.Initialize(context.Background())

	if auth, ok := gotAuth.Load().(string); !ok || auth != "Bearer tok-abc" {
		t.Errorf("Authorization header = %q, want %q", gotAuth.Load(), "Bearer tok-abc")
	}
}

// TestClient_SSEResponse tests that CallTool can parse an SSE (text/event-stream)
// response, picking the JSON-RPC object whose id matches.
func TestClient_SSEResponse(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sse-sess")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			id := *req.ID
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, id)
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
		case "tools/call":
			callCount++
			id := *req.ID
			// Respond as SSE stream with a leading unrelated event and then the
			// matching result.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Unrelated event (different id).
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"wrong\"}],\"isError\":false}}\n\n", id+999)
			// Matching event.
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"sse-result\"}],\"isError\":false}}\n\n", id)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.CallTool(context.Background(), "any", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("CallTool SSE: %v", err)
	}
	if res.Content != "sse-result" {
		t.Errorf("SSE content = %q, want sse-result", res.Content)
	}
}
