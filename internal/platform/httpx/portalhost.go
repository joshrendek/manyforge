package httpx

import (
	"context"

	"github.com/google/uuid"
)

type portalCtxKey struct{}

// WithPortalBusiness records the business that owns the verified custom hostname
// a request arrived on.
func WithPortalBusiness(ctx context.Context, businessID uuid.UUID) context.Context {
	return context.WithValue(ctx, portalCtxKey{}, businessID)
}

// PortalBusiness returns the business bound to the request's custom hostname, if
// the request arrived on one.
func PortalBusiness(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(portalCtxKey{}).(uuid.UUID)
	return id, ok
}
