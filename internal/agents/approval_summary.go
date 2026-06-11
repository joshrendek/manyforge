package agents

import (
	"encoding/json"
	"fmt"
	"strings"
)

// approvalSummary renders a short, human-readable, REDACTED one-line description
// of a pending action for the approvals queue. It is a presentation helper: it
// NEVER returns raw args. Any unmarshal error or unhandled tool falls back to the
// bare tool name (never an echo of args). Free text is whitespace-collapsed and
// rune-truncated. Ticket identifiers are shortened to an 8-char prefix.
func approvalSummary(tool string, args json.RawMessage) string {
	switch tool {
	case "add_external_comment", "draft_reply":
		var a struct {
			TicketID string `json:"ticket_id"`
			BodyText string `json:"body_text"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		verb := "Comment on"
		if tool == "draft_reply" {
			verb = "Draft reply on"
		}
		return fmt.Sprintf("%s ticket %s: %q", verb, shortID(a.TicketID), truncate(a.BodyText, 80))
	case "transition_external_status", "set_status":
		var a struct {
			TicketID string `json:"ticket_id"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		verb := "Transition"
		if tool == "set_status" {
			verb = "Set status of"
		}
		return fmt.Sprintf("%s ticket %s → %s", verb, shortID(a.TicketID), truncate(a.Status, 32))
	case "set_priority":
		var a struct {
			TicketID string `json:"ticket_id"`
			Priority string `json:"priority"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		return fmt.Sprintf("Set priority of ticket %s → %s", shortID(a.TicketID), truncate(a.Priority, 32))
	case "set_tags":
		var a struct {
			TicketID string   `json:"ticket_id"`
			Tags     []string `json:"tags"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		return fmt.Sprintf("Set tags of ticket %s: %s", shortID(a.TicketID), truncate(strings.Join(a.Tags, ", "), 60))
	case "set_assignee":
		var a struct {
			TicketID string  `json:"ticket_id"`
			Assignee *string `json:"assignee"`
		}
		if json.Unmarshal(args, &a) != nil {
			return tool
		}
		if a.Assignee == nil {
			return fmt.Sprintf("Unassign ticket %s", shortID(a.TicketID))
		}
		return fmt.Sprintf("Assign ticket %s", shortID(a.TicketID))
	default:
		return tool
	}
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// truncate whitespace-collapses (removing newlines/tabs) then rune-truncates with an ellipsis.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
