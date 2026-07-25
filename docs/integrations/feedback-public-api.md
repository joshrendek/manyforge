# Feedback Public API — Client Integration Guide (iOS / mobile / SPA)

> **For an implementing agent with zero manyforge context:** this file is your complete spec.
> You do not need the manyforge repo. Implement the **anonymous** feedback client below. Do NOT
> try to use the "verified" tier from an app — see §1.

**Base URL (prod):** `https://hub.bluescripts.net/api/v1`
**Contract source of truth:** `specs/006-feedback-boards/contracts/openapi.yaml` (this doc mirrors it).
**Auth:** a per-board **publishable key** (`fbk_…`) in the URL path. It's a client token like a Sentry
DSN — safe to embed in an app binary. There is no JWT/login on these endpoints.

---

## 1. Which tier does an app use? (read this first)

There are two tiers over the same endpoints:

| Tier | Who | Auth material | An iOS app? |
|------|-----|---------------|-------------|
| **Anonymous** | app client / browser | publishable `fbk_` key + a caller-supplied device identity | **✅ this is you** |
| Verified | a **backend** you run | `fbk_` key **+** an HMAC `X-Feedback-Signature` computed from a secret (`fbs_`) | ❌ never from an app |

**Never put the `fbs_` secret or an `X-Feedback-Signature` in a mobile app.** The signing secret must
stay server-side; shipping it in a binary leaks it. So an iOS app uses the **anonymous** path only.
If you later want *verified* identity (e.g. tie feedback to a logged-in account), the app calls
**your own backend**, and your backend signs and forwards to manyforge — that's a separate,
server-only integration (§8), not something the app does directly.

Everything below is the anonymous client. It's fully functional: submit, vote, list, and
server-truth "did I vote" state.

## 2. Getting a publishable key

