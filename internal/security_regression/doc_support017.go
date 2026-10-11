package security_regression

// Spec 017 (Support Email via Resend) security-regression finding IDs. Each pin
// file carries one ID in its header comment. Two findings are split across files:
// MF-017-RECORD-AFTER-INGEST across resend_received_netsafe_pin_test.go (ingest
// before record, single record call site) and mf017_record_after_ingest_pin_test.go
// (seen-event short-circuit, no direct record); MF-017-RAW-DOWNLOAD across
// resend_received_netsafe_pin_test.go (production netsafe client) and
// mf017_raw_download_pin_test.go (the download boundary). The domain takeover is
// pinned in mf017_domain_takeover_pin_test.go.
const (
	// FindingReplyTokenSecret — production refuses to serve with an empty
	// reply-token HMAC key (R14); the migrate Job is unaffected.
	FindingReplyTokenSecret = "MF-017-REPLY-TOKEN-SECRET"
	// FindingSameBusiness — Resend-received mail routes only to verified custom
	// addresses owned by the receiving profile's business (R4).
	FindingSameBusiness = "MF-017-SAME-BUSINESS"
	// FindingRecordAfterIngest — the email.received event id is recorded only
	// after fetch and ingest succeed, so a retry is never discarded (R5).
	FindingRecordAfterIngest = "MF-017-RECORD-AFTER-INGEST"
	// FindingNoFallback — a business with a Resend support identity never sends
	// through the shared transport (R11).
	FindingNoFallback = "MF-017-NO-FALLBACK"
	// FindingRawDownload — the pre-signed raw URL is https-only, size-capped,
	// fetched through netsafe, never sent the API key, never logged.
	FindingRawDownload = "MF-017-RAW-DOWNLOAD"
	// FindingDefiners — 0139's functions are search-path-pinned DEFINERs with
	// PUBLIC execute revoked, and verified domains are globally unique (R7).
	FindingDefiners = "MF-017-DEFINERS"
)
