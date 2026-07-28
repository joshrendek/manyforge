# Tenant-merge identity and access reconciliation

**Issue:** `manyforge-cnrc.4`

**Schema version:** migration `0116`

## Outcome

Whole-tenant preflight now persists a versioned reconciliation plan rather than
leaving identity and access behavior implicit in the generic root rewrite. V1 is
lossless and conservative: supported records keep their IDs and semantic fields,
while every tenant-wide collision or malformed cross-scope reference blocks the
operation.

The same plan has three consumers:

1. `tenant_merge_preflight` displays the plan's blockers and stores its canonical
   SHA-256 hash.
2. `tenant_merge_validate_preflight` recomputes the plan under the merge locks and
   invalidates a ready operation when either the plan or its indirect inputs changed.
3. `tenant_merge_write_fence` authorizes every source-to-destination table rewrite
   against the stored per-table action. The ready-to-running trigger independently
   requires the current, conflict-free plan.

The successful transition appends `reconciliation.applied` with the exact plan
version and hash. The complete plan remains on the durable operation.

## V1 policies

| Domain | Action | Blocking rule |
|---|---|---|
| Human principals | Preserve the global principal and account rows. | A principal-kind/account change alters the plan digest. |
| Agent principals | Rewrite `tenant_root_id`; preserve `home_business_id`, agent ID, and its single home membership. | Block a home outside the source subtree or an invalid/administrative agent membership. |
| Custom roles | Rewrite the role root and preserve role ID, key, fields, and permission set. | Block every destination key collision. No matching-role coalescing in V1. |
| Memberships | Rewrite the root; preserve principal, business, role, grantor, and grant time. | Block a custom role from another tenant. Grants remain anchored at their original business/subtree. |
| Invitations | Rewrite the root; preserve token, status, business, and role. | Block a custom role from another tenant. |
| Contacts, requesters, companies, domains, and inbound routes | Rewrite only when the destination uniqueness boundary is clear. | Block every email, domain, or address collision; never choose a winner. |
| AI credentials, OAuth state, MCP, secrets, and connectors | Rewrite the root; preserve IDs, business ownership, ciphertext, endpoints, and external references. | Existing business/root composite constraints must remain valid. |
| GitHub App installations | Rewrite linked scope; preserve installation ID and configuration. | Block an installation whose business/agent/root links disagree. |

Ticket/thread, activity, attachment, and analytics aggregate collisions remain
part of the same plan because they also carry tenant-wide unique keys.

## Indirect IAM state

`role_permission` has no `tenant_root_id`, so the inventory snapshot cannot see a
permission change. The plan includes a canonical custom-role permission digest,
and migration `0116` adds a root-aware trigger to fence those writes. This closes
both windows:

- a permission edit before fencing makes validation mark the plan stale;
- an edit after fencing returns `TENANT_MERGE_IN_PROGRESS`.

## Access invariants

Closure-derived authorization continues to start at each membership's unchanged
`business_id`. Moving source root `S` beneath destination parent `P` therefore
does not turn an Owner at `S` into an Owner at `P`, destination root `D`, an
ancestor, or a sibling. Destination Owners inherit the newly attached subtree in
the normal direction.

After cutover, `S.tenant_root_id = D` and `S.parent_id = P`. The deferred
last-Owner guard consequently evaluates destination root ownership for later
membership changes; all direct Owner grants at former root `S` may be removed
without treating `S` as a tenant root.

## Verification

`TestTenantMergeReconciliationPreservesIdentityAndAccess` covers source-only
Owners, a member with grants in both tenants, the destination Owner, agent home
identity, custom-role permissions, invitations, AI credentials, sealed connector
secrets, connectors, GitHub installation identity, indirect permission staleness
and fencing, and the former-root Owner invariant.

`TestTenantMergeReconciliationBlocksIdentityCollisionsAndMalformedLinks` covers
CRM/support identity collisions plus invalid invitation-role and installation
links. The catalog security gate verifies that plan helpers have no direct app
surface and that preflight, validation, cutover rewrites, and indirect IAM writes
all consume the reconciliation controls.