An operator creates it in the manyforge admin UI: **Feedback → a board → Ingest keys → Create**.
Copy the `fbk_…` value. One board = one product surface (e.g. "iOS App"). Put the key in your app
config (it's not a secret). A **revoked** or unknown key returns a uniform `401` on every call.

## 3. Identity: one stable device id

The anonymous tier identifies a user by an **opaque, caller-supplied string** you send as
`author_identity` (on submit) and `voter_identity` (on vote/list). Generate **one stable id per
install** and reuse it everywhere:

- Generate a UUID once, persist it (Keychain recommended so it survives reinstalls; UserDefaults is
  fine if you don't care about that). **Reuse the same value for submit, vote, and the `voter_identity`
  query on list** — the server correlates "did I vote" by exact match.
- Max length **200 chars**. Treat it as opaque; the server namespaces it internally.
- It's not authenticated — it only dedupes/attributes within this device. Clearing it = a "new" user.

## 4. Endpoints

All paths are under the base URL. `{key}` is your `fbk_` publishable key.

### 4.1 Submit a post — `POST /feedback/public/{key}/posts`

Request body (`application/json`):
```jsonc
{
  "title": "Add dark mode",          // REQUIRED, 1–300 chars
  "body": "Would love a dark theme", // optional, ≤20000 chars
  "author_identity": "<device-id>",  // optional but recommended, ≤200 chars
  "idempotency_key": "<uuid>"        // optional, ≤255 chars — see §5
}
```
Responses:
- **`201 Created`** — new post created.
- **`200 OK`** — idempotency-key replay (same key + same body): the post already existed. The body
  **echoes the original submission** (`status:"open"`, `vote_count:0`) and sets `"deduped": true` —
  it is NOT current post state. Treat 200 as "already submitted".

Response body (both 200 and 201) — `PublicSubmitResult`:
```jsonc
{
  "id": "0f7c…-uuid",
  "title": "Add dark mode",
  "status": "open",           // open | planned | in_progress | done | declined
  "vote_count": 0,
  "identity_verified": false, // always false on the anonymous path
  "deduped": false            // true only on a 200 replay
}
```
Errors: `400` (missing/oversized title or body, or `idempotency_key`>255 / `author_identity`>200),
`401` (unknown/revoked key or non-public board), `409` (same `idempotency_key` reused with a
**different** body), `413` (body over 64 KiB).

### 4.2 Vote for a post — `POST /feedback/public/{key}/posts/{postID}/votes`

`{postID}` is a post's `id` (UUID) from the list. Request body:
```jsonc
{ "voter_identity": "<device-id>" }   // REQUIRED, ≤200 chars — use the SAME device id
```
Response — `VoteResult` (`200 OK`):
```jsonc
{
  "voted": true,        // true if this call recorded a new vote; false if you'd already voted (idempotent)
  "vote_count": 12,     // fresh total for the post
  "identity_verified": false
}
```
Voting is **idempotent** — voting twice with the same `voter_identity` on the same post is a no-op
(`"voted": false`, count unchanged). There is no un-vote endpoint.
Errors: `400` (missing `voter_identity`), `401` (bad key), `404` (post not on this board / deleted).

### 4.3 List posts — `GET /feedback/public/{key}/posts`

Query params (all optional):
- `limit` — 1–100 (default 20, clamped).
- `voter_identity=<device-id>` — makes each post's `viewer_voted` reflect **whether *this* device
  already voted** (server truth). Pass it so your vote buttons render correct state on load.
- `author=<device-id>` — "my submissions": returns only posts this device authored.

Response — `PublicPostList` (`200 OK`):
```jsonc
{
  "items": [
    {
      "id": "0f7c…-uuid",
      "title": "Add dark mode",
      "body": "Would love a dark theme",   // may be null
      "status": "open",
      "vote_count": 12,
      "created_at": "2026-07-25T00:00:00Z",
      "viewer_voted": true,        // true only when you passed voter_identity AND this device voted
      "identity_verified": false
    }
  ]
}
```
Posts are ordered by `vote_count` desc, then newest. `401` on a bad key.

## 5. Idempotency & retries

Network retries can double-submit. To make submit **exactly-once**, send an `idempotency_key` (a
UUID you generate per user action) in the body:
- First call with that key → creates the post (`201`).
- Retry with the **same key + same body** → `200`, `"deduped": true`, same post `id` (no duplicate).
- Same key + **different body** → `409` (client bug — you reused a key for a different request).

Generate a fresh `idempotency_key` for each *distinct* submission; reuse it only when retrying that
exact submission. Votes need no idempotency key — they're already idempotent per `voter_identity`.

## 6. Errors & rate limiting

- Errors return JSON `{ "error": { "code": "…", "message": "…" } }` (codes: `VALIDATION`,
  `UNAUTHORIZED`, `NOT_FOUND`, `CONFLICT`, `PAYLOAD_TOO_LARGE`). `401` is deliberately **uniform** —
  it never reveals whether a key exists vs. a board is private. Don't build UX that distinguishes them.
- The endpoints are **per-IP rate-limited**. Handle `429` (and transient `5xx`) with exponential
  backoff. Body cap is 64 KiB.
- All requests must be HTTPS.

## 7. Swift / URLSession reference

```swift
import Foundation

struct FeedbackClient {
    let baseURL = URL(string: "https://hub.bluescripts.net/api/v1")!
    let publishableKey: String            // "fbk_…"
    let deviceID: String                  // one stable UUID per install (persist in Keychain)

    private func url(_ path: String, query: [URLQueryItem] = []) -> URL {
        var c = URLComponents(url: baseURL.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        if !query.isEmpty { c.queryItems = query }
        return c.url!
    }

    // MARK: Submit
    struct SubmitResult: Decodable { let id: String; let status: String; let deduped: Bool }
    func submit(title: String, body: String?, idempotencyKey: String = UUID().uuidString) async throws -> SubmitResult {
        var req = URLRequest(url: url("feedback/public/\(publishableKey)/posts"))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: [
            "title": title,
            "body": body as Any,
            "author_identity": deviceID,
            "idempotency_key": idempotencyKey,
        ].compactMapValues { $0 })
        let (data, resp) = try await URLSession.shared.data(for: req)
        try Self.check(resp)                                   // maps 4xx → thrown error
        return try JSONDecoder().decode(SubmitResult.self, from: data)
    }

    // MARK: Vote
    struct VoteResult: Decodable { let voted: Bool; let vote_count: Int }
    func vote(postID: String) async throws -> VoteResult {
        var req = URLRequest(url: url("feedback/public/\(publishableKey)/posts/\(postID)/votes"))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: ["voter_identity": deviceID])
        let (data, resp) = try await URLSession.shared.data(for: req)
        try Self.check(resp)
        return try JSONDecoder().decode(VoteResult.self, from: data)
    }

    // MARK: List (with server-truth voted state)
    struct Post: Decodable { let id, title: String; let body: String?; let status: String
                             let vote_count: Int; let created_at: String; let viewer_voted, identity_verified: Bool }
    struct PostList: Decodable { let items: [Post] }
    func list(limit: Int = 50, mine: Bool = false) async throws -> [Post] {
        var q = [URLQueryItem(name: "limit", value: String(limit)),
                 URLQueryItem(name: "voter_identity", value: deviceID)]      // → viewer_voted
        if mine { q.append(URLQueryItem(name: "author", value: deviceID)) }  // → my submissions
        var req = URLRequest(url: url("feedback/public/\(publishableKey)/posts", query: q))
        req.httpMethod = "GET"
        let (data, resp) = try await URLSession.shared.data(for: req)
        try Self.check(resp)
        return try JSONDecoder().decode(PostList.self, from: data).items
    }

    enum APIError: Error { case unauthorized, notFound, conflict, tooLarge, rateLimited, http(Int) }
    static func check(_ resp: URLResponse) throws {
        guard let code = (resp as? HTTPURLResponse)?.statusCode else { return }
        switch code {
        case 200, 201: return
        case 401: throw APIError.unauthorized      // bad/revoked key — surface a generic "unavailable"
        case 404: throw APIError.notFound          // post gone
        case 409: throw APIError.conflict          // idempotency key reused w/ different body
        case 413: throw APIError.tooLarge
        case 429: throw APIError.rateLimited       // back off + retry
        default: throw APIError.http(code)
        }
    }
}
```

UI notes: bind the vote button's "voted" state to `post.viewer_voted` from a list fetched **with**
`voter_identity` (server truth), not just a local flag — so it's correct after reinstall/relaunch.
Expose the toggle to assistive tech (`accessibilityValue` / `aria-pressed` equivalent).

## 8. (Info only) The verified tier — server-to-server, NOT for the app

If you later want feedback tied to a *verified* account identity, do it in **your backend**, never
the app: your server holds the `fbs_` secret (created alongside the `fbk_` key, shown once), and for
each request computes `X-Feedback-Signature: t=<unix>,v1=hex(HMAC_SHA256(secret, "<t>.<METHOD>.<request-target>.<body>"))`
where `<request-target>` is the full path **including `/api/v1` and the query string**, then POSTs to
the same endpoints. That's out of scope for the iOS client — the app just talks to your backend. See
`docs/superpowers/specs/2026-07-24-feedback-verified-identity-design.md` for the full scheme.

## 9. Quick test (curl)

```bash
KEY=fbk_yourkey
BASE=https://hub.bluescripts.net/api/v1
DEV=$(uuidgen)
# submit
curl -sX POST "$BASE/feedback/public/$KEY/posts" -H 'content-type: application/json' \
  -d "{\"title\":\"Add dark mode\",\"author_identity\":\"$DEV\",\"idempotency_key\":\"$(uuidgen)\"}"
# list with my voted state
curl -s "$BASE/feedback/public/$KEY/posts?voter_identity=$DEV&limit=20"
# vote (use an id from the list)
curl -sX POST "$BASE/feedback/public/$KEY/posts/<POST_ID>/votes" -H 'content-type: application/json' \
  -d "{\"voter_identity\":\"$DEV\"}"
```
