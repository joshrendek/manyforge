# Feedback Verified-Identity Tier + Read Path — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional HMAC-signed "verified identity" tier to the existing anonymous feedback public-ingress, plus exactly-once submit and a server-truth read path (`viewer_voted` + my-submissions).

**Architecture:** A per-ingest-key secret (`fbs_…`) is sealed at rest with the existing `crypto.Sealer`. A customer's *backend* signs requests `X-Feedback-Signature: t=…,v1=HMAC_SHA256(secret,"<t>.<METHOD>.<path>.<body>")`; a valid signature marks the request `identity_verified` and namespaces its identity `v:` (vs anonymous `a:`, vs internal bare-UUID) so the three trust tiers can't squat each other against `UNIQUE (post_id, voter_identity)`. Submit gains a signed `idempotency_key` backed by a locked-down consumed-set table for exactly-once. All DB access stays through the SECURITY DEFINER functions of migration 0102 (extended by 0104).

**Tech Stack:** Go 1.x (chi, pgx v5, `crypto/hmac`, `crypto/sha256`), PostgreSQL (SECURITY DEFINER + RLS), sqlc v1.27.0, golang-migrate, Angular (signals, standalone components), Vitest, Playwright.

**Spec:** `docs/superpowers/specs/2026-07-24-feedback-verified-identity-design.md` (read it — this plan implements it).

## Global Constraints

- **Branch:** cut `006-feedback-verified-identity` fresh off `master` (at most one branch off master; the prior 006 branch is merged+deleted). Do NOT stack.
- **sqlc pin:** global sqlc MUST be v1.27.0 (repo pin) or `make generate` re-churns generated code. Never hand-edit `internal/platform/db/dbgen/*`.
- **Migration ⇒ migrate the dev DB** or the app version-guard refuses to serve. Dev DB is at **103**; apply 0104 with:
  `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`
- **Backend gates before any commit that touches Go/SQL:** `make test`, `make lint` (go vet + staticcheck), and for route/contract changes `go test -tags contract ./cmd/...`. Security tests: `make sec-test` (needs Docker/testcontainers).
- **Keep `db/schema.sql` (sqlc input) in sync with `migrations/` by hand** — they are separate sources.
- **Never log** the plaintext secret, the sealed blob, or the master key.
- **Oracle discipline:** unknown/revoked key and bad-signature both return **uniform 401** (never a key-existence differential).
- **Frontend:** verify visible UI in a real browser (gstack/Playwright) before "done"; then codify as a spec. zsh has `noclobber` (use `>|` for bg log redirects); use `docker-compose` (v1, no Compose V2).
- **Commits:** no `Co-Authored-By` trailer (per user global CLAUDE.md).

---

### Task 1: Config + Sealer wiring (`MANYFORGE_FEEDBACK_MASTER_KEY`)

Mirror `ConnectorMasterKey` exactly. Unset ⇒ nil ⇒ verified tier disabled, server still boots, anon path unaffected. Set-but-wrong-length ⇒ fatal at startup.

**Files:**
- Modify: `internal/platform/config/config.go` (add field ~near `ConnectorMasterKey` :~89; add load ~near :310)
- Test: `internal/platform/config/config_test.go` (add cases)
- Modify: `internal/feedback/board.go:23-25` (add `Sealer` to `Service`)
- Modify: `internal/feedback/public.go:40-49` (add `Sealer` to `PublicHandler` + constructor arg)
- Modify: `cmd/manyforge/main.go:183-185` (build sealer, inject)

**Interfaces:**
- Produces: `config.Config.FeedbackMasterKey []byte`; `feedback.Service.Sealer *crypto.Sealer`; `feedback.NewPublicHandler(db, logger, sealer)`; both nil-safe.

- [ ] **Step 1: Write the failing test**

