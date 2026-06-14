// mf_004_us6_agent_tools (Spec 004 US6 §7, manyforge-a7j.6.7): source-level pins for the
// connector agent tools. All pins here are pure string matches against source files (no
// infrastructure) and run under both `make test` AND `make sec-test`. The one genuinely
// behavioral tenant-isolation pin (needs a real DB) lives in the integration-tagged file
// internal/connectors/mf_004_us6_agent_tools_integration_test.go.
//
// Finding IDs (Spec 004 US6 §7):
//   - MF-004-US6-EFFECT-CLASSES  — effect classifications are correct: read_external_ticket
//     is EffectRead, add_external_comment and transition_external_status are EffectExternal.
//     A refactor downgrading a write tool to EffectReversible would bypass the external-action
//     gate in ModeAssist, letting the LLM auto-execute external ops without approval.
//   - MF-004-US6-PERM-GATING     — write tools carry RequiredPerm "connectors.write" and the
//     read tool "connectors.read". Dropping or downgrading a perm would let an unpermissioned
//     agent call external-system ops.
//   - MF-004-US6-NO-BYPASS       — connector tool Invoke bodies contain no direct gate/decideExec
//     call (gating is the loop's job). If a tool called gate itself it could decide its own
//     approval fate, bypassing the central gate in runner.go.
//   - MF-004-US6-ENQUEUES        — write tool Invoke bodies call EnqueueComment/EnqueueTransition
//     (non-vacuity: the gate genuinely guards a real side-effect path).
//   - MF-004-US6-GATE-ORDER      — runner.go calls gate/decideApproval BEFORE tool.Invoke (the
//     gate cannot protect an already-executed call).
//   - MF-004-US6-AUDIT           — migration 0047 installs complete_outbound_transition, which
//     audits 'connector.outbound.transitioned' for every executed transition op.
//   - MF-004-US6-TENANT          — see internal/connectors/mf_004_us6_agent_tools_integration_test.go
package security_regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAgentsSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "agents", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US6: read %s: %v", path, err)
	}
	return string(raw)
}

// TestMF004US6_EffectClasses pins MF-004-US6-EFFECT-CLASSES (Spec 004 US6 §7).
// read_external_ticket must be EffectRead; add_external_comment and
// transition_external_status must be EffectExternal. If a write tool were reclassified
// as EffectReversible (value 1), ModeAssist would auto-execute it inline without queuing
// an approval — the external-op gate would be silently bypassed.
func TestMF004US6_EffectClasses(t *testing.T) {
	// MF-004-US6-EFFECT-CLASSES — Spec 004 US6 §7
	src := readAgentsSource(t, "tools.go")

	// read_external_ticket: must be EffectRead (never mutates; gate runs reads inline).
	if !strings.Contains(src, `Name: "read_external_ticket", Effect: EffectRead`) {
		t.Fatal("MF-004-US6-EFFECT-CLASSES SOURCE PIN: read_external_ticket no longer declares Effect: EffectRead — reads must never queue for approval")
	}

	// add_external_comment: must be EffectExternal (leaves tenant boundary → gate queues in Assist/QueueWrites).
	if !strings.Contains(src, `Name: "add_external_comment", Effect: EffectExternal`) {
		t.Fatal("MF-004-US6-EFFECT-CLASSES SOURCE PIN: add_external_comment no longer declares Effect: EffectExternal — write tool must be gated in ModeAssist/QueueWrites")
	}

	// transition_external_status: must be EffectExternal.
	if !strings.Contains(src, `Name: "transition_external_status", Effect: EffectExternal`) {
		t.Fatal("MF-004-US6-EFFECT-CLASSES SOURCE PIN: transition_external_status no longer declares Effect: EffectExternal — write tool must be gated in ModeAssist/QueueWrites")
	}
}

