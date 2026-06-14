package agents

import "testing"

func TestEffectFromString(t *testing.T) {
	cases := map[string]struct {
		eff EffectClass
		ok  bool
	}{
		"read":       {EffectRead, true},
		"reversible": {EffectReversible, true},
		"external":   {0, false}, // not assignable
		"":           {0, false},
		"delete":     {0, false},
	}
	for in, want := range cases {
		got, err := effectFromString(in)
		if want.ok && (err != nil || got != want.eff) {
			t.Errorf("effectFromString(%q) = (%d,%v), want (%d,nil)", in, got, err, want.eff)
		}
		if !want.ok && err == nil {
			t.Errorf("effectFromString(%q) = nil err, want validation error", in)
		}
	}
}

func TestEffectToString(t *testing.T) {
	if effectToString(EffectRead) != "read" || effectToString(EffectReversible) != "reversible" {
		t.Fatal("effectToString mapping wrong")
	}
	if effectToString(EffectExternal) != "external" {
		t.Fatal("EffectExternal must stringify to external (the default)")
	}
}
