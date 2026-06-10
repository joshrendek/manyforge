//go:build integration

package connectors

// mf_004_us4_outbound_pin (Spec 004 US4 §7.5, manyforge-a7j.4): the ONE behavioural SSRF
// pin for the outbound dispatch path. It is co-located in package connectors (not
// security_regression) because the dispatch entry point dispatchOnce is unexported and the
// seed helpers (seedOutboundConnector, the outboundSeed struct, registerStubJira) live in
// this package's _test.go files.
//
// Finding ID: MF-004-US4-SSRF — the outbound dispatcher dials the external system only
// through Registry.BuildSystem → the SSRF-guarded netsafe client. Cloud-metadata
// (169.254.169.254) MUST be refused at DIAL time, matching FindingUS3SSRF's semantic
// ("Cloud-metadata is blocked regardless of the flag").

import (
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestMF004US4_OutboundRefusesMetadataDial pins MF-004-US4-SSRF (Spec 004 §7.5).
//
// The connector is seeded the normal way (valid loopback httptest base_url so Service.Create's
// create-time base_url validation accepts it) with AllowPrivateBaseURL=true. We then rewrite
// the stored base_url to http://169.254.169.254 (the cloud-metadata IP) DIRECTLY via Super,
// bypassing create-time validation, to isolate the SECOND line of defense: the dispatcher's
// DIAL-time SSRF guard. connector_outbound_context reads base_url straight off the connector
// row, so the dispatcher will attempt to dial the metadata IP.
//
// AllowPrivateBaseURL=true makes this pin STRONGER, not weaker: cloud-metadata must be refused
// at dial EVEN WITH the private-IP hatch open. (netsafe blocks link-local/metadata
// unconditionally; the AllowPrivate flag only opens RFC1918 + loopback.) This mirrors the
// FindingUS3SSRF semantic for the inbound path.
//
// Non-vacuity: the seed registers a REAL netsafe-backed httpStubConnector, so if the outbound
// dispatcher had used a raw http client instead of Registry.BuildSystem → netsafe, the dial
// would actually reach the metadata endpoint and the write-back would stamp the message
// external_id. Because the dial is refused UPSTREAM of any write-back, the message external_id
// must remain NULL after dispatch.
func TestMF004US4_OutboundRefusesMetadataDial(t *testing.T) {
	// MF-004-US4-SSRF — Spec 004 §7.5
	ctx, tdb, tenant := startConn(t)

	// Seed via a valid loopback URL so Service.Create's create-time base_url validation accepts
	// it (it rejects 169.254.169.254 outright — defense-in-depth layer #1). The seed sets
	// AllowPrivateBaseURL=true.
	seed := seedOutboundConnector(t, ctx, tdb, tenant, "http://127.0.0.1:9")

	// Re-point the stored base_url at the cloud-metadata IP directly (bypassing create-time
	// validation) to isolate the dial-time SSRF guard. The dispatcher reads this base_url via
	// connector_outbound_context and must refuse to dial it.
	if _, err := tdb.Super.Exec(ctx,
		`UPDATE connector SET base_url='http://169.254.169.254' WHERE id=$1`, seed.ConnectorID); err != nil {
		t.Fatalf("MF-004-US4-SSRF: repoint base_url to metadata IP: %v", err)
	}

	disp := &OutboundDispatcher{
		DB:       tdb.App,
		Sealer:   seed.Sealer,
		Registry: seed.Registry,
		Logger:   slog.Default(),
		Batch:    10,
	}

	// dispatchOnce swallows per-op errors into recordFailure (fail_outbound_op), so a refused
	// dial is NOT a fatal pass error — it returns nil. The fatal-error path would only fire on
	// a claim/scan failure, which we are not exercising here.
	if err := disp.dispatchOnce(ctx); err != nil {
		t.Fatalf("MF-004-US4-SSRF: dispatchOnce returned a fatal error (expected per-op swallow): %v", err)
	}

	// The dial was refused upstream of any write-back, so the message external_id must STILL be
	// NULL. A non-NULL value here would mean the POST reached the metadata endpoint and the
	// dispatcher stamped a comment id back — i.e. the dial-time SSRF guard was bypassed.
	// ticket_message is RLS-protected, so read it via Super (a principal-less App read sees nothing).
	var ext *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT external_id FROM ticket_message WHERE id=$1`, seed.MessageID).Scan(&ext); err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("MF-004-US4-SSRF: seeded message %s missing", seed.MessageID)
		}
		t.Fatalf("MF-004-US4-SSRF: read message external_id: %v", err)
	}
	if ext != nil && *ext != "" {
		t.Fatalf("MF-004-US4-SSRF VIOLATION: message external_id = %q after dispatch to 169.254.169.254 — the metadata dial was NOT refused (SSRF guard bypassed)", *ext)
	}

	// Belt-and-suspenders: the op must NOT be 'done'. A refused dial requeues the op via
	// recordFailure (status back to 'pending', or 'failed' once the attempt cap is hit); a
	// 'done' status would mean the external post somehow completed.
	var status string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status FROM connector_outbound_op WHERE id=$1`, seed.OpID).Scan(&status); err != nil {
		t.Fatalf("MF-004-US4-SSRF: read op status: %v", err)
	}
	if status == "done" {
		t.Fatalf("MF-004-US4-SSRF VIOLATION: op marked 'done' after dispatch to 169.254.169.254 — the metadata dial was NOT refused")
	}
}
