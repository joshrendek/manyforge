# Feature Specification: Tenant Foundation

**Feature Branch**: `001-tenant-foundation`

**Created**: 2026-05-29

**Status**: Draft

**Input**: User description: "Tenant foundation: user sign-up and sign-in, a master-to-sub-business hierarchy, member invitations and roles, and tenant-scoped isolated data access across the business subtree"

## Overview

This feature establishes the foundation that every other ManyForge capability (CRM, ticketing, inbound email, AI agents, feature-request/bug-report flows) is built on: people can create accounts, model their portfolio of businesses as a master-business → sub-business hierarchy, invite teammates with roles, and trust that everyone sees and acts only within the businesses they are authorized for. It delivers no end-user "vertical" capability on its own — its value is the trustworthy multi-tenant workspace and access model that all later features depend on.

## Clarifications

### Session 2026-05-29

- Q: Access inheritance model across the master→sub-business hierarchy → A: Downward-only — a role granted on a business applies to that business and all of its descendants, never its ancestors or siblings (per FR-010). Per-business role overrides (inheriting a role from an ancestor but downgrading/upgrading it on a specific descendant) are noted as a potential future enhancement, out of scope for v1.
- Q: How custom roles define permissions → A: Capability-catalog RBAC — the platform defines a catalog of fine-grained, per-capability permissions; a custom role is a named, admin-defined set of those permissions; every action (human member or AI agent) is authorized against the actor's effective permission set. Built-in presets (Owner/Admin/Member/Viewer) ship as starting points, with Owner locked at full access.
- Q: How AI agents acquire permissions → A: Agents are first-class principals — an agent is a non-human principal scoped to one business and assigned a role (preset or custom), exactly like a member; its effective permissions are its role's permissions, and it appears in access lists and the audit log. The foundation's membership/role model admits non-human principals; agent lifecycle/management is delivered by the separate agents feature.
- Q: Where custom roles are defined and scoped → A: Tenant-scoped — custom role definitions belong to the tenant (master business), are authored by holders of a manage-roles permission, and are assignable on any business within that tenant's subtree; they are invisible to other tenants. Delegated per-node role authoring is a future enhancement.

### Session 2026-05-29 (Codex second-pass review)

Resolutions adopted from an independent Codex review of the constitution + spec:

