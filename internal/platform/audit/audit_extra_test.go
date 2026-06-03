package audit

import (
	"encoding/json"
	"testing"
)

// Pins that Entry exposes Inputs/Outputs/Decision and they marshal to the jsonb/text
// columns. The DB round-trip is covered by run_integration_test.go; here we only pin
// the struct surface + JSON shape so the loop can rely on it.
func TestEntryCarriesInputsOutputsDecision(t *testing.T) {
	dec := "executed"
	e := Entry{
		Action:   "agent.tool.invoked",
		Inputs:   map[string]any{"ticket_id": "t1"},
		Outputs:  map[string]any{"status": "open"},
		Decision: &dec,
	}
	if e.Inputs == nil || e.Outputs == nil || e.Decision == nil {
		t.Fatal("Entry must carry Inputs/Outputs/Decision")
	}
	b, err := json.Marshal(e.Inputs)
	if err != nil || string(b) != `{"ticket_id":"t1"}` {
		t.Fatalf("inputs marshal: %q err=%v", b, err)
	}
	ob, err := json.Marshal(e.Outputs)
	if err != nil || string(ob) != `{"status":"open"}` {
		t.Fatalf("outputs marshal: %q err=%v", ob, err)
	}
	if *e.Decision != "executed" {
		t.Fatalf("decision: got %q want %q", *e.Decision, "executed")
	}
}
