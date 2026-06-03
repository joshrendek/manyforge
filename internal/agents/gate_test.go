package agents

import "testing"

func TestGateMatrix(t *testing.T) {
	cases := []struct {
		effect EffectClass
		mode   int
		want   autonomyDecision
	}{
		// reads: always inline
		{EffectRead, ModeAssist, decideExec},
		{EffectRead, ModeQueueWrites, decideExec},
		{EffectRead, ModeAutonomous, decideExec},
		// reversible writes: inline in 1 & 3, queued in 2
		{EffectReversible, ModeAssist, decideExec},
		{EffectReversible, ModeQueueWrites, decideApproval},
		{EffectReversible, ModeAutonomous, decideExec},
		// external: queued in 1 & 2, inline in 3
		{EffectExternal, ModeAssist, decideApproval},
		{EffectExternal, ModeQueueWrites, decideApproval},
		{EffectExternal, ModeAutonomous, decideExec},
		// irreversible: queued in 1 & 2, inline in 3
		{EffectIrreversible, ModeAssist, decideApproval},
		{EffectIrreversible, ModeQueueWrites, decideApproval},
		{EffectIrreversible, ModeAutonomous, decideExec},
		// fail-closed: unknown effect ⇒ approval in every mode
		{EffectClass(99), ModeAssist, decideApproval},
		{EffectClass(99), ModeAutonomous, decideApproval},
		// fail-closed: unknown mode ⇒ approval even for a reversible write
		{EffectReversible, 0, decideApproval},
		{EffectReversible, 7, decideApproval},
		{EffectRead, 0, decideExec}, // reads still inline regardless of mode
	}
	for _, c := range cases {
		if got := gate(c.effect, c.mode); got != c.want {
			t.Errorf("gate(%d,%d) = %d, want %d", c.effect, c.mode, got, c.want)
		}
	}
}
