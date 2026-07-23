package coding

import "testing"

func TestApplyOverridesToPanel_DisableAndFloor(t *testing.T) {
	panel := []Dimension{
		{Key: "security", Enabled: true, MinSeverity: "info"},
		{Key: "docs", Enabled: true, MinSeverity: "info"},
		{Key: "tests", Enabled: true, MinSeverity: "warning"},
	}
	warn := "warning"
	overrides := map[string]repoOverride{
		"security": {Enabled: false, MinSeverity: nil}, // disabled for this repo
		"docs":     {Enabled: true, MinSeverity: &warn}, // floor raised to warning
		// tests: no override → inherited unchanged
	}
	out := applyOverridesToPanel(panel, overrides)
	if out[0].Enabled {
		t.Error("security must be disabled by the override")
	}
	if out[1].MinSeverity != "warning" || !out[1].Enabled {
		t.Errorf("docs must keep enabled + take the overridden floor; got enabled=%v floor=%s", out[1].Enabled, out[1].MinSeverity)
	}
	if !out[2].Enabled || out[2].MinSeverity != "warning" {
		t.Errorf("tests (no override) must be inherited unchanged; got enabled=%v floor=%s", out[2].Enabled, out[2].MinSeverity)
	}
}

func TestApplyOverridesToPanel_NilOrBlankSeverityInherits(t *testing.T) {
	blank := ""
	panel := []Dimension{
		{Key: "security", Enabled: true, MinSeverity: "error"},
		{Key: "docs", Enabled: true, MinSeverity: "error"},
	}
	overrides := map[string]repoOverride{
		"security": {Enabled: true, MinSeverity: nil},     // no floor override
		"docs":     {Enabled: true, MinSeverity: &blank},  // explicit blank ⇒ also inherit
	}
	out := applyOverridesToPanel(panel, overrides)
	for i, d := range out {
		if d.MinSeverity != "error" {
			t.Errorf("dim %d: a nil/blank override floor must inherit the business floor; got %s", i, d.MinSeverity)
		}
	}
}

func TestApplyOverridesToPanel_UnknownKeyIgnored(t *testing.T) {
	panel := []Dimension{{Key: "security", Enabled: true}}
	out := applyOverridesToPanel(panel, map[string]repoOverride{"nonexistent": {Enabled: false}})
	if !out[0].Enabled {
		t.Error("an override for a dimension not in the panel must not affect any lane")
	}
}
