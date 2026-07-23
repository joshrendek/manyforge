package authz

// Permission keys — the catalog seeded by the migrations under migrations/ (the
// permission.key rows). These exported constants let Go call sites reference a
// permission by symbol — the RBAC middleware wiring (cmd/manyforge) and the agent tool
// registry (internal/agents) — so a rename fails at COMPILE time instead of silently
// mis-gating an endpoint or a tool.
//
// The string VALUES must stay identical to the seeded keys; a source-level pin in
// internal/security_regression asserts every constant here appears in the migrations,
// so a Go-side rename that drifts from the SQL catalog fails CI loudly.
const (
	PermTicketsRead   = "tickets.read"
	PermTicketsReply  = "tickets.reply"
	PermTicketsWrite  = "tickets.write"
	PermTicketsAssign = "tickets.assign"
	PermTicketsDelete = "tickets.delete"
	PermInboxManage   = "inbox.manage"

	PermAgentsConfigure = "agents.configure"
	PermAgentsRun       = "agents.run"
	PermAgentsApprove   = "agents.approve"

	PermConnectorsRead   = "connectors.read"
	PermConnectorsWrite  = "connectors.write"
	PermConnectorsManage = "connectors.manage"

	PermCRMRead  = "crm.read"
	PermCRMWrite = "crm.write"

	PermFeedbackRead  = "feedback.read"
	PermFeedbackWrite = "feedback.write"
)