- **Permission-gated operations** (not role names): every management action is authorized by permission (manage-members, manage-roles, …); only deleting a business and transferring ownership are reserved Owner-only actions. (FR-008, FR-013, FR-016, FR-017, FR-023)
- **Owner semantics**: every tenant (master business) always has ≥1 direct Owner; last-Owner protection and ownership transfer operate on the tenant root and are atomic. Sub-businesses may carry optional delegated Owner/Admin grants. (FR-014, FR-024)
- **No privilege escalation**: a principal cannot grant a role/permission it does not itself hold; agents never receive administrative permissions. (FR-023, FR-027)
- **Agent containment**: an AI-agent principal is scoped to exactly one home business, gets no cross-subtree inheritance, holds no admin permissions, and passes a server-side autonomy gate after RBAC (reconciles with Constitution Principle IV). (FR-027)
- **Role mutation**: editing/deleting a role applies transactionally and immediately; a role in use cannot be deleted without reassignment; removed permissions deny by default. (FR-025)
- **Existence-oracle boundary**: the no-oracle guarantee is scoped to *across authorization boundaries* — account/business existence is never disclosed outside the requester's authorized scope (uniform shape + bounded latency); seeing members of a business you administer is expected. (FR-011, FR-026, SC-010)
- **Audit scope**: every administrative and agent-initiated mutation is audited, append-only, in the same transaction as the change. (FR-015)
- **Lifecycle & GDPR**: account/business deactivation, deletion, and export; erasure anonymizes PII while preserving a pseudonymized append-only audit record. (FR-028)
- **Abuse limits**: all lists paginated with a max page size; auth/invite/hierarchy endpoints rate-limited per IP and per account. (FR-029)
- **Cross-tenant PII**: access lists never expose memberships/metadata outside the viewer's authorized scope; no email enumeration. (FR-030)
- **Concurrency**: concurrent hierarchy mutations serialize to an acyclic, non-orphaning tree with deterministic conflict responses. (FR-031)
- **Depth**: "arbitrary depth" replaced with a configurable maximum (≥10 levels). (FR-004)
- **Deferred to the plan** (HOW, not WHAT): `tenant_root_id` column, closure-table design, RLS `SET LOCAL`/`FORCE ROW LEVEL SECURITY`/pool hygiene, permission-resolution query strategy, auth token TTL/hashing. Captured in `plan-inputs.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Establish account and master business (Priority: P1)

A new founder signs up with their email, verifies it, signs in, and creates their first **master business** — the root of their workspace.

**Why this priority**: Nothing in the platform exists until a person has an account and a top-level business to anchor everything else. This is the irreducible first slice of a usable product.

**Independent Test**: Register a brand-new user, verify the email, sign in, and create a master business; confirm the master business appears as the root of that user's workspace and that the user is its Owner.

**Acceptance Scenarios**:

1. **Given** a visitor with no account, **When** they sign up with a valid email and complete verification, **Then** they can sign in and reach an empty workspace prompting them to create a master business.
2. **Given** a signed-in user with no businesses, **When** they create a master business with a name, **Then** the business is created as a top-level (root) business and the creator is recorded as its Owner.
3. **Given** a signed-in user, **When** they attempt to sign up or create a business with a missing or invalid name/email, **Then** the system rejects the action with a clear, non-technical message and creates nothing.

---

### User Story 2 - Build the business hierarchy (Priority: P1)

The founder creates sub-businesses beneath the master business, nests them to model their portfolio, and renames, moves, archives, or restores businesses as the portfolio evolves.

**Why this priority**: The defining capability of the platform is "one master business with many sub-businesses." A founder must be able to model their real structure before any capability is attached to a business.

**Independent Test**: Under an existing master business, create several sub-businesses (including nesting one under another), rename one, move one to a different parent, and archive one; confirm the resulting tree reflects every change and the archived branch is hidden from normal views.

**Acceptance Scenarios**:

1. **Given** an Owner of a master business, **When** they create a sub-business under it, **Then** the sub-business appears as a child in the hierarchy and inherits the tenant it belongs to.
2. **Given** an existing sub-business, **When** an authorized user nests another sub-business beneath it, **Then** the hierarchy reflects arbitrary depth without loss of the ancestor relationship.
3. **Given** a business with sub-businesses, **When** a user attempts to move it underneath one of its own descendants, **Then** the system refuses the move (no cycles) and the hierarchy is unchanged.
4. **Given** a business, **When** an authorized user archives it, **Then** it and its descendants are hidden from normal lists but their data is preserved and can be restored.

---

### User Story 3 - Invite members and assign roles (Priority: P2)

A member with the manage-members permission invites a teammate by email to a specific business with a chosen role (no higher than their own). The invitee accepts and gains access scoped to that business and its descendants.

**Why this priority**: Founders work with collaborators. Roles and invitations turn a single-user workspace into a team workspace, but they are only meaningful once the hierarchy (P1) exists.

**Independent Test**: Invite a second person to a sub-business as a Member; have them accept and sign in; confirm they see only that sub-business and anything beneath it, with permissions matching the Member role, and nothing above or beside it.

**Acceptance Scenarios**:

1. **Given** a member with the manage-members permission, **When** they invite an email address with a role no higher than their own, **Then** a pending, time-limited, single-use invitation is created and the invitee is notified.
2. **Given** a pending invitation, **When** the invitee accepts (registering first if they have no account), **Then** they become a member of the target business with the assigned role.
3. **Given** an invitation that has expired or already been used, **When** someone attempts to accept it, **Then** the system refuses and explains the invitation is no longer valid.
4. **Given** an email that already has access to the target business, **When** a member with manage-members re-invites it, **Then** the system reports the existing access (the inviter is authorized to see this business's members) without creating a duplicate — and without revealing whether that email has an account or access anywhere outside this business.

---

### User Story 4 - Enforce scoped, isolated access (Priority: P2)

Every person sees and acts only within the businesses they are authorized for — through direct membership or inherited from an ancestor business. Unrelated top-level businesses are completely invisible to one another, and removing access takes effect immediately.

**Why this priority**: Isolation is the platform's core trust guarantee; a leak across businesses or tenants is the worst possible failure. It must hold from the first release, so it is specified and tested explicitly rather than assumed.

**Independent Test**: Create two unrelated master businesses owned by different people; confirm neither owner can list, open, reference, or act on the other's businesses, members, or data. Then revoke a member from a business and confirm their access disappears on their next action.

**Acceptance Scenarios**:

1. **Given** access granted at a business, **When** the member views their workspace, **Then** they see that business and all of its descendants, and never any ancestor or sibling they were not granted.
2. **Given** two unrelated top-level businesses, **When** a user authorized for one attempts to access anything in the other (by any identifier), **Then** the system responds as though it does not exist — with no signal distinguishing "not allowed" from "does not exist."
3. **Given** a member of a business, **When** an Admin revokes their access, **Then** the member can no longer view or act on that business or its descendants effective immediately.
4. **Given** a member with inherited access from an ancestor, **When** that ancestor membership is revoked, **Then** inherited access to descendants is also removed unless a separate direct grant exists.

---

### User Story 5 - Manage members and access with an audit trail (Priority: P3)

Members with the manage-members permission review who has access to a business and why (direct vs. inherited), change roles, and revoke access; Owners transfer ownership — and every such change is recorded in an audit trail authorized reviewers can read.

**Why this priority**: Day-to-day administration and accountability matter for a platform holding sensitive business data, but they build on the membership and isolation mechanics from P1–P2.

**Independent Test**: For a business with several members, view the access list showing each member's role and whether access is direct or inherited; change one member's role, revoke another, and transfer ownership; confirm effective permissions change accordingly and that an audit entry exists for each change.

**Acceptance Scenarios**:

1. **Given** a member with the manage-members permission viewing a business, **When** they open its access list, **Then** they see every member, their role, and whether the access is direct or inherited from a named ancestor business.
2. **Given** an Owner, **When** they change a member's role or revoke access, **Then** the change takes effect immediately and an audit entry records who did it, to whom, the before/after value, and when.
3. **Given** a business with a single Owner, **When** someone attempts to remove or demote that last Owner, **Then** the system refuses until ownership is transferred to another member.
4. **Given** a member authorized to view audit, **When** they review the audit trail for the business, **Then** they see a chronological, read-only record of all administrative and agent-initiated changes affecting it.

---

### Edge Cases

- **Last-owner protection**: A tenant (master business) must always retain at least one direct Owner; removing or demoting the last direct Owner is refused until ownership is transferred (atomically).
- **Cycle prevention**: A business can never be moved beneath one of its own descendants.
- **Delete vs. archive**: Deleting a business that still has active sub-businesses is refused; the user must archive or move the children first. Deletion is Owner-only and requires explicit confirmation.
- **Pending invitations for unregistered emails**: An invitation sent to an email with no account stays pending and resolves automatically when that person signs up.
- **Re-invitation / duplicate membership**: Re-inviting someone who already has access to a business the inviter administers reports the existing access without duplicating it, while never disclosing account existence outside that business.
- **Expired / reused invitations**: Acceptance is refused for expired or already-consumed invitations.
- **Inherited vs. direct access conflicts**: A person may hold both an inherited grant and a direct grant on the same business; the most permissive applicable role governs, and revoking one grant does not remove the other.
- **Self-removal**: A member may leave a business, but an Owner cannot leave while they are the last Owner.
- **Email-verification gaps**: Unverified accounts cannot create businesses, accept invitations, or be granted access.
- **Concurrent hierarchy edits**: Two simultaneous moves/archives of the same branch resolve to a single consistent tree without orphaning sub-businesses.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST allow a visitor to create an account identified by a unique email and to authenticate as that account.
- **FR-002**: System MUST require email verification before an account can create businesses, accept invitations, or be granted access.
- **FR-003**: System MUST allow an authenticated, verified user to create one or more master (top-level) businesses, each of which becomes the isolated root of its own hierarchy, with the creator recorded as Owner.
- **FR-004**: System MUST allow authorized users to create sub-businesses nested beneath a master business or another sub-business, up to a configurable maximum depth (supporting at least 10 levels).
- **FR-005**: System MUST allow authorized users to rename, move, archive, and restore businesses within a hierarchy they control.
- **FR-006**: System MUST prevent hierarchy cycles — a business MUST NOT be movable beneath one of its own descendants — and MUST NOT allow a business to be moved across tenant roots (into a different master business's tree).
- **FR-007**: System MUST define a platform-wide catalog of fine-grained, per-capability permissions (e.g., manage members, manage roles, manage the hierarchy, and read/create/update/delete or act on each resource type a module exposes). The catalog MUST be extensible — each platform module/capability contributes its own permissions as it ships.
- **FR-008**: System MUST allow principals holding the manage-members permission to invite people by email to a business with an assigned role (no higher than the inviter's own; see FR-023), producing a time-limited, single-use invitation.
- **FR-009**: System MUST let an invitee accept an invitation (registering first if needed) and thereby gain membership of the target business with the assigned role; pending invitations to unregistered emails MUST resolve on sign-up.
- **FR-010**: Access granted at a business MUST cascade to that business's descendants (inherited access) and MUST NOT grant any access to its ancestors or siblings.
- **FR-011**: System MUST scope every list, read, and action so that users perceive only the businesses and data they are authorized for; a business a user is not authorized for MUST be indistinguishable from one that does not exist (no existence disclosure, no "allowed vs. exists" oracle).
- **FR-012**: Separate top-level businesses MUST be fully isolated — no user MUST be able to list, read, reference, or act on a business to which they have no membership path.
- **FR-013**: System MUST allow principals holding the manage-members permission to change roles and revoke access, and Owners to transfer ownership; revocation and role changes MUST take effect immediately.
- **FR-014**: System MUST prevent removing or demoting the last direct Owner of a tenant (its master business) until ownership is transferred to another member; this protection and ownership transfer MUST be atomic (see FR-024).
- **FR-015**: System MUST record an append-only audit entry — in the same transaction as the change — for every administrative and agent-initiated mutation (account, membership, role, permission, ownership, hierarchy, and invitation changes, plus agent actions), capturing the actor, action, target, before and after values, and timestamp.
- **FR-016**: System MUST let principals holding the appropriate permission (e.g., manage-members or an audit-view permission) view a business's current access list — each member's role and whether access is direct or inherited (and from which ancestor) — and review the business's audit trail as a read-only record.
- **FR-017**: System MUST guard destructive operations: deleting a business is Owner-only, requires explicit confirmation, and is refused while the business still has active sub-businesses.
- **FR-018**: System MUST allow a member to leave a business voluntarily, except that the last Owner cannot leave until ownership is transferred.
- **FR-019**: System MUST scope custom role definitions to the tenant (the master business): principals holding the manage-roles permission MUST be able to create, rename, edit, and delete the tenant's custom roles as named sets of catalog permissions, those roles MUST be assignable on any business within that tenant's subtree, and a tenant's custom roles MUST NOT be visible to any other tenant.
- **FR-020**: System MUST ship built-in default roles (Owner, Admin, Member, Viewer) as ready-to-use starting points. The Owner role MUST be a locked, full-permission role that always exists on every business and cannot be edited, downgraded, or deleted.
- **FR-021**: System MUST authorize every action against the acting principal's effective permission set for the target business, denying any action whose required permission the principal lacks. This check MUST apply uniformly to human members and to non-human principals (e.g., AI agents), so that an agent's actions can be constrained to least privilege.
- **FR-022**: System MUST model business membership in terms of a *principal* that is either a human account or a non-human principal (e.g., an AI agent), so a role can be assigned to an agent through the same mechanism — and the same isolation, authorization, and audit paths — as a human member. (Creating and operating agents is delivered by the separate agents feature; this requirement only establishes that the foundation's membership/role model admits non-human principals.)
- **FR-023**: A principal MUST NOT grant a role or permission it does not itself hold; assigning the Owner role and transferring ownership are reserved to Owners. Agent principals MUST NOT be granted administrative permissions (e.g., manage-members, manage-roles, hierarchy management, business deletion, ownership transfer).
- **FR-024**: Every tenant (its master business) MUST always have at least one direct Owner. Last-Owner protection and ownership transfer operate on the tenant root and MUST be atomic — no concurrent transfer or demotion may leave a tenant with zero direct Owners. Sub-businesses MAY carry optional delegated Owner/Admin grants but do not require their own direct Owner; the tenant's Owners retain inherited administrative control over the whole subtree.
- **FR-025**: Editing or deleting a role MUST apply transactionally and take effect immediately for every principal assigned it. Deleting a role that is still assigned MUST be refused until its holders are reassigned. A permission removed from the catalog MUST deny by default wherever it was referenced.
- **FR-026**: The system MUST NOT disclose account or business existence outside the requester's authorized scope. Sign-up, sign-in, invitation, and verification responses MUST use uniform response shapes and bounded-latency behavior so they cannot be used to enumerate registered emails or accounts across tenants.
- **FR-027**: An AI-agent principal MUST be scoped to exactly one home business, MUST NOT receive cross-subtree inherited access, and MUST NOT hold administrative permissions. Every agent-initiated action MUST pass a server-side autonomy policy gate (per Constitution Principle IV) applied after the RBAC permission check. (The agent lifecycle is delivered by the separate agents feature; this requirement bounds how the foundation admits agent principals.)
- **FR-028**: System MUST support account deactivation, deletion, and personal-data export. Deleting an account or business MUST remove or irreversibly anonymize personal data while preserving an append-only, pseudonymized audit record (reconciling GDPR erasure with audit integrity). Business deletion MUST be soft by default with a defined retention window before irreversible purge.
- **FR-029**: All list endpoints MUST be paginated with an enforced maximum page size. Sign-up, sign-in, email verification, invitation creation, invitation acceptance, and hierarchy-mutation endpoints MUST be rate-limited per IP and per account/email.
- **FR-030**: An access list or member view MUST NOT expose an account's identity, metadata, or other memberships outside the businesses the viewer is authorized for; email-based lookups MUST NOT enable cross-tenant enumeration.
- **FR-031**: Concurrent hierarchy mutations (move, archive, restore, delete) on overlapping branches MUST be serialized so the tree stays acyclic and no sub-business is orphaned; conflicting concurrent mutations MUST fail deterministically with a stable conflict response.

### Key Entities

- **Account**: A person who can authenticate. Identified by a unique, verified email; has a display name and a verification status. Identity is global — one account may hold membership in many businesses across unrelated hierarchies. An Account is one kind of Principal.
- **Principal**: An actor that can hold a business membership and a role — either a human Account or a non-human principal such as an AI agent. Authorization, isolation, and auditing treat all principals uniformly. Agent principals are additionally constrained (single home business, no inherited access, no administrative permissions, autonomy gate) per FR-027.
- **Business**: A node in a hierarchy and the unit of tenancy. Has a name, an optional parent business (absent for a master/root business), and a status (active or archived). Relationships: one parent, many children, many memberships. A business with no parent is a "master" business and the isolation boundary for its subtree.
- **Membership**: A grant linking a Principal (a human Account or a non-human principal such as an AI agent) to a Business with a Role. Records who granted it, when, and whether it is direct (granted on this business) — inherited access is derived from ancestor memberships rather than stored per descendant.
- **Permission**: A single fine-grained capability in the platform-wide catalog (e.g., "manage members", "read contacts", "send email", "create agents"). Each module contributes its own permissions. Authorization compares the permission an action requires against the acting principal's effective permission set.
- **Role**: A named set of permissions defining what its holder may do within a business. The system ships built-in roles — Owner (locked, full access) plus Admin, Member, and Viewer presets — and lets holders of the manage-roles permission define additional custom roles drawn from the Permission catalog. Custom role definitions are scoped to the tenant (master business) and assignable across its subtree. Roles are assigned to members (and, via the future agents feature, to AI-agent principals).
- **Invitation**: A pending, time-limited, single-use grant addressed to an email for a target business and role, with a status (pending, accepted, expired, revoked).
- **Audit Entry**: An append-only, immutable record of any administrative or agent-initiated mutation — actor, action, target, before/after value, and timestamp — written in the same transaction as the change and scoped to the affected business. On personal-data erasure, identifying fields are pseudonymized while the record itself is retained.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A brand-new founder can go from sign-up to a master business containing at least one sub-business in under 5 minutes.
- **SC-002**: In access-boundary testing, a principal sees exactly the set of businesses reachable from its grants (each granted business plus its descendants), and no others — 100% of the time.
- **SC-003**: Two unrelated tenants exhibit 0% cross-visibility: no business, member, invitation, or record belonging to one is ever surfaced to the other through any path.
- **SC-004**: Revoking a member's access removes their ability to view or act on the affected business and its descendants on their next action (no stale access window).
- **SC-005**: 100% of membership, role, ownership, and hierarchy changes produce a corresponding audit entry.
- **SC-006**: An Admin can determine "who has access to this business, with what role, and why (direct vs. inherited)" for any business in under 30 seconds.
- **SC-007**: At 1,000 businesses and 10 levels of nesting per tenant, business-listing and access-check operations complete within a p95 latency target of 200 ms, enforced by automated performance tests.
- **SC-008**: Attempts to create a hierarchy cycle, remove the last Owner, or delete a business with active children are refused 100% of the time without leaving the hierarchy in an inconsistent state.
- **SC-009**: An Admin can define a custom role from the permission catalog and confirm that a principal assigned that role can perform exactly the permitted actions and is denied every action outside the set — verified across 100% of permission-enforcement tests.
- **SC-010**: Across tenants and accounts, sign-up, sign-in, and invitation responses reveal no account-existence signal — identical response shape and latency variance within a bounded threshold — verified by automated oracle tests.
- **SC-011**: An AI-agent principal can never read, list, or act on any business other than its single home business, and can never hold an administrative permission — verified 100% in containment tests.

## Assumptions

- **Identity is global**: one account can be a member of many businesses across unrelated hierarchies; there is no per-tenant separate identity in v1.
- **A user may own multiple master businesses**: each top-level business is its own isolated tenant root; "master" denotes "has no parent," not "exactly one per user."
- **Custom roles from day 1**: beyond the built-in presets (Owner/Admin/Member/Viewer), holders of the manage-roles permission can define custom roles from a fine-grained, per-capability permission catalog. Role definitions are tenant-scoped (authored at the master business) and assignable across the subtree. Custom roles are the mechanism for constraining both teammates and AI agents to least privilege; the Owner role is locked at full access.
- **Authentication method**: standard email-based authentication (password and/or magic link) for v1, including session revocation/logout, password reset, email-change re-verification, and account recovery as in-scope behaviors. JWT validation pins algorithm/issuer/audience and refresh-token rotation/reuse handling per the constitution; concrete token TTLs and hashing parameters are decided in the plan. Enterprise SSO/SAML/SCIM is an enterprise-tier concern, out of scope here.
- **Access inherits downward only**: there is no sibling or upward sharing; cross-branch collaboration requires an explicit grant on each branch. Per-business role overrides (inheriting a role from an ancestor but downgrading/upgrading it on a specific descendant) are a recognized future enhancement, out of scope for v1.
- **Email delivery is available**: invitations and verification rely on an available outbound-email capability provided by the platform.
- **Billing and subscription management are out of scope** for this feature.
- **The business capabilities themselves are out of scope**: CRM, ticketing, inbound email, AI agents, and feature-request/bug-report flows are separate features that consume this foundation; this spec only establishes accounts, the hierarchy, membership/roles (including the principal model that admits AI agents as role-holders), isolation, and auditing. The agent *lifecycle* (creating, configuring, and running agents) belongs to the separate agents feature.
