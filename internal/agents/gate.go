package agents

// Autonomy modes (agent.autonomy_mode; design §6). Default is ModeAssist.
const (
	ModeAssist      = 1 // auto-apply reversible internal writes; queue external/irreversible
	ModeQueueWrites = 2 // queue EVERY write; reads still run inline
	ModeAutonomous  = 3 // auto-run everything within tenant scope
)

// autonomyDecision is the gate's verdict for one already-RBAC-allowed tool call.
type autonomyDecision int

const (
	decideExec     autonomyDecision = iota // execute inline now
	decideApproval                         // record a pending approval_item; do NOT execute
)

// gate combines a tool's static effect class with the agent's autonomy mode to decide
// whether the call runs inline or is queued for human approval. It runs strictly AFTER
// RBAC (the caller has already confirmed the agent holds the tool's permission) and
// BEFORE any execution. It is deterministic and FAIL-CLOSED: an unknown/unclassified
// effect class OR an out-of-range mode defaults to approval. No LLM input influences it.
func gate(effect EffectClass, mode int) autonomyDecision {
	switch effect {
	case EffectRead:
		return decideExec // reads never mutate — always safe to run inline
	case EffectReversible:
		// Reversible internal writes auto-apply in assist/autonomous; mode 2 queues them.
		if mode == ModeAssist || mode == ModeAutonomous {
			return decideExec
		}
		return decideApproval // ModeQueueWrites, or any unknown mode → fail-closed
	case EffectExternal, EffectIrreversible:
		if mode == ModeAutonomous {
			return decideExec // fully autonomous within tenant scope
		}
		return decideApproval // assist/queue-writes, or unknown mode → fail-closed
	default:
		// FAIL-CLOSED: unknown/unclassified effect ⇒ approval (never auto-execute).
		return decideApproval
	}
}
