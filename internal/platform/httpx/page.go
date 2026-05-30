// Package httpx holds shared HTTP plumbing: router/middleware, error mapping,
// and pagination helpers.
package httpx

// Pagination caps. Every list endpoint MUST clamp to MaxPageSize (FR-029).
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// ClampLimit applies the default and hard cap to a requested page size.
// Non-positive requests default; oversized requests are silently capped (never
// returning the whole table).
func ClampLimit(requested int) int {
	switch {
	case requested <= 0:
		return DefaultPageSize
	case requested > MaxPageSize:
		return MaxPageSize
	default:
		return requested
	}
}

// Page is the standard cursor-paginated response envelope.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}