In `internal/platform/config/config_test.go` add (adapt to the file's existing test style/helpers):

```go
func TestFeedbackMasterKey(t *testing.T) {
	t.Run("unset → nil, no error", func(t *testing.T) {
		t.Setenv("MANYFORGE_FEEDBACK_MASTER_KEY", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.FeedbackMasterKey != nil {
			t.Fatalf("want nil, got %d bytes", len(cfg.FeedbackMasterKey))
		}
	})
	t.Run("valid 32-byte hex → decoded", func(t *testing.T) {
		t.Setenv("MANYFORGE_FEEDBACK_MASTER_KEY", "hex:"+strings.Repeat("ab", 32))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.FeedbackMasterKey) != 32 {
			t.Fatalf("want 32 bytes, got %d", len(cfg.FeedbackMasterKey))
		}
	})
	t.Run("wrong length → hard error", func(t *testing.T) {
		t.Setenv("MANYFORGE_FEEDBACK_MASTER_KEY", "hex:abcd")
		if _, err := Load(); err == nil {
			t.Fatal("want error for short key, got nil")
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/platform/config/ -run TestFeedbackMasterKey -v`
Expected: FAIL (undefined `FeedbackMasterKey`). If `Load` needs other env to succeed, mirror what the existing config tests set.

- [ ] **Step 3: Add the config field + load**

In `config.go`, next to `ConnectorMasterKey` (the struct field ~:89), add:

```go
	// FeedbackMasterKey seals the per-ingest-key feedback secret used for HMAC-signed
	// verified ingress. Supplied via MANYFORGE_FEEDBACK_MASTER_KEY as base64 or hex; the
	// decoded value MUST be 32 bytes (AES-256). Nil/empty when unset — the verified tier is
	// disabled (anonymous ingress unaffected) and the server still boots. Set-but-wrong-length
	// is a hard config error caught here.
	FeedbackMasterKey []byte
```

Next to the `ConnectorMasterKey` load (~:310) add:

```go
	if cfg.FeedbackMasterKey, err = envKey32("MANYFORGE_FEEDBACK_MASTER_KEY"); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_FEEDBACK_MASTER_KEY: %w", err)
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/platform/config/ -run TestFeedbackMasterKey -v`
Expected: PASS.

- [ ] **Step 5: Thread the sealer into Service + PublicHandler**

`internal/feedback/board.go` — extend `Service`:

```go
type Service struct {
	DB     *db.DB
	Sealer *crypto.Sealer // nil ⇒ verified tier disabled (no secret minting)
}
```
Add import `"github.com/manyforge/manyforge/internal/platform/crypto"`.

`internal/feedback/public.go` — extend the handler + constructor:

```go
type PublicHandler struct {
	DB       *appdb.DB
	Logger   *slog.Logger
	Sealer   *crypto.Sealer // nil ⇒ signed requests against secret'd keys → 401 (fail closed)
	maxBytes int64
}

func NewPublicHandler(database *appdb.DB, logger *slog.Logger, sealer *crypto.Sealer) *PublicHandler {
	return &PublicHandler{DB: database, Logger: logger, Sealer: sealer, maxBytes: maxPublicBytes}
}
```
Add import `"github.com/manyforge/manyforge/internal/platform/crypto"`.

`cmd/manyforge/main.go:183-185` — build the sealer (nil when key unset) and inject:

```go
	var feedbackSealer *crypto.Sealer
	if len(cfg.FeedbackMasterKey) > 0 {
		feedbackSealer, err = crypto.NewSealer(cfg.FeedbackMasterKey)
		if err != nil {
			return fmt.Errorf("feedback sealer: %w", err)
		}
	}
	feedbackSvc := &feedback.Service{DB: database, Sealer: feedbackSealer}
	// ... (existing feedbackSvc wiring)
	feedbackPublicH := feedback.NewPublicHandler(database, logger, feedbackSealer)
```
(Confirm `crypto` is imported in main.go; add if missing. Match the surrounding error-return style — if `main` uses a `run() error` pattern, `return fmt.Errorf(...)`; otherwise `log.Fatal`.)

- [ ] **Step 6: Build + existing tests**

Run: `go build ./... && go test ./internal/feedback/ ./internal/platform/config/`
Expected: PASS (all callers of `NewPublicHandler` updated — grep to confirm none missed: `grep -rn NewPublicHandler --include=*.go`).

- [ ] **Step 7: Commit**

```bash
git add internal/platform/config/ internal/feedback/board.go internal/feedback/public.go cmd/manyforge/main.go
git commit -m "feat(006): wire MANYFORGE_FEEDBACK_MASTER_KEY + inject feedback sealer (saz.5)"
```

---

### Task 2: Migration 0104 — columns, backfill, consumed-set, DEFINER changes

**Files:**
- Create: `migrations/0104_feedback_verified_identity.up.sql`
- Create: `migrations/0104_feedback_verified_identity.down.sql`

**Interfaces:**
- Produces (DB): `feedback_public_board(text)` now returns `(board_id, business_id, tenant_root_id, is_public, key_id, sealed_secret)`; `feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea)` returns `TABLE(post_id uuid, deduped boolean)` (RAISEs SQLSTATE `FB409` on same-key-different-body); `feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean)`; `feedback_public_list_posts(uuid,int,text,text)` returns `(…, viewer_voted boolean, identity_verified boolean)`. New table `feedback_ingest_idempotency`. New columns `feedback_post.identity_verified`, `feedback_vote.identity_verified`, `feedback_ingest_key.sealed_secret`.

- [ ] **Step 1: Write `0104_feedback_verified_identity.up.sql`**

```sql
-- 0104: feedback verified-identity tier + exactly-once submit + read path (Spec 006 / saz.5).
-- Extends the 0102 public-ingress DEFINER surface. See
-- docs/superpowers/specs/2026-07-24-feedback-verified-identity-design.md.

BEGIN;

-- 1. Columns ---------------------------------------------------------------------------
ALTER TABLE feedback_post ADD COLUMN identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE feedback_vote ADD COLUMN identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE feedback_ingest_key ADD COLUMN sealed_secret text NULL;

-- 2. Backfill legacy PUBLIC identities into the `a:` namespace, leaving INTERNAL
--    principal-UUID rows raw (votes carry no author-kind column, so principal membership is
--    the only discriminator; the a:/v: prefixes never appear in a UUID, so the three
--    namespaces are disjoint going forward). A public anon voter who supplied a real
--    principal's UUID as their identity was ALREADY a collision pre-0104, so leaving such
--    rows raw neither creates nor worsens exposure.
UPDATE feedback_vote
   SET voter_identity = 'a:' || voter_identity
 WHERE voter_identity NOT IN (SELECT id::text FROM principal);

-- feedback_post_author_chk guarantees principal posts have author_identity IS NULL, so
-- author_kind='public' is an exact, safe discriminator (no principal-UUID ambiguity).
UPDATE feedback_post
   SET author_identity = 'a:' || author_identity
 WHERE author_kind = 'public' AND author_identity IS NOT NULL;

-- 3. Consumed-set for exactly-once submit. LOCKED DOWN: RLS on, NO policies, NO app grants —
--    only the SECURITY DEFINER functions (running as owner → bypass RLS) read/write it. Its
--    idem_key values often embed customer-side user/order identifiers, so no principal path
--    may read it (unlike the 0102 feedback tables which are RLS-enabled WITH per-tenant
--    policies + app grants).
CREATE TABLE feedback_ingest_idempotency (
    key_id         uuid        NOT NULL REFERENCES feedback_ingest_key(id) ON DELETE CASCADE,
    idem_key       text        NOT NULL,   -- ALREADY tier-namespaced (a:/v:) by the DEFINER
    business_id    uuid        NOT NULL,
    tenant_root_id uuid        NOT NULL,
    body_sha256    bytea       NOT NULL,
    post_id        uuid        NULL,        -- backfilled after the post is created
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key_id, idem_key)
);
ALTER TABLE feedback_ingest_idempotency ENABLE ROW LEVEL SECURITY;  -- no policy, no grant
CREATE TRIGGER feedback_ingest_idempotency_tenant_root_immutable
    BEFORE UPDATE ON feedback_ingest_idempotency
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- 4. DEFINER functions: DROP + CREATE (return/arg shapes change). Re-REVOKE/GRANT each. All
--    stay SECURITY DEFINER SET search_path = public and VOLATILE (a STABLE annotation would
--    freeze the claim-first snapshot below).

-- 4a. board lookup now also returns key_id + sealed_secret.
DROP FUNCTION feedback_public_board(text);
CREATE FUNCTION feedback_public_board(p_key text)
RETURNS TABLE(board_id uuid, business_id uuid, tenant_root_id uuid, is_public boolean,
              key_id uuid, sealed_secret text)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.business_id, b.tenant_root_id, b.is_public, k.id, k.sealed_secret
    FROM feedback_ingest_key k
    JOIN feedback_board b ON b.id = k.board_id
    WHERE k.publishable_key = p_key AND k.status = 'enabled' AND b.is_public;
$$;
REVOKE ALL ON FUNCTION feedback_public_board(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_board(text) TO manyforge_app;

-- 4b. submit: tier-prefix author identity from p_verified; exactly-once via claim-first when
--     p_idem_key non-empty. Returns (post_id, deduped); RAISEs FB409 on same-key-different-body.
DROP FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text);
CREATE FUNCTION feedback_public_submit(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_title text, p_body text, p_author_identity text,
    p_verified boolean, p_key_id uuid, p_idem_key text, p_body_sha256 bytea
) RETURNS TABLE(post_id uuid, deduped boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_id            uuid;
    v_ik            text;
    v_rows          int;
    v_existing_post uuid;
    v_existing_hash bytea;
    v_prefix        text := CASE WHEN p_verified THEN 'v:' ELSE 'a:' END;
    v_raw_identity  text := NULLIF(btrim(coalesce(p_author_identity, '')), '');
    v_identity      text;  -- NULL author stays NULL (no bare-prefix row)
BEGIN
    IF v_raw_identity IS NOT NULL THEN
        v_identity := v_prefix || left(v_raw_identity, 200);
    END IF;

    IF NULLIF(btrim(coalesce(p_idem_key, '')), '') IS NULL THEN
        INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body,
            status, vote_count, author_kind, author_identity, identity_verified, created_at, updated_at)
        VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
            left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
            'open', 0, 'public', v_identity, p_verified, now(), now())
        RETURNING id INTO v_id;
        post_id := v_id; deduped := false; RETURN NEXT; RETURN;
    END IF;

    v_ik := v_prefix || left(p_idem_key, 255);

    INSERT INTO feedback_ingest_idempotency (key_id, idem_key, business_id, tenant_root_id, body_sha256)
    VALUES (p_key_id, v_ik, p_business_id, p_tenant_root, p_body_sha256)
    ON CONFLICT (key_id, idem_key) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;

    IF v_rows = 0 THEN
        -- replay. The ON CONFLICT above blocked on the PK until any concurrent first claimant
        -- committed, so this read (READ COMMITTED, fresh snapshot) sees the backfilled post_id.
        SELECT fi.post_id, fi.body_sha256 INTO v_existing_post, v_existing_hash
        FROM feedback_ingest_idempotency fi
        WHERE fi.key_id = p_key_id AND fi.idem_key = v_ik;
        IF v_existing_hash IS DISTINCT FROM p_body_sha256 THEN
            RAISE EXCEPTION 'feedback idempotency key reused with different body'
                USING ERRCODE = 'FB409';
        END IF;
        post_id := v_existing_post; deduped := true; RETURN NEXT; RETURN;
    END IF;

    -- claimed: create the post, backfill post_id (all one tx → no committed NULL post_id).
    INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body,
        status, vote_count, author_kind, author_identity, identity_verified, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
        left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
        'open', 0, 'public', v_identity, p_verified, now(), now())
    RETURNING id INTO v_id;
    UPDATE feedback_ingest_idempotency SET post_id = v_id
        WHERE key_id = p_key_id AND idem_key = v_ik;
    post_id := v_id; deduped := false; RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea) TO manyforge_app;

-- 4c. vote: tier-prefix voter identity from p_verified; set the vote's identity_verified.
DROP FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text);
CREATE FUNCTION feedback_public_vote(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid,
    p_post_id uuid, p_voter_identity text, p_verified boolean
) RETURNS TABLE(accepted boolean, out_votes int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_rows  int;
    v_count int;
    v_vid   text := (CASE WHEN p_verified THEN 'v:' ELSE 'a:' END)
                    || left(btrim(coalesce(p_voter_identity, '')), 200);
BEGIN
    PERFORM 1 FROM feedback_post
        WHERE id = p_post_id AND board_id = p_board_id AND deleted_at IS NULL;
    IF NOT FOUND THEN
        accepted := false; out_votes := NULL; RETURN NEXT; RETURN;
    END IF;

    INSERT INTO feedback_vote (id, business_id, tenant_root_id, post_id, voter_identity,
                              identity_verified, created_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_post_id, v_vid, p_verified, now())
    ON CONFLICT (post_id, voter_identity) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;

    IF v_rows > 0 THEN
        UPDATE feedback_post SET vote_count = vote_count + 1, updated_at = now()
            WHERE id = p_post_id RETURNING vote_count INTO v_count;
        accepted := true;
    ELSE
        SELECT fp.vote_count INTO v_count FROM feedback_post fp WHERE fp.id = p_post_id;
        accepted := false;
    END IF;
    out_votes := v_count;
    RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean) TO manyforge_app;

-- 4d. list: add viewer_voted + identity_verified; optional author filter. Handler passes
--     already-namespaced p_viewer / p_author.
DROP FUNCTION feedback_public_list_posts(uuid,int);
CREATE FUNCTION feedback_public_list_posts(p_board_id uuid, p_limit int, p_viewer text, p_author text)
RETURNS TABLE(id uuid, title text, body text, status feedback_status, vote_count int,
              created_at timestamptz, viewer_voted boolean, identity_verified boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT fp.id, fp.title, fp.body, fp.status, fp.vote_count, fp.created_at,
           (p_viewer IS NOT NULL AND EXISTS (
               SELECT 1 FROM feedback_vote v WHERE v.post_id = fp.id AND v.voter_identity = p_viewer)),
           fp.identity_verified
    FROM feedback_post fp
    WHERE fp.board_id = p_board_id AND fp.deleted_at IS NULL
      AND (p_author IS NULL OR fp.author_identity = p_author)
    ORDER BY fp.vote_count DESC, fp.created_at DESC, fp.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 20), 1), 100);
$$;
REVOKE ALL ON FUNCTION feedback_public_list_posts(uuid,int,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_list_posts(uuid,int,text,text) TO manyforge_app;

COMMIT;
```

- [ ] **Step 2: Write `0104_feedback_verified_identity.down.sql`**

```sql
BEGIN;

-- Recreate original 0102 function signatures verbatim.
DROP FUNCTION feedback_public_list_posts(uuid,int,text,text);
CREATE FUNCTION feedback_public_list_posts(p_board_id uuid, p_limit int)
RETURNS TABLE(id uuid, title text, body text, status feedback_status, vote_count int, created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT fp.id, fp.title, fp.body, fp.status, fp.vote_count, fp.created_at
    FROM feedback_post fp
    WHERE fp.board_id = p_board_id AND fp.deleted_at IS NULL
    ORDER BY fp.vote_count DESC, fp.created_at DESC, fp.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 20), 1), 100);
$$;
REVOKE ALL ON FUNCTION feedback_public_list_posts(uuid,int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_list_posts(uuid,int) TO manyforge_app;

DROP FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text,boolean);
CREATE FUNCTION feedback_public_vote(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid, p_post_id uuid, p_voter_identity text
) RETURNS TABLE(accepted boolean, out_votes int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_rows int; v_count int;
BEGIN
    PERFORM 1 FROM feedback_post WHERE id = p_post_id AND board_id = p_board_id AND deleted_at IS NULL;
    IF NOT FOUND THEN accepted := false; out_votes := NULL; RETURN NEXT; RETURN; END IF;
    INSERT INTO feedback_vote (id, business_id, tenant_root_id, post_id, voter_identity, created_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_post_id, p_voter_identity, now())
    ON CONFLICT (post_id, voter_identity) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    IF v_rows > 0 THEN
        UPDATE feedback_post SET vote_count = vote_count + 1, updated_at = now()
            WHERE id = p_post_id RETURNING vote_count INTO v_count;
        accepted := true;
    ELSE
        SELECT fp.vote_count INTO v_count FROM feedback_post fp WHERE fp.id = p_post_id;
        accepted := false;
    END IF;
    out_votes := v_count; RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_vote(uuid,uuid,uuid,uuid,text) TO manyforge_app;

DROP FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text,boolean,uuid,text,bytea);
CREATE FUNCTION feedback_public_submit(
    p_board_id uuid, p_business_id uuid, p_tenant_root uuid, p_title text, p_body text, p_author_identity text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_id uuid;
BEGIN
    INSERT INTO feedback_post (id, business_id, tenant_root_id, board_id, title, body, status,
        vote_count, author_kind, author_identity, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, p_board_id,
        left(btrim(p_title), 300), NULLIF(btrim(coalesce(p_body, '')), ''),
        'open', 0, 'public', NULLIF(btrim(coalesce(p_author_identity, '')), ''), now(), now())
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$;
REVOKE ALL ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_submit(uuid,uuid,uuid,text,text,text) TO manyforge_app;

DROP FUNCTION feedback_public_board(text);
CREATE FUNCTION feedback_public_board(p_key text)
RETURNS TABLE(board_id uuid, business_id uuid, tenant_root_id uuid, is_public boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT b.id, b.business_id, b.tenant_root_id, b.is_public
    FROM feedback_ingest_key k JOIN feedback_board b ON b.id = k.board_id
    WHERE k.publishable_key = p_key AND k.status = 'enabled' AND b.is_public;
$$;
REVOKE ALL ON FUNCTION feedback_public_board(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION feedback_public_board(text) TO manyforge_app;

-- Strip the a:/v: identity prefixes (best-effort; a legacy raw identity that genuinely began
-- 'a:'/'v:' would be over-stripped — vanishingly unlikely, acceptable for a down path).
UPDATE feedback_vote SET voter_identity = substring(voter_identity from 3)
 WHERE voter_identity LIKE 'a:%' OR voter_identity LIKE 'v:%';
UPDATE feedback_post SET author_identity = substring(author_identity from 3)
 WHERE author_identity LIKE 'a:%' OR author_identity LIKE 'v:%';

DROP TABLE feedback_ingest_idempotency;
ALTER TABLE feedback_ingest_key DROP COLUMN sealed_secret;
ALTER TABLE feedback_vote DROP COLUMN identity_verified;
ALTER TABLE feedback_post DROP COLUMN identity_verified;

COMMIT;
```

- [ ] **Step 3: Apply up + down + up against the dev DB (round-trip)**

Run:
```bash
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" down 1
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up
```
Expected: all three succeed with no error (proves up, down, and re-up are all valid). Leave the DB **at 0104** (final `up`).

- [ ] **Step 4: Commit**

```bash
git add migrations/0104_feedback_verified_identity.up.sql migrations/0104_feedback_verified_identity.down.sql
git commit -m "feat(006): migration 0104 — verified identity + exactly-once + read path (saz.5)"
```

---

### Task 3: sqlc — `sealed_secret` on `feedback_ingest_key`

Only `ingestkey.go` uses dbgen (the public functions are called via raw SQL, so they need no sqlc changes). Add `sealed_secret` to the schema + insert query so `CreateIngestKey` can store it and `toIngestKey` can expose `has_secret`. Also mirror the 0104 schema into `db/schema.sql` for parity.

**Files:**
- Modify: `db/schema.sql:857-870` (add `sealed_secret`; add the new table + column mirrors)
- Modify: `db/query/feedback.sql:100-103` (InsertFeedbackIngestKey)
- Regenerate: `internal/platform/db/dbgen/*` via `make generate`

**Interfaces:**
- Produces: `dbgen.FeedbackIngestKey.SealedSecret *string`; `dbgen.InsertFeedbackIngestKeyParams.SealedSecret *string`.

- [ ] **Step 1: Update `db/schema.sql`**

In `feedback_ingest_key` (line ~866, after `revoked_at`) add:

```sql
    sealed_secret   text,
```

At the tail of the feedback section (after the ingest-key table), add the new column defaults + table so the sqlc schema matches 0104:

```sql
ALTER TABLE feedback_post ADD COLUMN IF NOT EXISTS identity_verified boolean NOT NULL DEFAULT false;
ALTER TABLE feedback_vote ADD COLUMN IF NOT EXISTS identity_verified boolean NOT NULL DEFAULT false;

CREATE TABLE feedback_ingest_idempotency (
    key_id         uuid NOT NULL,
    idem_key       text NOT NULL,
    business_id    uuid NOT NULL,
    tenant_root_id uuid NOT NULL,
    body_sha256    bytea NOT NULL,
    post_id        uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key_id, idem_key)
);
```
(Match however `db/schema.sql` orders `ALTER`/`CREATE` vs the base `CREATE TABLE` — if the file is a flat concatenation of migrations, appending is fine. If it declares each table once in final form, instead add the `identity_verified` columns inline to `feedback_post`/`feedback_vote` and place the new table near the others.)

- [ ] **Step 2: Update `db/query/feedback.sql` InsertFeedbackIngestKey**

Replace lines 100-103:

```sql
-- name: InsertFeedbackIngestKey :one
INSERT INTO feedback_ingest_key (id, business_id, tenant_root_id, board_id, publishable_key, label, sealed_secret, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'enabled', now())
RETURNING *;
```

- [ ] **Step 3: Regenerate**

Run: `make generate`
Expected: `internal/platform/db/dbgen/` updates — `FeedbackIngestKey` gains `SealedSecret *string`, `InsertFeedbackIngestKeyParams` gains `SealedSecret *string`. If a large unrelated churn appears, the global sqlc is not v1.27.0 — stop and fix the version.

- [ ] **Step 4: Verify build (dbgen wired but not yet consumed → still compiles)**

Run: `go build ./...`
Expected: PASS (the new `SealedSecret` param is optional/pointer; existing `CreateIngestKey` call omits it → nil, compiles). If sqlc made it non-pointer, adjust the column to nullable so it generates `*string`.

- [ ] **Step 5: Commit**

```bash
git add db/schema.sql db/query/feedback.sql internal/platform/db/dbgen/
git commit -m "feat(006): sqlc — sealed_secret on feedback_ingest_key (saz.5)"
```

---

### Task 4: Mint + seal the `fbs_` secret at key creation

**Files:**
- Modify: `internal/feedback/ingestkey.go` (`CreateIngestKey`, `toIngestKey`, add `newSecret`)
- Modify: `internal/feedback/types.go:83-93` (`IngestKey.HasSecret`) + add create-response carrier
- Test: `internal/feedback/ingestkey_test.go` (create with/without sealer)

**Interfaces:**
- Consumes: `Service.Sealer` (Task 1), `dbgen…SealedSecret` (Task 3).
- Produces: `CreateIngestKey` returns `IngestKey` whose `Secret` is set once when a sealer is present; `IngestKey.HasSecret bool`.

- [ ] **Step 1: Write the failing test**

`internal/feedback/ingestkey_test.go` (create the file if absent; use the package's existing test harness for `*Service` + a DB — mirror `board_test.go` / `ingestkey`-adjacent tests). If unit-testing against a DB is heavy here, assert the pure minting helper instead:

```go
func TestNewSecretShape(t *testing.T) {
	s, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "fbs_") {
		t.Fatalf("want fbs_ prefix, got %q", s)
	}
	if len(s) < 20 {
		t.Fatalf("secret too short: %q", s)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/feedback/ -run TestNewSecretShape -v`
Expected: FAIL (undefined `newSecret`).

- [ ] **Step 3: Implement `newSecret` + minting in `CreateIngestKey`**

In `ingestkey.go` add near `newPublishableKey`:

```go
// secretPrefix marks a feedback ingest SECRET (fbs_) — a server-to-server signing secret,
// distinct from the publishable fbk_ key so the two are never confused in config. Never ships
// to a client; returned in plaintext exactly once at creation, then only its sealed form is stored.
const secretPrefix = "fbs_"

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("feedback: secret generation: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
```

In `CreateIngestKey`, after minting `pk`, mint+seal the secret only when a sealer is present, and thread it into the insert + response:

```go
	var secretPlain string
	var sealed *string
	if s.Sealer != nil {
		sec, serr := newSecret()
		if serr != nil {
			return IngestKey{}, serr
		}
		blob, berr := s.Sealer.Seal([]byte(sec))
		if berr != nil {
			return IngestKey{}, fmt.Errorf("feedback: seal secret: %w", berr)
		}
		secretPlain = sec
		sealed = &blob
	}
```
Add `SealedSecret: sealed,` to the `InsertFeedbackIngestKeyParams`. After `out = toIngestKey(row)` set the write-once plaintext: `out.Secret = secretPlain`.

- [ ] **Step 4: Add fields to types + `toIngestKey`**

`internal/feedback/types.go` — extend `IngestKey`:

```go
	Secret         string     `json:"secret,omitempty"`     // write-once, only on create; never persisted plaintext
	HasSecret      bool       `json:"has_secret"`
```

`toIngestKey` — set `HasSecret: k.SealedSecret != nil` (leave `Secret` zero; only `CreateIngestKey` fills it).

- [ ] **Step 5: Run to verify it passes + build**

Run: `go test ./internal/feedback/ -run TestNewSecretShape -v && go build ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/feedback/ingestkey.go internal/feedback/types.go internal/feedback/ingestkey_test.go
git commit -m "feat(006): mint+seal fbs_ secret at ingest-key creation; has_secret (saz.5)"
```

---

### Task 5: Signature verification (pure, TDD)

**Files:**
- Create: `internal/feedback/signature.go`
- Test: `internal/feedback/signature_test.go`

**Interfaces:**
- Produces: `errNoSignature`; `verifyFeedbackSignature(header, secret, method, path string, body []byte, now time.Time) error` — returns `nil` (verified), `errNoSignature` (absent → anon), or another error (present-but-bad → 401). Uses `hmac.Equal` (constant-time).

- [ ] **Step 1: Write the failing test**

`internal/feedback/signature_test.go`:

```go
package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

func sign(t int64, secret, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(feedbackSigningString(t, method, path, body))
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyFeedbackSignature(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret, method, path := "fbs_abc", "POST", "/feedback/public/fbk_x/posts"
	body := []byte(`{"title":"hi","idempotency_key":"k1"}`)

	t.Run("valid", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, body, now); err != nil {
			t.Fatalf("want verified, got %v", err)
		}
	})
	t.Run("absent → errNoSignature", func(t *testing.T) {
		if err := verifyFeedbackSignature("", secret, method, path, body, now); !errors.Is(err, errNoSignature) {
			t.Fatalf("want errNoSignature, got %v", err)
		}
	})
	t.Run("tampered body → error", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, []byte(`{"title":"HACKED"}`), now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want bad-sig error, got %v", err)
		}
	})
	t.Run("wrong path → error (method+path binding)", func(t *testing.T) {
		h := sign(now.Unix(), secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, "/feedback/public/fbk_x/posts/OTHER/votes", body, now); err == nil {
			t.Fatal("want error for path mismatch")
		}
	})
	t.Run("expired → error", func(t *testing.T) {
		h := sign(now.Unix()-301, secret, method, path, body)
		if err := verifyFeedbackSignature(h, secret, method, path, body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want expired error, got %v", err)
		}
	})
	t.Run("malformed header → error", func(t *testing.T) {
		if err := verifyFeedbackSignature("garbage", secret, method, path, body, now); err == nil || errors.Is(err, errNoSignature) {
			t.Fatalf("want malformed error, got %v", err)
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/feedback/ -run TestVerifyFeedbackSignature -v`
Expected: FAIL (undefined `feedbackSigningString`/`verifyFeedbackSignature`).

- [ ] **Step 3: Implement `signature.go`**

```go
package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// errNoSignature ⇒ the X-Feedback-Signature header was absent → treat the request as anonymous.
var errNoSignature = errors.New("feedback: no signature header")

// sigMaxSkew bounds replay: a signature timestamp farther than this from now is rejected.
const sigMaxSkew = 300 * time.Second

// feedbackSigningString is the exact byte string a verified caller signs:
// "<t>.<METHOD>.<path>.<body>". Binding method+path stops a captured MAC from being replayed
// against a different endpoint/post; the (signed) body carries any idempotency_key.
func feedbackSigningString(t int64, method, path string, body []byte) []byte {
	head := strconv.FormatInt(t, 10) + "." + method + "." + path + "."
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, body...)
	return out
}

// verifyFeedbackSignature validates the X-Feedback-Signature header against secret over the
// signing string. nil = verified; errNoSignature = absent (→ anon); any other error = present
// but invalid/expired/malformed (→ 401). Constant-time compare via hmac.Equal.
func verifyFeedbackSignature(header, secret, method, path string, body []byte, now time.Time) error {
	if strings.TrimSpace(header) == "" {
		return errNoSignature
	}
	var tsStr, v1 string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			tsStr = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if tsStr == "" || v1 == "" {
		return errors.New("feedback: malformed signature header")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("feedback: bad signature timestamp")
	}
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > sigMaxSkew {
		return errors.New("feedback: signature expired")
	}
	want, err := hex.DecodeString(v1)
	if err != nil {
		return errors.New("feedback: bad signature encoding")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(feedbackSigningString(ts, method, path, body))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("feedback: signature mismatch")
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/feedback/ -run TestVerifyFeedbackSignature -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/feedback/signature.go internal/feedback/signature_test.go
git commit -m "feat(006): HMAC signature verify (method+path+body, replay window) (saz.5)"
```

---

### Task 6: `submit` — signature matrix + exactly-once

**Files:**
- Modify: `internal/feedback/public.go` (`publicBoard`, `resolveBoard`, add `resolveVerified` + `readBody`, rewrite `submit`)
- Test: covered by Task 10 (integration/regression); build + a handler smoke test here.

**Interfaces:**
- Consumes: `verifyFeedbackSignature` (Task 5), extended DEFINERs (Task 2), `Sealer` (Task 1).
- Produces: `publicBoard{boardID, businessID, tenantRoot, keyID uuid.UUID; sealedSecret *string}`; `(h *PublicHandler) resolveVerified(r, raw, sealedSecret) (verified, sigBad bool)`; `(h *PublicHandler) readBody(w, r) ([]byte, bool)`. Submit request body adds `idempotency_key`; response adds `identity_verified` + `deduped`; 201 fresh / 200 replay / 409 same-key-different-body.

- [ ] **Step 1: Extend `publicBoard` + `resolveBoard`**

```go
type publicBoard struct {
	boardID, businessID, tenantRoot, keyID uuid.UUID
	sealedSecret                           *string
}

func (h *PublicHandler) resolveBoard(r *http.Request, tx pgx.Tx, key string) (publicBoard, bool, error) {
	var b publicBoard
	var isPublic bool
	err := tx.QueryRow(r.Context(),
		`SELECT board_id, business_id, tenant_root_id, is_public, key_id, sealed_secret
		   FROM feedback_public_board($1)`, key,
	).Scan(&b.boardID, &b.businessID, &b.tenantRoot, &isPublic, &b.keyID, &b.sealedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicBoard{}, false, nil
	}
	if err != nil {
		return publicBoard{}, false, err
	}
	return b, true, nil
}
```

- [ ] **Step 2: Add `readBody` + `resolveVerified` (the §3 fail-closed matrix)**

```go
// readBody reads the capped raw request body (needed verbatim for the signature base + body
// hash). Writes 413/400 and returns false on cap/read failure.
func (h *PublicHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "payload too large")
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid request")
		return nil, false
	}
	return raw, true
}

// resolveVerified applies the §3 fail-closed matrix. verified=true ⇒ v: namespace; sigBad=true
// ⇒ caller must answer 401. raw is the exact request body (nil for GET). Call only after a known
// key (resolveBoard ok=true).
func (h *PublicHandler) resolveVerified(r *http.Request, raw []byte, sealedSecret *string) (verified, sigBad bool) {
	header := r.Header.Get("X-Feedback-Signature")
	if strings.TrimSpace(header) == "" {
		return false, false // no signature → anon
	}
	if sealedSecret == nil {
		h.Logger.WarnContext(r.Context(), "feedback/public: signature on key without secret")
		return false, false // nothing to verify → anon
	}
	if h.Sealer == nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: signature but sealer disabled")
		return false, true // secret exists but can't unseal → fail closed
	}
	secret, err := h.Sealer.Open(*sealedSecret)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: unseal failed")
		return false, true
	}
	if verr := verifyFeedbackSignature(header, string(secret), r.Method, r.URL.Path, raw, time.Now()); verr != nil {
		return false, true // present-but-bad → 401
	}
	return true, false
}
```
Add imports: `"crypto/sha256"`, `"strings"`, and `"github.com/jackc/pgx/v5/pgconn"` (used in submit). Remove the now-unused `decode` if no caller remains (vote is rewritten in Task 7 to use `readBody`); otherwise keep it.

- [ ] **Step 3: Rewrite `submit`**

```go
func (h *PublicHandler) submit(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body struct {
		Title          string `json:"title"`
		Body           string `json:"body"`
		AuthorIdentity string `json:"author_identity"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body")
			return
		}
	}
	title := trimTo(body.Title)
	if title == "" || len(title) > maxTitleLen {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "title required (1.."+strconv.Itoa(maxTitleLen)+" chars)")
		return
	}
	if len(body.Body) > maxBodyLen {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "body too long")
		return
	}
	if len(body.IdempotencyKey) > 255 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "idempotency_key too long")
		return
	}
	if len(body.AuthorIdentity) > 200 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "author_identity too long")
		return
	}
	bodyHash := sha256.Sum256(raw)

	var postID uuid.UUID
	var known, verified, sigBad, deduped bool
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil // known stays false → 401
		}
		known = true
		v, bad := h.resolveVerified(r, raw, b.sealedSecret)
		if bad {
			sigBad = true
			return nil // do not submit
		}
		verified = v
		return tx.QueryRow(r.Context(),
			`SELECT post_id, deduped FROM feedback_public_submit($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			b.boardID, b.businessID, b.tenantRoot, title, body.Body, body.AuthorIdentity,
			verified, b.keyID, body.IdempotencyKey, bodyHash[:],
		).Scan(&postID, &deduped)
	})
	if txErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(txErr, &pgErr) && pgErr.Code == "FB409" {
			writeErr(w, http.StatusConflict, "CONFLICT", "idempotency key reused with a different request")
			return
		}
		h.Logger.ErrorContext(r.Context(), "feedback/public: submit tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if sigBad {
		writeUnauthorized(w)
		return
	}
	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, map[string]any{
		"id": postID.String(), "title": title, "status": "open", "vote_count": 0,
		"identity_verified": verified, "deduped": deduped,
	})
}
```

- [ ] **Step 4: Build + existing feedback tests**

Run: `go build ./... && go test ./internal/feedback/`
Expected: PASS. (Behavioral coverage lands in Task 10.)

- [ ] **Step 5: Commit**

```bash
git add internal/feedback/public.go
git commit -m "feat(006): submit — signature matrix + exactly-once idempotency (saz.5)"
```

---

### Task 7: `vote` — signature + verified namespace

**Files:**
- Modify: `internal/feedback/public.go` (`vote`)

**Interfaces:**
- Consumes: `readBody`, `resolveVerified`, `resolveBoard` (Task 6); vote DEFINER (Task 2).
- Produces: vote response adds `identity_verified`.

- [ ] **Step 1: Rewrite `vote`**

```go
func (h *PublicHandler) vote(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	postID, perr := uuid.Parse(chi.URLParam(r, "postID"))
	if perr != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body struct {
		VoterIdentity string `json:"voter_identity"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body")
			return
		}
	}
	vid := trimTo(body.VoterIdentity)
	if vid == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "voter_identity required")
		return
	}
	if len(vid) > 200 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "voter_identity too long")
		return
	}

	var known, verified, sigBad, accepted bool
	var count *int32
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		known = true
		v, bad := h.resolveVerified(r, raw, b.sealedSecret)
		if bad {
			sigBad = true
			return nil
		}
		verified = v
		return tx.QueryRow(r.Context(),
			`SELECT accepted, out_votes FROM feedback_public_vote($1,$2,$3,$4,$5,$6)`,
			b.boardID, b.businessID, b.tenantRoot, postID, vid, verified,
		).Scan(&accepted, &count)
	})
	if txErr != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: vote tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if sigBad {
		writeUnauthorized(w)
		return
	}
	if count == nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"voted": accepted, "vote_count": *count, "identity_verified": verified,
	})
}
```

- [ ] **Step 2: Build + tests**

Run: `go build ./... && go test ./internal/feedback/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/feedback/public.go
git commit -m "feat(006): vote — signature + verified namespace (saz.5)"
```

---

### Task 8: `list` — read path (`viewer_voted` + author filter)

**Files:**
- Modify: `internal/feedback/public.go` (`publicPost`, `list`, add `namespacedParam` helper)

**Interfaces:**
- Consumes: `resolveBoard`, `resolveVerified`; list DEFINER (Task 2).
- Produces: list items add `viewer_voted` + `identity_verified`; query params `voter_identity`, `author` (namespaced by verified state, capped 200).

- [ ] **Step 1: Extend `publicPost`**

```go
type publicPost struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Body             *string `json:"body,omitempty"`
	Status           string  `json:"status"`
	VoteCount        int     `json:"vote_count"`
	CreatedAt        string  `json:"created_at"`
	ViewerVoted      bool    `json:"viewer_voted"`
	IdentityVerified bool    `json:"identity_verified"`
}
```

- [ ] **Step 2: Rewrite `list`**

```go
// namespacedParam prefixes a read identity with the caller's authoritative tier (a:/v:) and
// caps to 200 — matching the write-side transform so it can match a stored identity. Returns
// nil for an empty value (→ SQL NULL → no filter / viewer_voted=false).
func namespacedParam(v string, verified bool) *string {
	v = trimTo(v)
	if v == "" {
		return nil
	}
	if len(v) > 200 {
		v = v[:200]
	}
	prefix := "a:"
	if verified {
		prefix = "v:"
	}
	s := prefix + v
	return &s
}