// TestMF004US6_PermGating pins MF-004-US6-PERM-GATING (Spec 004 US6 §7).
// The RBAC check in execTool uses each tool's RequiredPerm; if a write tool's perm were
// dropped or changed to a weaker key, an unpermissioned agent could trigger external ops.
func TestMF004US6_PermGating(t *testing.T) {
	// MF-004-US6-PERM-GATING — Spec 004 US6 §7
	src := readAgentsSource(t, "tools.go")

	// read_external_ticket requires connectors.read. We pin the full struct-literal triple
	// (Name + Effect + RequiredPerm are contiguous on one line) so a double-edit regression
	// — moving read_external_ticket to a different perm AND adding connectors.read to some
	// OTHER tool — cannot escape a file-wide substring match.
	// RequiredPerm uses authz.PermConnectors* constants since manyforge-xxe (the constant→SQL
	// key mapping is pinned by TestPin_PermConstantsMatchSeededCatalog).
	if !strings.Contains(src, `Name: "read_external_ticket", Effect: EffectRead, RequiredPerm: authz.PermConnectorsRead`) {
		t.Fatal(`MF-004-US6-PERM-GATING SOURCE PIN: read_external_ticket no longer declares RequiredPerm: authz.PermConnectorsRead on its Name/Effect line — the read tool must be perm-gated by connectors.read`)
	}

	// Both write tools require connectors.write. The perm must appear; one occurrence per
	// write tool means it appears at least twice (the source contains exactly two write
	// tool declarations). We count occurrences to pin that BOTH tools carry the perm.
	count := strings.Count(src, `RequiredPerm: authz.PermConnectorsWrite`)
	if count < 2 {
		t.Fatalf("MF-004-US6-PERM-GATING SOURCE PIN: connectors.write appears %d time(s) in tools.go, want ≥2 (one per write tool — add_external_comment + transition_external_status)", count)
	}
}

// TestMF004US6_NoBypassInInvokeBodies pins MF-004-US6-NO-BYPASS (Spec 004 US6 §7).
// The central gate in runner.go (execTool) is the ONLY place that calls gate/decideApproval.
// A connector-tool Invoke body that directly called gate or decideExec would short-circuit
// the central gate, deciding its own approval fate without audit.
func TestMF004US6_NoBypassInInvokeBodies(t *testing.T) {
	// MF-004-US6-NO-BYPASS — Spec 004 US6 §7
	src := readAgentsSource(t, "tools.go")

	// gate() and decideExec are defined in gate.go (different file). Their presence in
	// tools.go would mean a tool body is calling the gate directly.
	if strings.Contains(src, "gate(") {
		t.Fatal("MF-004-US6-NO-BYPASS SOURCE PIN: tools.go calls gate() — connector tool Invoke bodies must not call gate; gating is the runner loop's exclusive responsibility")
	}
	if strings.Contains(src, "decideExec") {
		t.Fatal("MF-004-US6-NO-BYPASS SOURCE PIN: tools.go references decideExec — tool bodies must not bypass the central gate")
	}
	if strings.Contains(src, "decideApproval") {
		t.Fatal("MF-004-US6-NO-BYPASS SOURCE PIN: tools.go references decideApproval — tool bodies must not bypass the central gate")
	}
}

// TestMF004US6_WriteToolsEnqueue pins MF-004-US6-ENQUEUES (Spec 004 US6 §7).
// Non-vacuity: the write tools must actually call EnqueueComment / EnqueueTransition.
// If they did not, the central gate would guard a no-op and the pin would be vacuously
// satisfied even after a regression that removed the external side-effect entirely.
func TestMF004US6_WriteToolsEnqueue(t *testing.T) {
	// MF-004-US6-ENQUEUES — Spec 004 US6 §7
	src := readAgentsSource(t, "tools.go")

	if !strings.Contains(src, "conn.EnqueueComment(") {
		t.Fatal("MF-004-US6-ENQUEUES SOURCE PIN: tools.go no longer calls conn.EnqueueComment — add_external_comment must enqueue a real side-effect guarded by the central gate")
	}
	if !strings.Contains(src, "conn.EnqueueTransition(") {
		t.Fatal("MF-004-US6-ENQUEUES SOURCE PIN: tools.go no longer calls conn.EnqueueTransition — transition_external_status must enqueue a real side-effect guarded by the central gate")
	}
}

