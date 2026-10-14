# Spec 018 — Work Board (a light beads replacement)

**Status:** Draft → implementing (manyforge-jncc)
**Date:** 2026-10-14

## Why

We track agent work with `bd` (beads): a local Dolt database per repo, wired into
`AGENTS.md`/`CLAUDE.md` and Claude Code hooks (`bd prime`). It works, but it is
heavy (Dolt, redirects, private stores, JSONL export hazards), invisible from
ManyForge, and it has no notion of **work only a human can do** — rotate a prod
key, approve DNS, click "install" on a GitHub App. Agents bury those asks in
chat or in issue descriptions and they get lost.

The Work Board moves task tracking into ManyForge:

- A per-business **work project** holds tasks.
- Tasks live in one of two **lanes**: `agent` (things coding agents pick up)
  and `operator` (things the humans must do). Agents create operator tasks
  when they are blocked on a person; the operator sees them in ManyForge.
- Tasks move through a **backlog** and an **active board**.
- A small CLI, **`mf`**, gives agents the beads ergonomics (`mf ready`,
  `mf create`, `mf claim`, `mf close`, `mf prime`) from any repo, keeps a local
  copy, and **syncs** with the ManyForge instance.

## Data model

`work_project` — per business. `slug` (unique per business), `name`,
`key_prefix` (e.g. `mf`, used in task keys `mf-k3x9q2`).

`work_task` — per project.

| field | notes |
|---|---|
| `key` | matches `^[a-z][a-z0-9]{0,11}-[a-z0-9]{2,16}(\.[0-9]{1,4}){0,3}$` (beads-compatible); new keys are `<prefix>-<6 base36>`, generated client-side by `mf` (offline create) or server-side by the web UI. Unique per project. |
| `title`, `description`, `notes` | `notes` is append-only text (CLI `mf note`). |
| `lane` | `agent` \| `operator` |
| `status` | `backlog` \| `todo` \| `in_progress` \| `blocked` \| `done` \| `canceled` |
| `priority` | 0–4 (0 = critical, beads semantics) |
| `type` | `task` \| `bug` \| `feature` \| `chore` |
| `assignee` | free text (agent name, human handle) |
| `labels` | text[] |
| `blocked_by` | text[] of task keys in the same project |
| `rev` | bigint from a global sequence, bumped on every write — the sync cursor |
| `closed_at`, `created_at`, `updated_at` | |

Board semantics:

- **Backlog** = `status = backlog` (captured, not scheduled).
- **Active board** = columns `todo → in_progress → blocked → done`.
- **Ready** (agent lane) = `status = todo` and every `blocked_by` key is `done`
  or `canceled` (unknown keys count as unresolved).
- **Operator inbox** = operator-lane tasks not done/canceled.

## Sync protocol

Local-first with an outbox; field-level last-writer-wins in server arrival order.

`POST /api/v1/work/sync` (project bound by the token)

```json
{ "since": 1234, "ops": [ {"op":"create","key":"mf-k3x9q2","fields":{...}},
                          {"op":"update","key":"mf-k3x9q2","fields":{"status":"done"}},
                          {"op":"note","key":"mf-k3x9q2","text":"..."} ] }
```

Response: `{ "rev": 1300, "project": {...}, "tasks": [ every task with rev > since ], "results": [ {"key":..., "ok":true} | {"key":..., "ok":false, "error":"..."} ] }`

- Ops apply in order, each in its own savepoint; a bad op is reported and skipped.
- `create` on an existing key is a conflict (reported, not applied).
- `update` patches only the fields sent.
- Pull returns full task snapshots, so the client just upserts by key.

## Authentication for `mf`

Project-scoped **work tokens** (`mfw_…`), created in the web UI, shown once,
stored as SHA-256 hashes, revocable, with a last-used timestamp. A token can only
read and write its own project via `/api/v1/work/sync` (and the CLI download).
The web UI uses the normal session JWT and RBAC (`work.read`, `work.write`).

## CLI (`cmd/mf`)

Repo files: `.mf/config.json` (server, project slug; committed), `.mf/state.json`
(local cache, cursor, outbox; gitignored via `.mf/.gitignore`). The token comes
from `MF_TOKEN` or `~/.config/mf/credentials.json` (keyed by server + project), and
is never written into the repo.

Commands: `init`, `prime`, `ready`, `list`, `board`, `backlog`, `operator`, `show`,
`create`, `update`, `claim`, `close`, `note`, `sync`, `setup claude`.

Every mutating command writes locally first, then makes a best-effort sync. With no
server configured, `mf` is a fully local tracker.

**Agent integration:** `mf init` adds a marked block
(`<!-- mf:begin --> … <!-- mf:end -->`) to `AGENTS.md` and `CLAUDE.md`, or creates
`AGENTS.md`. `mf setup claude` merges `mf prime` into `.claude/settings.json`
SessionStart/PreCompact hooks. `mf prime` prints the workflow rules plus the
current ready list and operator inbox.