func (h *PublicHandler) list(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n // the DEFINER clamps to [1,100]
		}
	}

	var known, sigBad bool
	var items []publicPost
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		known = true
		verified, bad := h.resolveVerified(r, nil, b.sealedSecret) // GET: empty body
		if bad {
			sigBad = true
			return nil
		}
		pViewer := namespacedParam(r.URL.Query().Get("voter_identity"), verified)
		pAuthor := namespacedParam(r.URL.Query().Get("author"), verified)

		rows, qerr := tx.Query(r.Context(),
			`SELECT id, title, body, status, vote_count, created_at, viewer_voted, identity_verified
			   FROM feedback_public_list_posts($1, $2, $3, $4)`,
			b.boardID, limit, pViewer, pAuthor)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id         uuid.UUID
				title      string
				bodyText   *string
				status     string
				voteCount  int32
				createdAt  time.Time
				viewerVote bool
				idVerified bool
			)
			if err := rows.Scan(&id, &title, &bodyText, &status, &voteCount, &createdAt, &viewerVote, &idVerified); err != nil {
				return err
			}
			items = append(items, publicPost{
				ID: id.String(), Title: title, Body: bodyText, Status: status,
				VoteCount: int(voteCount), CreatedAt: createdAt.UTC().Format(rfc3339),
				ViewerVoted: viewerVote, IdentityVerified: idVerified,
			})
		}
		return rows.Err()
	})
	if txErr != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: list tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if sigBad {
		writeUnauthorized(w)
		return
	}
	if items == nil {
		items = []publicPost{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
```

- [ ] **Step 3: Build + tests + lint**

Run: `go build ./... && go test ./internal/feedback/ && make lint`
Expected: PASS (no unused imports/vars; `pgconn` used in submit; `sha256` used).

- [ ] **Step 4: Commit**

```bash
git add internal/feedback/public.go
git commit -m "feat(006): list — viewer_voted + author filter read path (saz.5)"
```

---

### Task 9: OpenAPI contract + drift test

**Files:**
- Modify: `specs/006-feedback-boards/contracts/openapi.yaml` (paths at :253 submit, :290 votes, list op, key create/list schemas)
- Modify: `cmd/manyforge/drift_006_test.go`

**Interfaces:**
- Produces: contract parity so `go test -tags contract ./cmd/...` passes.

- [ ] **Step 1: Update `openapi.yaml`**

Read the current feedback section (`sed -n '250,340p'`), then:
- On `POST /feedback/public/{key}/posts`: add optional header param `X-Feedback-Signature` (string); add `idempotency_key` (string, ≤255) to the request body schema; add `identity_verified` (boolean) + `deduped` (boolean) to the 200/201 response; add a `409` response.
- On `POST /feedback/public/{key}/posts/{postID}/votes`: add optional `X-Feedback-Signature` header; add `identity_verified` (boolean) to the response.
- On `GET /feedback/public/{key}/posts`: add optional query params `voter_identity`, `author`; add `viewer_voted` + `identity_verified` to the post item schema.
- On the admin key schemas: add write-once `secret` (string) to the create response; add `has_secret` (boolean) to the key list/read schema.

Match the file's existing style (component schemas vs inline). Keep descriptions terse.

- [ ] **Step 2: Update `drift_006_test.go`**

Add assertions mirroring the existing ones in that file for the new fields/params/responses (grep the file for how it pins a field, replicate for `idempotency_key`, `deduped`, `identity_verified`, `viewer_voted`, `has_secret`, `secret`, the `X-Feedback-Signature` header, and the 409).

- [ ] **Step 3: Run the contract test**

Run: `go test -tags contract ./cmd/... -run Drift -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add specs/006-feedback-boards/contracts/openapi.yaml cmd/manyforge/drift_006_test.go
git commit -m "feat(006): openapi + drift — signed ingress, idempotency, read path (saz.5)"
```

---

### Task 10: Security-regression + integration tests

**Files:**
- Modify: `internal/security_regression/feedback_pins_test.go` (source-level pins)
- Modify: `internal/feedback/feedback_integration_test.go` (behavioral, `-tags integration`)

**Interfaces:**
- Consumes: everything above. This task is the safety net; it must fail if a later refactor drops a control.

- [ ] **Step 1: Add source-level pins to `feedback_pins_test.go`**

Follow the file's existing `strings.Contains`/reflection pin style. Add pins (each with the finding rationale in a comment):
- `internal/feedback/signature.go` contains `hmac.Equal(` (constant-time compare) and `sigMaxSkew` = `300`.
- `migrations/0104_*.up.sql` contains `SET search_path = public` on each of the 4 functions, `ENABLE ROW LEVEL SECURITY` for `feedback_ingest_idempotency` **without** a `CREATE POLICY … feedback_ingest_idempotency` and **without** a `GRANT … feedback_ingest_idempotency … manyforge_app`, and the `FB409` errcode.
- `migrations/0104_*.up.sql` backfill excludes principals: contains `NOT IN (SELECT id::text FROM principal)`.
- `internal/feedback/public.go` `resolveVerified` returns `sigBad`/fail-closed: contains `h.Sealer == nil` → `true` and `Sealer.Open` error → `true` branches (assert both `return false, true` occurrences exist).
- `internal/feedback/ingestkey.go` never logs the secret: assert no `Logger` call references `secretPlain`/`sec`/`sealed` (grep-style negative pin).

- [ ] **Step 2: Run source pins**

Run: `go test ./internal/security_regression/ -run Feedback -v`
Expected: PASS.

- [ ] **Step 3: Add behavioral integration tests** (`-tags integration`, testcontainers Postgres — mirror the file's existing harness that runs migrations + grants `manyforge_app`)

Cover (each as a subtest):
- Valid signature → `identity_verified=true`; tampered/expired/malformed → 401; MAC for one post replayed to another post → 401 (path binding).
- Fail-closed: `sealed_secret` present + `Sealer==nil` + signature → 401; unseal failure (seal with key A, open with key B) → 401; `sealed_secret IS NULL` + signature → anon 201.
- Exactly-once: same `(key, idem, body)` → one post (2nd `deduped=true`, HTTP 200, same id); same key+idem, **different body** → 409; different idem → 2 posts; empty idem → no dedupe.
- **Cross-tier squat:** anon submit idem `X` then verified submit idem `X` → **two** posts (disjoint `a:`/`v:` idem namespaces; verified post not swallowed).
- **Backfill:** seed a pre-0104-style raw public vote + a raw internal principal vote, run 0104, assert public → `a:`-prefixed and principal row unchanged, and that the principal can't be double-voted via the internal `Service.Vote` path.
- Tier isolation: anon `voter_identity="v:alice"` stored `a:v:alice`; verified `alice` stored `v:alice` — two rows; independent dedupe.
- `viewer_voted`: true only for the identity that voted (a second identity sees false); scoped to tier namespace.
- Idempotency table unreachable by a principal-scoped query (attempt a `SELECT` under `WithPrincipal` → RLS blocks / zero rows).

- [ ] **Step 4: Run integration + full gates**

Run: `make sec-test && make test && make lint && go test -tags contract ./cmd/...`
Expected: PASS. (If Docker is unavailable, note it and run `make sec-test` before pushing.)

- [ ] **Step 5: Commit**

```bash
git add internal/security_regression/feedback_pins_test.go internal/feedback/feedback_integration_test.go
git commit -m "test(006): signature/exactly-once/backfill/tier-isolation regression + integration (saz.5)"
```

---

### Task 11: Frontend — admin secret-once + `has_secret` + Verified badge

**Files:**
- Modify: `web/src/app/core/feedback.service.ts` (types for `secret`, `has_secret`, `identity_verified`)
- Modify: `web/src/app/pages/feedback/board-detail.ts` (+ its template) — secret-once panel, has_secret indicator, verified badge
- Test: `web/src/app/pages/feedback/board-detail.spec.ts` (Vitest)

**Interfaces:**
- Consumes: create-key response `{ ..., secret?, has_secret }`; post `identity_verified`.

- [ ] **Step 1: Read the component + service** to match house style (signals, standalone, custom CSS tokens): `board-detail.ts`, `feedback.service.ts`, and the existing key-list markup. Note the exact signal/method names.

- [ ] **Step 2: Write the failing Vitest spec**

In `board-detail.spec.ts` (mirror the existing spec's `HttpTestingController` mount helper — see the cost-estimate spec pattern in `code-review` for reference):
- On create-key, when the response includes `secret: "fbs_x"`, the template renders a one-time secret block containing `fbs_x` and copy affordance; after dismiss it is gone and not re-fetchable.
- A key with `has_secret: true` shows the "secret set" indicator; `has_secret: false` does not.
- A post with `identity_verified: true` renders a "Verified" badge; false does not.

Run: `cd web && npx ng test --watch=false -t board-detail` → FAIL.

- [ ] **Step 3: Implement** the service type additions + template/logic:
- `feedback.service.ts`: add `secret?: string`, `has_secret: boolean` to the ingest-key type; `identity_verified: boolean` to the post type.
- `board-detail.ts`: a `createdSecret = signal<string | null>(null)` set from the create response, rendered once with copy + dismiss (`createdSecret.set(null)`); a `has_secret` pill per key; a `@if (post.identity_verified)` Verified badge in the moderation row. Style with existing CSS tokens.

- [ ] **Step 4: Run spec + build + prettier**

Run: `cd web && npx ng test --watch=false -t board-detail && npm run build && npx prettier --write "src/app/pages/feedback/**" "src/app/core/feedback.service.ts"`
Expected: PASS, build OK.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/feedback/board-detail.ts web/src/app/pages/feedback/board-detail.spec.ts web/src/app/core/feedback.service.ts
git commit -m "feat(006): admin secret-once UX + has_secret + Verified badge (saz.5)"
```

---

### Task 12: Frontend — portal server-truth voted state + Playwright

**Files:**
- Modify: `web/src/app/core/public-feedback.service.ts` (list params + post fields)
- Modify: `web/src/app/pages/feedback/portal.ts` (pass `voter_identity=<deviceId>`, render `viewer_voted`)
- Test: `web/e2e/feedback-portal.spec.ts` (Playwright) or extend the existing feedback e2e

**Interfaces:**
- Consumes: `GET …/posts?voter_identity=<id>` → `viewer_voted`.

- [ ] **Step 1: Read `portal.ts` + `public-feedback.service.ts`** and the existing feedback e2e (memory: mock `**/api/**` fallback first to avoid nav-badge 401→logout).

- [ ] **Step 2: Implement** — the portal already has a stable device id (reuse it; if not, generate + persist to `localStorage`). Pass it as `voter_identity` on the list fetch; bind the vote button's "voted" state to `post.viewer_voted` so it reflects server truth on load (not just optimistic local state).

- [ ] **Step 3: Write the Playwright spec** — load the portal against the live dev stack (admin :4300, backend :8081; login `live-demo@manyforge.test` / `DevPassw0rd!` for setup if needed), vote on a post, reload, assert the vote persists as "voted" from server state (`viewer_voted`). Mock `**/api/**` empty fallback first per the logout gotcha.

Run: `cd web && E2E_BASE_URL=http://localhost:4300 npx playwright test e2e/feedback-portal.spec.ts` → iterate to green.

- [ ] **Step 4: Build + prettier + drive the real browser** (gstack `$B` or Playwright headed) to confirm the secret-once flow and portal voted-state visually.

Run: `cd web && npm run build && npx prettier --write "src/app/pages/feedback/**" "src/app/core/public-feedback.service.ts"`

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/feedback/portal.ts web/src/app/core/public-feedback.service.ts web/e2e/feedback-portal.spec.ts
git commit -m "feat(006): portal server-truth voted state + Playwright (saz.5)"
```

---

## Final integration & ship (after Task 12)

- [ ] Apply 0104 to the dev DB (if not already), restart the backend (force an air rebuild — touch alone won't; edit a `.go`), and drive the full flow in a browser: create key → copy `fbs_` secret once → a signed submit (curl with a computed signature) lands `identity_verified=true` → portal shows `viewer_voted` from server truth.
- [ ] Full gates green: `make test && make lint && make sec-test && go test -tags contract ./cmd/...`; `cd web && npm run build && npx ng test --watch=false`.
- [ ] `bd update manyforge-saz.5 --claim` at start; `bd close` the verified-identity scope at end (leave SL-D notifications open as a separate item).
- [ ] Open PR → `master`, squash, **manual** merge (`--auto` races post-review commits); babysit CI + auto-review; watch the hub image build + Flux rollout.
- [ ] Set `MANYFORGE_FEEDBACK_MASTER_KEY` (32-byte base64/hex) in the prod sealed env **before** the verified tier is usable (unset is safe — anon keeps working).

## Self-review notes (author)

- Spec coverage: §3 signature → T5+T6; §3 fail-closed matrix → T6 `resolveVerified` + T10; §3 exactly-once → T2 DEFINER + T6 + T10; §4 secret/nil-guard → T1+T4; §5 namespacing + backfill → T2 + T10; §6 read path → T2+T8; §7 migration → T2; §8 wiring → T3+T4+T6-8; §9 FE → T11+T12; §10 tests → T10 (+ per-task); §11 verify/deploy → Final.
- Type consistency: `resolveVerified`/`readBody`/`namespacedParam`/`publicBoard{keyID,sealedSecret}` defined in T6, consumed T7/T8; DEFINER signatures in T2 match the raw SQL calls in T6-8 (arg counts: submit 10, vote 6, list 4, board return 6); `FB409` raised in T2, matched in T6.
- Placeholder scan: FE tasks (T11/T12) intentionally begin with a "read the component" step because they integrate into existing signal-based components; the code to add is shown concretely, the surrounding wiring is matched by reading. All backend steps carry complete code.