// TestMF004US6_GateCalledBeforeInvoke pins MF-004-US6-GATE-ORDER (Spec 004 US6 §7).
// In runner.go's execTool, gate(tool.Effect, mode) must be called and evaluated BEFORE
// tool.Invoke. If the order were reversed, the gate would protect an already-executed op.
// We pin this structurally: gate(..) == decideApproval comparison MUST appear before
// any tool.Invoke call in the same function body.
func TestMF004US6_GateCalledBeforeInvoke(t *testing.T) {
	// MF-004-US6-GATE-ORDER — Spec 004 US6 §7
	src := readAgentsSource(t, "runner.go")

	gateIdx := strings.Index(src, "gate(tool.Effect, mode)")
	if gateIdx < 0 {
		t.Fatal("MF-004-US6-GATE-ORDER SOURCE PIN: runner.go no longer calls gate(tool.Effect, mode) — central gate must evaluate effect+mode before every tool invocation")
	}
	invokeIdx := strings.Index(src, "tool.Invoke(")
	if invokeIdx < 0 {
		t.Fatal("MF-004-US6-GATE-ORDER SOURCE PIN: runner.go no longer calls tool.Invoke — central gate must guard a real invocation")
	}
	if gateIdx > invokeIdx {
		t.Fatal("MF-004-US6-GATE-ORDER SOURCE PIN: gate(tool.Effect, mode) appears AFTER tool.Invoke in runner.go — gate must be checked BEFORE execution")
	}

	// Additionally pin that the decideApproval branch (the queuing path) appears before
	// the Invoke call — not just that gate is called before Invoke.
	approvalBranchIdx := strings.Index(src, "decideApproval")
	if approvalBranchIdx < 0 {
		t.Fatal("MF-004-US6-GATE-ORDER SOURCE PIN: runner.go no longer branches on decideApproval — gate path may be no-op")
	}
	if approvalBranchIdx > invokeIdx {
		t.Fatal("MF-004-US6-GATE-ORDER SOURCE PIN: decideApproval branch appears AFTER tool.Invoke in runner.go — gate must route to queuing BEFORE execution")
	}
}

// TestMF004US6_TransitionAuditInMigration0047 pins MF-004-US6-AUDIT (Spec 004 US6 §7).
// Migration 0047 installs complete_outbound_transition, a SECURITY DEFINER function that
// audits every executed transition with 'connector.outbound.transitioned'. If this audit
// INSERT were removed, transitions would leave the tenant boundary with no durable record.
func TestMF004US6_TransitionAuditInMigration0047(t *testing.T) {
	// MF-004-US6-AUDIT — Spec 004 US6 §7 (migration 0047)
	path := filepath.Join("..", "..", "migrations", "0047_connector_agent_tools.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US6-AUDIT SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)

	// The DEFINER function must exist and audit the correct action string.
	if !strings.Contains(src, "complete_outbound_transition") {
		t.Fatal("MF-004-US6-AUDIT SOURCE PIN: 0047 no longer defines complete_outbound_transition — transition completion DEFINER function is missing")
	}
	if !strings.Contains(src, "connector.outbound.transitioned") {
		t.Fatal("MF-004-US6-AUDIT SOURCE PIN: 0047 no longer audits 'connector.outbound.transitioned' — every executed transition must produce a durable audit entry")
	}
	// Pin that the audit INSERT is inside the DEFINER (not just defined elsewhere): the
	// INSERT INTO audit_entry must appear in the same file as the function body.
	if !strings.Contains(src, "INSERT INTO audit_entry") {
		t.Fatal("MF-004-US6-AUDIT SOURCE PIN: 0047 no longer contains INSERT INTO audit_entry — complete_outbound_transition must atomically audit with the status update")
	}

	// The write tools also require connectors.write to be in the permission catalog
	// (also installed by this migration) — pin the catalog rows.
	if !strings.Contains(src, "connectors.read") {
		t.Fatal("MF-004-US6-AUDIT SOURCE PIN: 0047 no longer seeds 'connectors.read' permission — permission catalog for connector agent tools missing")
	}
	if !strings.Contains(src, "connectors.write") {
		t.Fatal("MF-004-US6-AUDIT SOURCE PIN: 0047 no longer seeds 'connectors.write' permission — permission catalog for connector agent tools missing")
	}
}
