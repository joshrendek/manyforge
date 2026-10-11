package mailing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

// ErrSupportProfileUnavailable means the business's Resend support profile cannot
// send right now: missing, not verified, not Resend, or its credential could not
// be opened. The caller retries and finally marks the reply failed (spec 017 R13).
var ErrSupportProfileUnavailable = errors.New("mailing: support sending profile unavailable")

// SendSupportReply sends one support-desk reply through the given business-owned
// Resend profile (spec 017 R10). It never falls back to another transport (R11).
// mailing_profile_context only returns verified profiles. The provider cache is
// keyed by the profile's updated_at, so a rotated credential is used on the next
// call. Returned errors never contain provider response bodies.
func (s *Service) SendSupportReply(ctx context.Context, profileID uuid.UUID, m notify.Mail) error {
	if s.Providers == nil {
		return ErrSupportProfileUnavailable
	}
	profile, err := s.resolveSystemProfile(ctx, profileID, false)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSupportProfileUnavailable, err)
	}
	if profile.provider.Mode != "resend" {
		return ErrSupportProfileUnavailable
	}
	deliverer, err := s.Providers.Resolve(ctx, profile.provider)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSupportProfileUnavailable, err)
	}
	if _, err := deliverer.Send(ctx, m); err != nil {
		return fmt.Errorf("mailing: support reply send: %w", err)
	}
	return nil
}
