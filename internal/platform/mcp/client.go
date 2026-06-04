package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// ErrServerError is returned for any non-2xx upstream response. The raw body is
// never surfaced to the caller — it is logged server-side only. Callers use
// errors.Is(err, ErrServerError).
var ErrServerError = errors.New("mcp: server error")

// ClientLike is the interface the MCP host depends on; both *Client and
// *MockClient satisfy it so the host is not coupled to the concrete transport.
type ClientLike interface {
	Initialize(ctx context.Context) error
	ListTools(ctx context.Context) ([]ToolDef, error)
	CallTool(ctx context.Context, name string, args json.RawMessage, idemHint string) (Result, error)
}

// Compile-time interface assertions (see mock.go for MockClient assertion).
var _ ClientLike = (*Client)(nil)

// ClientFactory builds a ClientLike from a server URL and auth header; inject at
// the host/executor level so production callers use the netsafe client and tests
// can inject a *MockClient without depending on the concrete *Client type.
type ClientFactory func(serverURL, authHeader string) ClientLike

// Client is a minimal MCP client over Streamable-HTTP (tools only). httpClient
// is injected: prod passes an SSRF-guarded netsafe client; tests pass an
// httptest client.
type Client struct {
	url        string
	authHeader string // e.g. "Bearer <token>"; "" => no Authorization header
	httpClient *http.Client
	sessionID  string
	nextID     int
}

// NewClient constructs a Client targeting serverURL. authHeader is sent as-is
// on every request (e.g. "Bearer <token>"); pass "" to omit. hc is the HTTP
// transport; pass nil to fall back to http.DefaultClient (callers SHOULD
// provide an SSRF-guarded client in production).
func NewClient(serverURL, authHeader string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{url: serverURL, authHeader: authHeader, httpClient: hc}
}

// Initialize sends the MCP initialize request, captures the Mcp-Session-Id
// response header, and fires the notifications/initialized notification.
func (c *Client) Initialize(ctx context.Context) error {
	type clientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type params struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      clientInfo     `json:"clientInfo"`
	}
	p := params{
		ProtocolVersion: "2025-06-18",
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "manyforge", Version: "0"},
	}

	respObj, sessionID, err := c.rpc(ctx, "initialize", p)
	if err != nil {
		return err
	}
	_ = respObj // we don't need the full initialize result beyond the session id
	c.sessionID = sessionID

	// Fire notifications/initialized — no id, no response expected.
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return err
	}
	return nil
}

// ListTools calls tools/list and returns the advertised tools.
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	respObj, _, err := c.rpc(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(respObj, &out); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list result: %w", err)
	}
	return out.Tools, nil
}

// CallTool calls tools/call with the given name, JSON arguments, and an
// optional idempotency hint. The _meta.idempotencyKey is omitted when idemHint
// is empty. Result.IsError mirrors the server's isError field; the caller
// decides how to handle tool-level errors versus transport errors.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage, idemHint string) (Result, error) {
	type meta struct {
		IdempotencyKey string `json:"idempotencyKey,omitempty"`
	}
	type params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      *meta           `json:"_meta,omitempty"`
	}
	p := params{Name: name, Arguments: args}
	if idemHint != "" {
		p.Meta = &meta{IdempotencyKey: idemHint}
	}

	respObj, _, err := c.rpc(ctx, "tools/call", p)
	if err != nil {
		return Result{}, err
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(respObj, &out); err != nil {
		return Result{}, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}

	var sb strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return Result{Content: sb.String(), IsError: out.IsError}, nil
}

// ----------------------------------------------------------------------------
// internal helpers
// ----------------------------------------------------------------------------

// rpcMsg is the JSON-RPC 2.0 envelope. Params is omitted when nil.
type rpcMsg struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is the JSON-RPC 2.0 response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpc sends a JSON-RPC request with a new id and returns the raw result bytes
// plus the Mcp-Session-Id from the response header (populated on initialize).
func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, string, error) {
	c.nextID++
	id := c.nextID
	msg := rpcMsg{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: marshal rpc request: %w", err)
	}

	sessionID, respBody, ct, err := c.post(ctx, body)
	if err != nil {
		return nil, sessionID, err
	}

	// Parse the JSON-RPC response — either from application/json directly or
	// from a text/event-stream body by scanning data: lines.
	var respObj rpcResponse
	if strings.HasPrefix(ct, "text/event-stream") {
		respObj, err = parseSSE(id, respBody)
	} else {
		err = json.Unmarshal(respBody, &respObj)
	}
	if err != nil {
		return nil, sessionID, fmt.Errorf("mcp: decode rpc response: %w", err)
	}

	if respObj.Error != nil {
		return nil, sessionID, fmt.Errorf("mcp: rpc error %d: %s: %w", respObj.Error.Code, respObj.Error.Message, ErrServerError)
	}
	return respObj.Result, sessionID, nil
}

// notify sends a JSON-RPC notification (no id, no response body expected).
func (c *Client) notify(ctx context.Context, method string, params any) error {
	msg := rpcMsg{
		JSONRPC: "2.0",
		// No ID field for notifications.
		Method: method,
		Params: params,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcp: marshal notification: %w", err)
	}
	// post drains and discards the body; for a 204 there is none.
	_, _, _, err = c.post(ctx, body)
	return err
}

// post executes the raw HTTP POST and returns (sessionIDHeader, responseBody,
// contentType, error). It applies io.LimitReader, never returns the upstream
// body on error, and asserts the response body for logging-only on non-2xx.
func (c *Client) post(ctx context.Context, body []byte) (sessionID string, respBody []byte, contentType string, err error) {
	u, parseErr := url.Parse(c.url)
	if parseErr != nil {
		return "", nil, "", fmt.Errorf("mcp: invalid server url: %w", parseErr)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", nil, "", fmt.Errorf("mcp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.authHeader != "" {
		httpReq.Header.Set("Authorization", c.authHeader)
	}
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", nil, "", fmt.Errorf("mcp: transport: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	capturedSession := res.Header.Get("Mcp-Session-Id")
	ct := res.Header.Get("Content-Type")

	if res.StatusCode == http.StatusNoContent {
		// Notifications expect 204 with no body.
		return capturedSession, nil, ct, nil
	}

	// Cap the read to 8 MiB regardless of status.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Log the status server-side; NEVER surface the body to the caller.
		slog.Error("mcp: upstream error", "status", res.StatusCode)
		return capturedSession, nil, ct, fmt.Errorf("mcp: upstream status %d: %w", res.StatusCode, ErrServerError)
	}

	return capturedSession, raw, ct, nil
}

// parseSSE scans a text/event-stream body for the JSON-RPC response whose id
// matches reqID. Lines that are not data: lines (comments, empty) are skipped.
// Unrelated events (different id) are ignored.
func parseSSE(reqID int, body []byte) (rpcResponse, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var obj rpcResponse
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue // skip unparseable lines
		}
		if obj.ID != nil && *obj.ID == reqID {
			return obj, nil
		}
	}
	return rpcResponse{}, fmt.Errorf("mcp: no SSE event matched id %d", reqID)
}
