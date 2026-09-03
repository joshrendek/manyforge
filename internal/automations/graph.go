// Package automations owns branching drip automation definitions and lifecycle.
package automations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const MaxNodes = 200

var graphIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

type Node struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Name   string          `json:"name,omitempty"`
	Config json.RawMessage `json:"config"`
}

type Edge struct {
	ID     string  `json:"id"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Branch *string `json:"branch"`
}

type Issue struct {
	Code    string `json:"code"`
	NodeID  string `json:"node_id,omitempty"`
	EdgeID  string `json:"edge_id,omitempty"`
	Message string `json:"message"`
}

// References is the set of business-local records visible to graph validation.
// A nil map skips that reference class, which is useful for syntax-only clients.
type References struct {
	Lists     map[uuid.UUID]bool
	Templates map[uuid.UUID]bool
}

type conditionReference struct {
	conditionID string
	sendNodeID  string
}

// Validate returns every graph issue in deterministic node/edge order.
func Validate(graph Graph, refs References) []Issue {
	issues := make([]Issue, 0)
	if len(graph.Nodes) > MaxNodes {
		issues = append(issues, Issue{Code: "too_many_nodes", Message: fmt.Sprintf("graph has %d nodes; maximum is %d", len(graph.Nodes), MaxNodes)})
	}

	nodes := make(map[string]Node, len(graph.Nodes))
	triggerIDs := make([]string, 0, 1)
	conditions := make([]conditionReference, 0)
	for _, node := range graph.Nodes {
		if !graphIDPattern.MatchString(node.ID) {
			issues = append(issues, nodeIssue("invalid_node_id", node.ID, "node id must match ^[a-z0-9_-]{1,64}$"))
		}
		if _, exists := nodes[node.ID]; exists {
			issues = append(issues, nodeIssue("duplicate_node_id", node.ID, "node id must be unique"))
		} else {
			nodes[node.ID] = node
		}
		if len(node.Name) > 200 {
			issues = append(issues, nodeIssue("invalid_node_name", node.ID, "node name must not exceed 200 characters"))
		}
		if node.Kind == "trigger" {
			triggerIDs = append(triggerIDs, node.ID)
		}
		configIssues, conditionRef := validateConfig(node, refs)
		issues = append(issues, configIssues...)
		if conditionRef != "" {
			conditions = append(conditions, conditionReference{conditionID: node.ID, sendNodeID: conditionRef})
		}
	}
	if len(triggerIDs) != 1 {
		issues = append(issues, Issue{Code: "trigger_count", Message: "graph must contain exactly one trigger"})
	}

	outgoing := make(map[string][]Edge, len(nodes))
	incoming := make(map[string][]Edge, len(nodes))
	edgeIDs := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		if !graphIDPattern.MatchString(edge.ID) {
			issues = append(issues, edgeIssue("invalid_edge_id", edge.ID, "edge id must match ^[a-z0-9_-]{1,64}$"))
		}
		if edgeIDs[edge.ID] {
			issues = append(issues, edgeIssue("duplicate_edge_id", edge.ID, "edge id must be unique"))
		}
		edgeIDs[edge.ID] = true
		_, fromOK := nodes[edge.From]
		_, toOK := nodes[edge.To]
		if !fromOK || !toOK {
			issues = append(issues, edgeIssue("edge_unknown_node", edge.ID, "edge endpoints must reference existing nodes"))
			continue
		}
		if edge.From == edge.To {
			issues = append(issues, edgeIssue("self_loop", edge.ID, "self-loop edges are not allowed"))
		}
		outgoing[edge.From] = append(outgoing[edge.From], edge)
		incoming[edge.To] = append(incoming[edge.To], edge)
	}

	for _, node := range graph.Nodes {
		outs := outgoing[node.ID]
		if node.Kind == "condition" {
			yes, no := 0, 0
			for _, edge := range outs {
				if edge.Branch == nil {
					issues = append(issues, edgeIssue("condition_branch_required", edge.ID, "condition edges require a yes or no branch"))
				} else if *edge.Branch == "yes" {
					yes++
				} else if *edge.Branch == "no" {
					no++
				} else {
					issues = append(issues, edgeIssue("invalid_edge_branch", edge.ID, "edge branch must be yes or no"))
				}
			}
			if yes != 1 || no != 1 || len(outs) != 2 {
				issues = append(issues, nodeIssue("condition_branches", node.ID, "condition must have exactly one yes edge and one no edge"))
			}
		} else {
			if len(outs) > 1 {
				issues = append(issues, nodeIssue("invalid_out_degree", node.ID, "non-condition nodes may have at most one outgoing edge"))
			}
			for _, edge := range outs {
				if edge.Branch != nil {
					issues = append(issues, edgeIssue("unexpected_edge_branch", edge.ID, "only condition edges may have a branch"))
				}
			}
		}
	}
	for _, triggerID := range triggerIDs {
		if len(incoming[triggerID]) != 0 {
			issues = append(issues, nodeIssue("trigger_indegree", triggerID, "trigger must have no incoming edges"))
		}
	}

	if graphHasCycle(nodes, outgoing, incoming) {
		issues = append(issues, Issue{Code: "cycle", Message: "graph must be acyclic"})
	}
	if len(triggerIDs) == 1 {
		reachable := reachableFrom(triggerIDs[0], outgoing)
		for _, node := range graph.Nodes {
			if !reachable[node.ID] {
				issues = append(issues, nodeIssue("unreachable_node", node.ID, "node must be reachable from the trigger"))
			}
		}
	}
	for _, ref := range conditions {
		node, exists := nodes[ref.sendNodeID]
		if !exists || node.Kind != "send_email" || !isAncestor(ref.sendNodeID, ref.conditionID, outgoing) {
			issues = append(issues, nodeIssue("invalid_engagement_ancestor", ref.conditionID, "engagement predicate must reference an ancestor send_email node"))
		}
	}
	return issues
}

func validateConfig(node Node, refs References) ([]Issue, string) {
	bad := func(message string) ([]Issue, string) {
		return []Issue{nodeIssue("invalid_config", node.ID, message)}, ""
	}
	switch node.Kind {
	case "trigger":
		var cfg struct {
			Type   string    `json:"type"`
			ListID uuid.UUID `json:"list_id"`
			Tag    string    `json:"tag,omitempty"`
			Name   string    `json:"name,omitempty"`
		}
		if err := decodeConfig(node.Config, &cfg); err != nil || cfg.ListID == uuid.Nil {
			return bad("trigger config is malformed")
		}
		switch cfg.Type {
		case "list_joined":
			if cfg.Tag != "" || cfg.Name != "" {
				return bad("list_joined trigger only accepts list_id")
			}
		case "tag_added":
			if !validLabel(cfg.Tag, 64) || cfg.Name != "" {
				return bad("tag_added trigger requires a valid tag")
			}
		case "event":
			if !validLabel(cfg.Name, 128) || cfg.Tag != "" {
				return bad("event trigger requires a valid name")
			}
		default:
			return bad("trigger type must be list_joined, tag_added, or event")
		}
		if refs.Lists != nil && !refs.Lists[cfg.ListID] {
			return []Issue{nodeIssue("list_not_found", node.ID, "referenced mailing list was not found")}, ""
		}
	case "send_email":
		var cfg struct {
			TemplateID  uuid.UUID `json:"template_id"`
			TrackOpens  *bool     `json:"track_opens"`
			TrackClicks *bool     `json:"track_clicks"`
		}
		if err := decodeConfig(node.Config, &cfg); err != nil || cfg.TemplateID == uuid.Nil || cfg.TrackOpens == nil || cfg.TrackClicks == nil {
			return bad("send_email requires template_id, track_opens, and track_clicks")
		}
		if refs.Templates != nil && !refs.Templates[cfg.TemplateID] {
			return []Issue{nodeIssue("template_not_found", node.ID, "referenced mailing template was not found")}, ""
		}
	case "wait":
		var cfg struct {
			Mode     string `json:"mode"`
			Seconds  *int64 `json:"seconds,omitempty"`
			Weekday  *int   `json:"weekday,omitempty"`
			Time     string `json:"time,omitempty"`
			Timezone string `json:"timezone,omitempty"`
		}
		if err := decodeConfig(node.Config, &cfg); err != nil {
			return bad("wait config is malformed")
		}
		switch cfg.Mode {
		case "duration":
			if cfg.Seconds == nil || *cfg.Seconds < 60 || *cfg.Seconds > 31536000 || cfg.Weekday != nil || cfg.Time != "" || cfg.Timezone != "" {
				return bad("duration wait requires seconds between 60 and 31536000")
			}
		case "until":
			if cfg.Seconds != nil || (cfg.Weekday != nil && (*cfg.Weekday < 1 || *cfg.Weekday > 7)) || !validClock(cfg.Time) {
				return bad("until wait requires an optional weekday 1-7 and time HH:MM")
			}
			if _, err := time.LoadLocation(cfg.Timezone); err != nil {
				return bad("until wait requires a valid IANA timezone")
			}
		default:
			return bad("wait mode must be duration or until")
		}
	case "condition":
		var cfg struct {
			Predicate json.RawMessage `json:"predicate"`
		}
		if err := decodeConfig(node.Config, &cfg); err != nil || len(cfg.Predicate) == 0 {
			return bad("condition requires one predicate")
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(cfg.Predicate, &kind); err != nil {
			return bad("condition predicate is malformed")
		}
		switch kind.Type {
		case "opened_email":
			var p struct {
				Type   string `json:"type"`
				NodeID string `json:"node_id"`
			}
			if decodeConfig(cfg.Predicate, &p) != nil || !graphIDPattern.MatchString(p.NodeID) {
				return bad("opened_email predicate requires node_id")
			}
			return nil, p.NodeID
		case "clicked_link":
			var p struct {
				Type   string  `json:"type"`
				NodeID string  `json:"node_id"`
				URL    *string `json:"url"`
			}
			if decodeConfig(cfg.Predicate, &p) != nil || !graphIDPattern.MatchString(p.NodeID) || (p.URL != nil && !validHTTPURL(*p.URL)) {
				return bad("clicked_link predicate requires node_id and an optional HTTP URL")
			}
			return nil, p.NodeID
		case "has_tag":
			var p struct{ Type, Tag string }
			if decodeConfig(cfg.Predicate, &p) != nil || !validLabel(p.Tag, 64) {
				return bad("has_tag predicate requires a valid tag")
			}
		case "on_list":
			var p struct {
				Type   string    `json:"type"`
				ListID uuid.UUID `json:"list_id"`
			}
			if decodeConfig(cfg.Predicate, &p) != nil || p.ListID == uuid.Nil {
				return bad("on_list predicate requires list_id")
			}
			if refs.Lists != nil && !refs.Lists[p.ListID] {
				return []Issue{nodeIssue("list_not_found", node.ID, "referenced mailing list was not found")}, ""
			}
		case "event_received":
			var p struct {
				Type          string `json:"type"`
				Name          string `json:"name"`
				WithinSeconds *int64 `json:"within_seconds"`
			}
			if decodeConfig(cfg.Predicate, &p) != nil || !validLabel(p.Name, 128) || (p.WithinSeconds != nil && (*p.WithinSeconds < 1 || *p.WithinSeconds > 31536000)) {
				return bad("event_received requires a valid name and optional within_seconds")
			}
		default:
			return bad("unknown condition predicate type")
		}
	case "add_tag", "remove_tag":
		var cfg struct {
			Tag string `json:"tag"`
		}
		if decodeConfig(node.Config, &cfg) != nil || !validLabel(cfg.Tag, 64) {
			return bad(node.Kind + " requires a valid tag")
		}
	case "exit":
		var cfg struct{}
		if decodeConfig(node.Config, &cfg) != nil {
			return bad("exit config must be empty")
		}
	default:
		return []Issue{nodeIssue("unknown_node_kind", node.ID, "unknown node kind")}, ""
	}
	return nil, ""
}

func decodeConfig(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("config must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing config data")
	}
	return nil
}

func graphHasCycle(nodes map[string]Node, outgoing, incoming map[string][]Edge) bool {
	degree := make(map[string]int, len(nodes))
	queue := make([]string, 0, len(nodes))
	for id := range nodes {
		degree[id] = len(incoming[id])
		if degree[id] == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, edge := range outgoing[id] {
			degree[edge.To]--
			if degree[edge.To] == 0 {
				queue = append(queue, edge.To)
			}
		}
	}
	return seen != len(nodes)
}

func reachableFrom(root string, outgoing map[string][]Edge) map[string]bool {
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, edge := range outgoing[id] {
			if !seen[edge.To] {
				seen[edge.To] = true
				queue = append(queue, edge.To)
			}
		}
	}
	return seen
}

func isAncestor(from, to string, outgoing map[string][]Edge) bool {
	if from == to {
		return false
	}
	return reachableFrom(from, outgoing)[to]
}

func nodeIssue(code, id, message string) Issue {
	return Issue{Code: code, NodeID: id, Message: message}
}

func edgeIssue(code, id, message string) Issue {
	return Issue{Code: code, EdgeID: id, Message: message}
}

func validLabel(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= max
}

func validClock(value string) bool {
	_, err := time.Parse("15:04", value)
	return err == nil && len(value) == 5
}

func validHTTPURL(value string) bool {
	u, err := url.ParseRequestURI(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
