package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// OpenAI-compatible chat/completions wire format. Covers OpenAI, Ollama, and
// vLLM (all expose /chat/completions). Hand-rolled, no vendor SDK.

type openAIReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
}

type openAITool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"` // system|user|assistant|tool
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // role:tool only
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON encoded as a STRING
	} `json:"function"`
}

// buildOpenAIRequest maps the common Request onto the chat/completions wire
// format. model overrides req.Model when req.Model is empty.
func buildOpenAIRequest(req Request, model string) openAIReq {
	out := openAIReq{Model: model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	for _, td := range req.Tools {
		var t openAITool
		t.Type = "function"
		t.Function.Name = td.Name
		t.Function.Description = td.Description
		t.Function.Parameters = td.Schema
		out.Tools = append(out.Tools, t)
	}
	if req.System != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: m.Text})
		case RoleTool:
			for _, tr := range m.ToolResults {
				out.Messages = append(out.Messages, openAIMessage{
					Role: "tool", ToolCallID: tr.CallID, Content: tr.Content,
				})
			}
		case RoleAssistant:
			msg := openAIMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				var otc openAIToolCall
				otc.ID = tc.ID
				otc.Type = "function"
				otc.Function.Name = tc.Name
				otc.Function.Arguments = string(tc.Args) // object -> JSON string
				msg.ToolCalls = append(msg.ToolCalls, otc)
			}
			out.Messages = append(out.Messages, msg)
		default: // RoleUser
			out.Messages = append(out.Messages, openAIMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}

type openAIResp struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// parseOpenAIResponse maps a 200 chat/completions body onto the common Response.
func parseOpenAIResponse(body []byte) (Response, error) {
	var or openAIResp
	if err := json.Unmarshal(body, &or); err != nil {
		return Response{}, err
	}
	if len(or.Choices) == 0 {
		return Response{}, errors.New("ai/openai: no choices in response")
	}
	ch := or.Choices[0]
	out := Response{
		Text:         ch.Message.Content,
		FinishReason: openAIFinish(ch.FinishReason),
		Usage:        Usage{InputTokens: or.Usage.PromptTokens, OutputTokens: or.Usage.CompletionTokens},
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}

func openAIFinish(raw string) FinishReason {
	switch raw {
	case "stop":
		return FinishStop
	case "tool_calls":
		return FinishToolUse
	case "length":
		return FinishLength
	default:
		return FinishOther
	}
}

// OpenAICompatProvider talks to any OpenAI-compatible /chat/completions endpoint
// (OpenAI, Ollama, vLLM). baseURL is USER-SUPPLIED for self-host, so production
// callers MUST inject an SSRF-guarded netsafe client (see factory.New).
type OpenAICompatProvider struct {
	apiKey     string
	baseURL    string // includes the version segment, e.g. https://api.openai.com/v1
	model      string
	httpClient *http.Client
}

// NewOpenAICompatProvider builds a provider. hc nil -> http.DefaultClient
// (callers SHOULD pass netsafe.NewClient or a test client). An empty apiKey
// omits the Authorization header (Ollama / vLLM).
func NewOpenAICompatProvider(apiKey, baseURL, model string, hc *http.Client) *OpenAICompatProvider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &OpenAICompatProvider{apiKey: apiKey, baseURL: baseURL, model: model, httpClient: hc}
}

func (p *OpenAICompatProvider) Name() string { return "openai-compat" }

func (p *OpenAICompatProvider) Complete(ctx context.Context, req Request) (Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	payload, err := json.Marshal(buildOpenAIRequest(req, model))
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: marshal: %w", ErrBadRequest)
	}

	u, err := url.Parse(p.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Response{}, fmt.Errorf("ai/openai: base_url %q: %w", p.baseURL, ErrBadRequest)
	}
	u = u.JoinPath("chat", "completions")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: new request: %w", ErrProviderUnavailable)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	res, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Network failure, timeout, or SSRF dial-refusal (netsafe) all land here.
		return Response{}, fmt.Errorf("ai/openai: transport: %w", ErrProviderUnavailable)
	}
	defer func() { _ = res.Body.Close() }() // errcheck: close error is not actionable
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20)) // cap at 8 MiB

	if res.StatusCode != http.StatusOK {
		return Response{}, openAIHTTPError(res.StatusCode, body)
	}
	resp, err := parseOpenAIResponse(body)
	if err != nil {
		return Response{}, fmt.Errorf("ai/openai: decode: %w", ErrProviderUnavailable)
	}
	return resp, nil
}

// openAIHTTPError maps a non-200 status + body onto a gateway sentinel. The raw
// upstream body is NEVER surfaced (Principle II).
func openAIHTTPError(status int, body []byte) error {
	if status >= 500 || status == http.StatusTooManyRequests {
		return fmt.Errorf("ai/openai: upstream status %d: %w", status, ErrProviderUnavailable)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error.Code == "context_length_exceeded" || isContextLengthMessage(e.Error.Message) {
		return fmt.Errorf("ai/openai: %w", ErrContextLength)
	}
	return fmt.Errorf("ai/openai: upstream status %d: %w", status, ErrBadRequest)
}
