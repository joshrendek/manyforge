package coding

// reviewFallbackModel returns a faster, provider-compatible model to retry a cloud
// review with after the configured model fails (e.g. an OpenRouter 504 from a slow
// reasoning model's long time-to-first-token). "" means no fallback for this
// provider — the retry uses the same model (today's behavior). ollama/vllm run
// host-side and never hit OpenRouter, so they have no entry (manyforge-206).
func reviewFallbackModel(provider string) string {
	switch provider {
	case "openrouter":
		return "google/gemini-2.5-flash"
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4o-mini"
	default:
		return ""
	}
}

// effectiveReviewModel returns the model a review attempt should use: the configured
// model on the first attempt, and (on any retry, attempts >= 2) the provider fallback
// when one exists and differs from the configured model.
func effectiveReviewModel(provider, configuredModel string, attempts int) string {
	if attempts >= 2 {
		if fb := reviewFallbackModel(provider); fb != "" && fb != configuredModel {
			return fb
		}
	}
	return configuredModel
}
