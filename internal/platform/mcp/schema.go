// Package mcp is a minimal MCP (Model Context Protocol) client over
// Streamable-HTTP (JSON-RPC 2.0, spec 2025-06-18). It covers the tools
// surface only: initialize / tools/list / tools/call. No resources, prompts,
// or sampling — those arrive in later user stories.
package mcp

import "encoding/json"

// ToolDef is a tool discovered from an MCP server (tools/list).
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result is a tools/call outcome. Content is the concatenated text blocks.
type Result struct {
	Content string
	IsError bool
}
