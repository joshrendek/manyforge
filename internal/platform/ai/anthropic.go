package ai

import (
	"encoding/json"
)

// Anthropic Messages API wire format. Hand-rolled (no vendor SDK). Only the
// fields we send/read are modeled; unknown response fields are ignored.

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
)

type anthropicReq struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMessage struct {
	Role    string           `json:"role"` // "user" | "assistant"
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is a polymorphic content block. Type selects which fields are
// populated: text -> Text; tool_use -> ID/Name/Input; tool_result -> ToolUseID/Content/IsError.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicResp struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// parseAnthropicResponse maps a 200 Messages body onto the common Response.
func parseAnthropicResponse(body []byte) (Response, error) {
	var ar anthropicResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return Response{}, err
	}
	var out Response
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	out.FinishReason = anthropicFinish(ar.StopReason)
	out.Usage = Usage{InputTokens: ar.Usage.InputTokens, OutputTokens: ar.Usage.OutputTokens}
	return out, nil
}

func anthropicFinish(raw string) FinishReason {
	switch raw {
	case "end_turn", "stop_sequence":
		return FinishStop
	case "tool_use":
		return FinishToolUse
	case "max_tokens":
		return FinishLength
	default:
		return FinishOther
	}
}

// buildAnthropicRequest maps the common Request onto Anthropic's Messages wire
// format. model overrides req.Model when req.Model is empty (transport default).
func buildAnthropicRequest(req Request, model string) anthropicReq {
	out := anthropicReq{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		System:      req.System,
	}
	for _, td := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name: td.Name, Description: td.Description, InputSchema: td.Schema,
		})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			// Fold a system message into the top-level system field.
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += m.Text
		case RoleTool:
			var blocks []anthropicBlock
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_result", ToolUseID: tr.CallID, Content: tr.Content, IsError: tr.IsError,
				})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: blocks})
		default: // RoleUser / RoleAssistant
			var blocks []anthropicBlock
			if m.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Args,
				})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: string(m.Role), Content: blocks})
		}
	}
	return out
}
