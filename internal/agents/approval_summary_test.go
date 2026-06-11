package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovalSummary(t *testing.T) {
	tid := "7bbeb32e-7c98-4c8f-966b-70acdb440dce"
	cases := []struct {
		name, tool, args, want string
	}{
		{"external comment", "add_external_comment",
			`{"ticket_id":"` + tid + `","body_text":"Thanks — fix shipped in v2.3"}`,
			`Comment on ticket 7bbeb32e: "Thanks — fix shipped in v2.3"`},
		{"transition", "transition_external_status",
			`{"ticket_id":"` + tid + `","status":"closed"}`,
			"Transition ticket 7bbeb32e → closed"},
		{"draft reply", "draft_reply",
			`{"ticket_id":"` + tid + `","body_text":"Hello"}`,
			`Draft reply on ticket 7bbeb32e: "Hello"`},
		{"set status", "set_status",
			`{"ticket_id":"` + tid + `","status":"solved"}`,
			"Set status of ticket 7bbeb32e → solved"},
		{"set priority", "set_priority",
			`{"ticket_id":"` + tid + `","priority":"high"}`,
			"Set priority of ticket 7bbeb32e → high"},
		{"set tags", "set_tags",
			`{"ticket_id":"` + tid + `","tags":["billing","urgent"]}`,
			"Set tags of ticket 7bbeb32e: billing, urgent"},
		{"unassign", "set_assignee",
			`{"ticket_id":"` + tid + `","assignee":null}`,
			"Unassign ticket 7bbeb32e"},
		{"assign", "set_assignee",
			`{"ticket_id":"` + tid + `","assignee":"9f1c2d3e-0000-0000-0000-000000000000"}`,
			"Assign ticket 7bbeb32e"},
		{"unknown tool falls back to bare name", "mystery_tool",
			`{"secret":"sk-leak"}`, "mystery_tool"},
		{"malformed args falls back", "add_external_comment", `{bad json`, "add_external_comment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := approvalSummary(c.tool, json.RawMessage(c.args))
			if got != c.want {
				t.Fatalf("approvalSummary(%q) = %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

func TestApprovalSummary_TruncatesAndStripsNewlines(t *testing.T) {
	// `\\n` is a literal backslash-n in the raw JSON string (valid JSON); json.Unmarshal
	// decodes it to a real newline in body_text, which truncate's strings.Fields then strips.
	long := strings.Repeat("a", 200) + "\\nSECOND LINE"
	got := approvalSummary("add_external_comment",
		json.RawMessage(`{"ticket_id":"7bbeb32e-0000-0000-0000-000000000000","body_text":"`+long+`"}`))
	if strings.Contains(got, "\n") {
		t.Fatal("summary must not contain newlines")
	}
	if strings.Contains(got, "SECOND LINE") || len([]rune(got)) > 120 {
		t.Fatalf("summary not truncated: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatal("expected ellipsis on truncated body")
	}
}
