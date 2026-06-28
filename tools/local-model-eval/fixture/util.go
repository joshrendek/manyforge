package payments

import "strings"

// normalizeName trims and lowercases a name for stable comparison. This file is
// intentionally CLEAN (no planted issues) — it's noise to check whether a model
// over-reports on benign code.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
