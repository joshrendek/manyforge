// Package blob is the thin first cut of shared layer SL-E (spec 002): attachment
// object storage behind a single interface, implemented with gocloud.dev/blob
// (local filesystem default for self-host, S3-compatible optional).
//
// Storage keys are tenant-scoped so a key never crosses tenants. On ingest the
// first 512 bytes are MIME-sniffed and validated against an explicit allowlist;
// the declared Content-Type is never trusted, and per-attachment/per-message
// size caps are enforced here (defense in depth), not only at the transport edge.
package blob
