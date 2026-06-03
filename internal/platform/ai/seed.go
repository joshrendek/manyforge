package ai

// RegisterDefaults seeds the registry with known hosted models. Pricing is in
// integer cents per MILLION tokens (snapshot — verify against the provider
// pricing page before relying on it for billing; this is a BYO-key guardrail,
// not an invoice). Self-hosters Register local models (e.g. an ollama tag) at
// runtime; those have zero cost.
// Model.Provider holds the CREDENTIAL provider name ("anthropic"/"openai"), which
// differs from a transport's Name() ("openai-compat"); US3 cost lookup keys on
// model ID via Registry.Lookup, never by reconciling Provider against Name().
func RegisterDefaults(r *Registry) {
	for _, m := range []Model{
		{ID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true},
		{ID: "claude-opus-4-1", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 1500, OutputCentsPerMTok: 7500, SupportsTools: true},
		{ID: "claude-haiku-4-5", Provider: "anthropic", ContextWindow: 200_000, InputCentsPerMTok: 100, OutputCentsPerMTok: 500, SupportsTools: true},
		{ID: "gpt-4o", Provider: "openai", ContextWindow: 128_000, InputCentsPerMTok: 250, OutputCentsPerMTok: 1000, SupportsTools: true},
		{ID: "gpt-4o-mini", Provider: "openai", ContextWindow: 128_000, InputCentsPerMTok: 15, OutputCentsPerMTok: 60, SupportsTools: true},
	} {
		r.Register(m)
	}
}
