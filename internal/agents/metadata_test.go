package agents

import "testing"

func TestEffectClassString(t *testing.T) {
	cases := map[EffectClass]string{
		EffectRead:         "read",
		EffectReversible:   "reversible",
		EffectExternal:     "external",
		EffectIrreversible: "irreversible",
		EffectClass(99):    "unknown",
	}
	for ec, want := range cases {
		if got := ec.String(); got != want {
			t.Fatalf("EffectClass(%d).String() = %q, want %q", ec, got, want)
		}
	}
}

func TestToolRegistryAllSorted(t *testing.T) {
	reg := &ToolRegistry{tools: map[string]Tool{
		"send_reply":  {Name: "send_reply", Effect: EffectExternal},
		"read_ticket": {Name: "read_ticket", Effect: EffectRead},
		"set_status":  {Name: "set_status", Effect: EffectReversible},
	}}
	all := reg.All()
	if len(all) != 3 {
		t.Fatalf("want 3 tools, got %d", len(all))
	}
	if all[0].Name != "read_ticket" || all[1].Name != "send_reply" || all[2].Name != "set_status" {
		t.Fatalf("All() not sorted by name: %v", []string{all[0].Name, all[1].Name, all[2].Name})
	}
}
