package inbox

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// route is the single-business routing tuple a recipient address resolves to
// (T024). It carries only what ingestion needs to assert single-business scope
// (FR-017) and where to store attachment bytes — never any oracle-leaking
// existence signal.
type route struct {
	businessID    uuid.UUID
	tenantRootID  uuid.UUID
	emailDomainID uuid.UUID // uuid.Nil for a system address (no email_domain row)
}

// normalizeRecipient lowercases an inbound recipient's routing key and strips the
// plus/VERP segment of the local part (R3). The stripped segment is the carrier
// for our reply token (`support+{token}@domain` routes on `support@domain`); it
// is returned separately so threading (T025) can attempt to verify it as a reply
// token. The returned address is the routing key matched against
// inbound_address.address (citext, already normalized at store time).
//
// Only the routing key (local-part + domain) is lowercased — email addressing is
// case-insensitive. The plus segment is returned with its case INTACT: the reply
// token is case-sensitive base64url, so lowercasing it would corrupt the HMAC and
// silently kill the reply-token threading fallback (manyforge-btv).
//
// Inputs are best-effort: a malformed address with no '@' is lowercased and
// returned with an empty token; resolution will simply find no match and the
// message is dropped with the uniform no-route ack (no oracle).
func normalizeRecipient(addr string) (normalized, plusToken string) {
	addr = strings.TrimSpace(addr)
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return strings.ToLower(addr), ""
	}
	local, domain := addr[:at], addr[at+1:]
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		plusToken = local[plus+1:] // case preserved — opaque token, not a routing key
		local = local[:plus]
	}
	return strings.ToLower(local) + "@" + strings.ToLower(domain), plusToken
}

// resolveRecipient maps a normalized recipient address to its single business via
// the resolve_inbound_address SECURITY DEFINER function (0014). The function
// bypasses RLS (owner-defined) and returns at most one row: the routing tuple for
// a system address (always routes) or a custom address whose domain is verified.
//
// A no-match is reported as errNoRoute and NOTHING else — the caller treats it as
// a uniform silent-success ack, indistinguishable from a routed message, so an
// unknown recipient is never an existence oracle (FR-003/SC-006). The lookup runs
// in the caller's principal-less ingestion tx (the function is the boundary that
// re-asserts scope), matching the raw-pgx RETURNS TABLE call pattern used for the
// outbox drain functions.
func resolveRecipient(ctx context.Context, tx pgx.Tx, address string) (route, error) {
	var (
		r           route
		emailDomain *uuid.UUID // nullable: NULL for system addresses
	)
	err := tx.QueryRow(ctx,
		"SELECT business_id, tenant_root_id, email_domain_id FROM resolve_inbound_address($1)",
		address,
	).Scan(&r.businessID, &r.tenantRootID, &emailDomain)
	if err != nil {
		if err == pgx.ErrNoRows {
			return route{}, errNoRoute
		}
		return route{}, err
	}
	if emailDomain != nil {
		r.emailDomainID = *emailDomain
	}
	return r, nil
}