## Web UI

`/work`: the project list, plus a "connect a repo" panel (install, `mf init`
command, token management).
`/work/:slug`: three tabs:
**Board** (active columns, lane filter), **Backlog**, **Operator** (human inbox).
The nav shows a badge with the count of open operator tasks.

## Test plan

- Go unit: key generation and validation, ready computation, op validation.
- Go integration (`internal/work`): CRUD + RLS isolation across tenants; sync
  create/update/note/conflict/cursor; token auth (revoked/unknown/foreign → 401);
  audit rows written in-tx.
- security_regression: token hashing + constant-time pin; RLS policy on new tables.
- Contract: openapi.yaml covers the new routes (drift test).
- CLI unit (`cmd/mf`): AGENTS.md block upsert idempotence, settings.json hook
  merge, outbox/sync against an httptest server, ready filter.
- Web unit specs for the board page and service; Playwright e2e
  `web/e2e/work-board.spec.ts` (mocked API) covering board/backlog/operator tabs,
  creating an operator task, and moving status.

## Wire format (shared by server + `mf`)

Task JSON (snake_case, all fields always present in server responses):

```json
{ "key":"mf-k3x9q2", "title":"…", "description":"", "notes":"", "lane":"agent",
  "status":"todo", "priority":2, "type":"task", "assignee":"", "labels":[],
  "blocked_by":[], "rev":1301, "created_at":"RFC3339", "updated_at":"RFC3339",
  "closed_at":null }
```

Project JSON: `{ "slug":"manyforge", "name":"ManyForge", "key_prefix":"mf" }`.

Sync: `POST {server}/api/v1/work/sync`, header `Authorization: Bearer mfw_<token>`.
Op `fields` may contain any of: title, description, lane, status, priority, type,
assignee, labels, blocked_by (create requires title). `note` ops append
`"\n[<RFC3339> <author>] <text>"`-style entries server-side (author = op.author or "agent").
Setting status to `done`/`canceled` stamps `closed_at`; moving out of those clears it.
Errors use the platform error envelope; 401 for any token problem.

## HTTP API

Management (session JWT, `x-manyforge-audience: management`, business-scoped,
404-never-403). `work.read` gates GETs; `work.write` gates mutations and token management.

| method | path | body / query | response |
|---|---|---|---|
| GET | `/api/v1/businesses/{id}/work/projects` | – | `{items:[Project+counts]}` |
| POST | `/api/v1/businesses/{id}/work/projects` | `{name, slug?, key_prefix?}` | `201 Project` |
| GET | `/api/v1/businesses/{id}/work/projects/{pid}` | – | `Project` |
| GET | `/api/v1/businesses/{id}/work/projects/{pid}/tasks` | `?include_closed=true` (else open + closed in last 14d) | `{items:[Task]}` (cap 2000, ordered priority, created_at) |
| POST | `/api/v1/businesses/{id}/work/projects/{pid}/tasks` | `{title, description?, lane?, status?, priority?, type?, assignee?, labels?, blocked_by?}` | `201 Task` (server-generated key) |
| PATCH | `/api/v1/businesses/{id}/work/projects/{pid}/tasks/{key}` | any task fields + optional `note` (appended, author = user's email/display name) | `Task` |
| GET | `/api/v1/businesses/{id}/work/projects/{pid}/tokens` | – | `{items:[{id,name,prefix,created_at,last_used_at,revoked_at}]}` |
| POST | `/api/v1/businesses/{id}/work/projects/{pid}/tokens` | `{name}` | `201 {id,name,prefix,created_at,token}` (plaintext `mfw_…` returned once) |
| DELETE | `/api/v1/businesses/{id}/work/projects/{pid}/tokens/{tid}` | – | `204` (revoke) |
| GET | `/api/v1/businesses/{id}/work/summary` | – | `{open_operator:int, open_agent:int, in_progress:int}` (nav badge) |

Project JSON (management): `{id, slug, name, key_prefix, created_at, counts:{backlog,todo,in_progress,blocked,operator_open}}`.

Token-authenticated (`x-manyforge-audience: server`, `security: []`):
`POST /api/v1/work/sync` — see Sync protocol. Token resolution goes through a
SECURITY DEFINER function (token hash → project/business/tenant/issuing principal);
the request then runs `WithPrincipal(issuer)` so RLS and `work.write` are
re-checked on every call. If the issuer loses access, the token goes dead.
Per-token rate limit. Uniform 401 for unknown/revoked/unauthorized tokens.

## Beads migration

`mf import beads [file|-]` reads `bd export` JSONL. It keeps beads IDs as task keys,
maps statuses (deferred→backlog, closed→done), folds acceptance criteria, design,
comments, and close reasons into the description and notes, and turns `blocks`
dependencies into `blocked_by`. By default it imports only issues that aren't closed.
Create ops may carry `created_at`/`closed_at` to preserve history.
