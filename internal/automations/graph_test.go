package automations

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

var testListID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
var testTemplateID = uuid.MustParse("22222222-2222-2222-2222-222222222222")

func validGraph() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"list_joined","list_id":"11111111-1111-1111-1111-111111111111"}`)},
			{ID: "welcome", Kind: "send_email", Config: json.RawMessage(`{"template_id":"22222222-2222-2222-2222-222222222222","track_opens":true,"track_clicks":true}`)},
			{ID: "opened", Kind: "condition", Config: json.RawMessage(`{"predicate":{"type":"opened_email","node_id":"welcome"}}`)},
			{ID: "tag", Kind: "add_tag", Config: json.RawMessage(`{"tag":"engaged"}`)},
			{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
		},
		Edges: []Edge{
			{ID: "e1", From: "trigger", To: "welcome"},
			{ID: "e2", From: "welcome", To: "opened"},
			{ID: "e3", From: "opened", To: "tag", Branch: stringPtr("yes")},
			{ID: "e4", From: "opened", To: "exit", Branch: stringPtr("no")},
			{ID: "e5", From: "tag", To: "exit"},
		},
	}
}

func TestValidate(t *testing.T) {
	refs := References{Lists: map[uuid.UUID]bool{testListID: true}, Templates: map[uuid.UUID]bool{testTemplateID: true}}
	tests := []struct {
		name string
		edit func(*Graph)
		code string
	}{
		{name: "valid"},
		{name: "duplicate node", edit: func(g *Graph) { g.Nodes[1].ID = "trigger" }, code: "duplicate_node_id"},
		{name: "cycle", edit: func(g *Graph) { g.Edges = append(g.Edges, Edge{ID: "loop", From: "exit", To: "welcome"}) }, code: "cycle"},
		{name: "unreachable", edit: func(g *Graph) { g.Edges = g.Edges[1:] }, code: "unreachable_node"},
		{name: "condition branches", edit: func(g *Graph) { g.Edges[3].Branch = stringPtr("yes") }, code: "condition_branches"},
		{name: "unknown endpoint", edit: func(g *Graph) { g.Edges[0].To = "missing" }, code: "edge_unknown_node"},
		{name: "self loop", edit: func(g *Graph) { g.Edges[4].To = "tag" }, code: "self_loop"},
		{name: "bad id", edit: func(g *Graph) { g.Nodes[4].ID = "NO" }, code: "invalid_node_id"},
		{name: "missing template", edit: func(g *Graph) {
			g.Nodes[1].Config = json.RawMessage(`{"template_id":"33333333-3333-3333-3333-333333333333","track_opens":true,"track_clicks":true}`)
		}, code: "template_not_found"},
		{name: "missing list", edit: func(g *Graph) {
			g.Nodes[0].Config = json.RawMessage(`{"type":"list_joined","list_id":"33333333-3333-3333-3333-333333333333"}`)
		}, code: "list_not_found"},
		{name: "non ancestor engagement", edit: func(g *Graph) {
			g.Nodes[2].Config = json.RawMessage(`{"predicate":{"type":"opened_email","node_id":"later"}}`)
			g.Nodes = append(g.Nodes, Node{ID: "later", Kind: "send_email", Config: g.Nodes[1].Config})
			g.Edges = append(g.Edges, Edge{ID: "e6", From: "exit", To: "later"})
		}, code: "invalid_engagement_ancestor"},
		{name: "invalid duration", edit: func(g *Graph) {
			g.Nodes[1] = Node{ID: "welcome", Kind: "wait", Config: json.RawMessage(`{"mode":"duration","seconds":59}`)}
		}, code: "invalid_config"},
		{name: "unknown config field", edit: func(g *Graph) { g.Nodes[4].Config = json.RawMessage(`{"position":1}`) }, code: "invalid_config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := validGraph()
			if tc.edit != nil {
				tc.edit(&graph)
			}
			issues := Validate(graph, refs)
			if tc.code == "" {
				if len(issues) != 0 {
					t.Fatalf("issues = %+v", issues)
				}
				return
			}
			for _, issue := range issues {
				if issue.Code == tc.code {
					return
				}
			}
			t.Fatalf("issues = %+v, want code %q", issues, tc.code)
		})
	}
}

func TestValidateNodeLimit(t *testing.T) {
	graph := Graph{Nodes: make([]Node, MaxNodes+1)}
	issues := Validate(graph, References{})
	if !hasIssue(issues, "too_many_nodes") {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestValidateConfigKinds(t *testing.T) {
	configs := []struct {
		name   string
		kind   string
		config string
	}{
		{"tag trigger", "trigger", `{"type":"tag_added","tag":"vip","list_id":"11111111-1111-1111-1111-111111111111"}`},
		{"event trigger", "trigger", `{"type":"event","name":"trial.started","list_id":"11111111-1111-1111-1111-111111111111"}`},
		{"duration wait", "wait", `{"mode":"duration","seconds":60}`},
		{"until wait", "wait", `{"mode":"until","weekday":1,"time":"09:30","timezone":"America/New_York"}`},
		{"clicked condition", "condition", `{"predicate":{"type":"clicked_link","node_id":"mail","url":"https://example.test/welcome"}}`},
		{"tag condition", "condition", `{"predicate":{"type":"has_tag","tag":"vip"}}`},
		{"list condition", "condition", `{"predicate":{"type":"on_list","list_id":"11111111-1111-1111-1111-111111111111"}}`},
		{"event condition", "condition", `{"predicate":{"type":"event_received","name":"trial.started","within_seconds":3600}}`},
		{"add tag", "add_tag", `{"tag":"vip"}`},
		{"remove tag", "remove_tag", `{"tag":"vip"}`},
		{"exit", "exit", `{}`},
	}
	refs := References{Lists: map[uuid.UUID]bool{testListID: true}, Templates: map[uuid.UUID]bool{testTemplateID: true}}
	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			node := Node{ID: "test", Kind: tc.kind, Config: json.RawMessage(tc.config)}
			issues, _ := validateConfig(node, refs)
			if len(issues) != 0 {
				t.Fatalf("issues = %+v", issues)
			}
		})
	}
}

func hasIssue(issues []Issue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func stringPtr(value string) *string { return &value }
