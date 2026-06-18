// Package ai is the provider-agnostic LLM gateway: one internal message/tool
// schema and a Provider interface that hand-rolled transports (anthropic,
// openai-compat) and a MockProvider implement. It holds NO database state and
// NO domain logic — callers pass a resolved credential + a Request and get a
// Response. (Spec 003 SL-A.)
package ai

import "encoding/json"

// Role is a chat message author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // a tool result fed back to the model
)

// FinishReason is why the model stopped.
type FinishReason string

const (
	FinishStop    FinishReason = "stop"     // natural end / final text
	FinishToolUse FinishReason = "tool_use" // model wants tool calls run
	FinishLength  FinishReason = "length"   // hit max tokens
	FinishOther   FinishReason = "other"
)

// ToolDef advertises a tool to the model. Schema is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// ToolCall is the model asking to run a tool. Args is the raw JSON arguments —
// it is UNTRUSTED and must be validated against the tool's schema before use.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult is the outcome of a tool call, fed back to the model.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Message is one turn. A RoleTool message carries ToolResults; an assistant
// message may carry Text and/or ToolCalls.
type Message struct {
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// ServerToolDef is a provider-executed (server-side) tool — currently OpenRouter's
// web_fetch/web_search. Unlike ToolDef (a function the agent invokes), these run at the
// provider; the model calls them and results return inline. Only emitted for OpenRouter.
type ServerToolDef struct {
	Type           string   // e.g. "openrouter:web_fetch", "openrouter:web_search"
	AllowedDomains []string // web_fetch domain scope (empty = unset)
}

// Request is a single completion call.
type Request struct {
	Model       string          `json:"model"`
	System      string          `json:"system,omitempty"`
	Messages    []Message       `json:"messages"`
	Tools       []ToolDef       `json:"tools,omitempty"`
	ServerTools []ServerToolDef `json:"server_tools,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
}

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total is input+output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Response is one completion result.
type Response struct {
	Text         string       `json:"text"`
	ToolCalls    []ToolCall   `json:"tool_calls,omitempty"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
}
