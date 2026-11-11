# Access & Visibility: Design Analysis and Phased Plan

> **Superseded by [ptone/scion#1793](https://github.com/ptone/scion/issues/1793).** This
> document proposes building the `visibility` trait out into a full access-control mechanism.
> The opposite decision was made: `visibility` is being removed. It has already been removed
> from agents, templates, and harness configs (ptone/scion#1904); the skills slice — the only
> one with live behavior — is tracked separately (ptone/scion#1903). Do not use this document
> to guide new work; it is retained only for historical context on the analysis that preceded
> the removal decision.

## Status
**Proposed** (superseded — see notice above)

---

## 1. Research Summary

### 1.1 What Exists Today

The `visibility` field is present across four resource types in the Hub:

| Resource | Schema Location | Values | Default | Settable at Creation | Updatable |
|----------|----------------|--------|---------|---------------------|-----------|
| **Grove** | `pkg/ent/schema/grove.go` | private, team, public | private | Yes (API request) | Yes (PATCH) |
| **Agent** | `pkg/ent/schema/agent.go` | private, team, public | private | No (hardcoded) | No |
| **Template** | `pkg/store/models.go` | private, grove, public | private | Yes (API request) | Yes (PATCH) |
| **Harness Config** | `pkg/store/models.go` | private, grove, public | private | Yes (API request) | Yes (PATCH) |

**Visibility constants** are defined canonically in `pkg/api/types.go:487-492`:
```go
const (
    VisibilityPrivate = "private" // Only the owner can access
    VisibilityTeam    = "team"    // Team members can access
    VisibilityPublic  = "public"  // Anyone can access (read-only)
)
```

### 1.2 How It's Used (and Not Used)

**Stored**: Yes -- all four resource types persist visibility in the database.

**Displayed**: The web frontend shows visibility as a read-only badge on the agent detail page (Configuration tab) and provides a select dropdown (Private/Team/Public) on the grove creation form.

**Filtered**: Only groves support query-time filtering (`GET /api/v1/groves?visibility=public`). No other resource supports visibility-based filtering.

**Enforced for access control**: **No**. The `AuthzService.CheckAccess()` method in `pkg/hub/authz.go` evaluates policies, owner bypass, and admin bypass -- but never examines the visibility field. Visibility is purely metadata today.

### 1.3 Terminology Divergence
<!--  feedback; we should modify the terms to be 'grove-team' to refer to all members of the grove group 

and 'public' -> everyone - subtle, but public makes it sound like open to all outside the hub user set

where everyone slightly implies that everyone who is "here" on the hub

visibililty does a good job of capturing the read-only

We can improve this by including "who can see this (changes are sill guarded by other policy) -->
There is an inconsistency in the middle-tier visibility value:

| Resource | Middle Tier |
|----------|------------|
| Agents, Groves | `team` |
| Templates, Harness Configs | `grove` |

This reflects different design intents: `team` implies cross-project team collaboration, while `grove` ties sharing to the project boundary. In practice, these serve similar purposes -- making a resource visible to the set of users who collaborate on a grove/project.

### 1.4 Relationship to the Permissions System

The permissions system (`permissions-design.md`, `groups-design.md`) provides fine-grained RBAC:

- **Groups**: Explicit (manual membership) and dynamic grove groups (`grove_agents`)
- **Policies**: Scope hierarchy (hub > grove > resource), action-based (read, update, delete, etc.), with allow/deny effects
- **Policy bindings**: Link principals (users, agents, groups) to policies
- **Delegation**: Agents can optionally inherit policy-addressable creator relationships

The permissions system is the intended enforcement mechanism. Visibility is designed to be a **coarse-grained layer on top** -- a simple, user-facing access level that translates into policy behavior without requiring users to manually create policies for common access patterns.

### 1.5 Current State of Agent Visibility

Agent visibility is effectively inert:
- Hardcoded to `"private"` at creation (`pkg/hub/handlers.go`)
- Not included in the `CreateAgentRequest` struct
- Not included in the agent update struct
- Not used in any query filter or access check

The agent detail page shows it as a badge, but it is always "private" and cannot be changed.

---

## 2. Design Intent

Based on the design documents (`hub-api.md` Section 10.3, `permissions-design.md`, `groups-design.md`, `hosted-templates.md`), the intended semantics are:

### 2.1 Visibility Levels

| Level | Intent | Who Can Access |
|-------|--------|---------------|
| **private** | Owner-only by default | The resource owner and hub admins. Others require explicit policy grants. |
| **team** | Shared within the grove's collaborator set | All users who are members of the grove (i.e., belong to a group associated with the grove). Read access is implicit; write access still requires explicit policy. |
| **public** | Platform-wide read access | All authenticated hub users can read. Write access still requires explicit policy. |

### 2.2 Relationship Between Visibility and Policies

Visibility should act as **implicit policy scaffolding**:

- `private` = default-deny, no implicit grants beyond owner
- `team` = implicit read grant to grove members (users in the grove's associated groups)
- `public` = implicit read grant to all authenticated users

Explicit policies always override these defaults. A deny policy can restrict access even for `public` resources. An allow policy can grant access even for `private` resources.

### 2.3 Inheritance Model

The design documents suggest (but do not mandate) that agent visibility could inherit from or be constrained by the parent grove:

- An agent in a `team` grove could default to `team` visibility
<!-- feedback; yes - agents become implicit group (team) members for the grove -->
- An agent should not have broader visibility than its grove (a `public` agent in a `private` grove would be contradictory)

---

## 3. Phased Implementation Plan

### Phase 1: Normalize and Expose (Low Risk, High Clarity)

**Goal**: Clean up the data model, unify terminology, and make visibility user-controllable across all resources.

#### 1a. Unify visibility values

Standardize on `private`, `team`, `public` for all resource types. Update templates and harness configs to use `team` instead of `grove` for the middle tier.

<!-- feedback - to clarify - the association of grove with template and harness configs is more than just visibility no? Is there not some degree of "attachment" or association. That is, a grove based template is available to be used only in that grove, and would show up in the list of templates only when creating an agent in that grove? -->

- `pkg/store/models.go`: Update Template and HarnessConfig comments from "private, grove, public" to "private, team, public"
- `pkg/hub/template_handlers.go`: Accept `grove` as an alias for `team` on input (backwards compatibility), but always store and return `team`
- `pkg/hub/harness_config_handlers.go`: Same treatment
- Add a validation function in `pkg/api/types.go`:
  ```go
  func NormalizeVisibility(v string) string {
      switch v {
      case "grove":
          return VisibilityTeam
      case "private", "team", "public":
          return v
      default:
          return VisibilityPrivate
      }
  }
  ```

#### 1b. Make agent visibility configurable

- Add `Visibility` field to `CreateAgentRequest` in `pkg/hub/handlers.go`
- Add `Visibility` to the agent update struct
- Default to the parent grove's visibility when not specified
- Validate that agent visibility does not exceed grove visibility (`private < team < public`)

#### 1c. Frontend: agent visibility control

- Add visibility edit control to the agent detail Configuration tab (currently read-only badge)
- Add visibility select to agent creation flow (if one exists in the web UI)

#### 1d. Add visibility filter to agent listing

- Support `?visibility=` query parameter on `GET /api/v1/groves/{groveId}/agents`
- Add `Visibility` field to `AgentFilter` struct (or equivalent)

**Deliverables**: Unified terminology, user-controllable visibility on all resources, query filtering.

---

### Phase 2: Visibility-Aware Listing (Medium Risk, High Value)

**Goal**: Make visibility affect what resources users can see in list endpoints, using the existing permissions infrastructure.

#### 2a. Define visibility resolution logic

Create a reusable helper that determines whether a caller can see a resource based on its visibility:

```go
// CanSeeResource returns true if the caller's identity permits
// viewing a resource with the given visibility and ownership.
func (s *AuthzService) CanSeeResource(
    ctx context.Context,
    identity Identity,
    resource Resource,
    visibility string,
    ownerID string,
    groveID string,
) bool {
    // Admin bypass
    if identity.IsAdmin() {
        return true
    }

    // Owner bypass
    if identity.ID() == ownerID {
        return true
    }

    switch visibility {
    case VisibilityPublic:
        return true // All authenticated users
    case VisibilityTeam:
        return s.isGroveMember(ctx, identity, groveID)
    case VisibilityPrivate:
        // Check explicit policies
        decision := s.CheckAccess(ctx, identity, resource, ActionRead)
        return decision.Allowed
    default:
        return false
    }
}
```

#### 2b. Apply visibility filtering to list endpoints

For each list endpoint, apply post-query filtering (or preferably query-time filtering) based on the caller's identity and each resource's visibility:

| Endpoint | Current Behavior | New Behavior |
|----------|-----------------|-------------|
| `GET /api/v1/groves` | Returns all groves | Filter by visibility + caller identity |
| `GET /api/v1/groves/{id}/agents` | Returns all agents in grove | Filter by visibility + caller identity |
| `GET /api/v1/templates` | Returns all templates | Filter by visibility + caller identity |
| `GET /api/v1/harness-configs` | Returns all harness configs | Filter by visibility + caller identity |

#### 2c. Grove membership check

Implement `isGroveMember` by checking whether the user belongs to any group associated with the grove. This leverages the existing group infrastructure:

- Check explicit groups where `grove_id` matches
- Check dynamic grove groups (`group_type = grove_agents` for agent callers)
- Check transitive group membership

**Deliverables**: Users only see resources they're allowed to see. `team` visibility grants read access to grove collaborators. `public` resources are visible to all.

---

### Phase 3: Visibility-Aware Access Control (Medium Risk, Core Security)

**Goal**: Make visibility affect read/write access decisions, not just listing.

#### 3a. Integrate visibility into CheckAccess

Extend the authorization decision flow to consider visibility before falling through to explicit policies:

```
1. Admin bypass -> Allow
2. Owner bypass -> Allow
3. Visibility check:
   a. public + read -> Allow
   b. team + read + grove member -> Allow
   c. private -> fall through
4. Explicit policy evaluation -> Allow/Deny
5. Default deny
```

This means `team` and `public` visibility provide **implicit read access** without requiring explicit policies. Write access always requires explicit policies or ownership.

#### 3b. Apply to single-resource endpoints

Ensure GET endpoints for individual resources (not just listings) respect visibility:

- `GET /api/v1/groves/{id}` -- visibility check
- `GET /api/v1/groves/{id}/agents/{agentId}` -- visibility check
- `GET /api/v1/templates/{id}` -- visibility check

#### 3c. Visibility change authorization

Only the resource owner or an admin should be able to change a resource's visibility. Broadening visibility (private -> team -> public) is allowed by owners. Narrowing is also allowed but should warn about breaking existing access patterns.

**Deliverables**: Visibility is a real access control mechanism. `team` resources are readable by grove collaborators without explicit policies.

---

### Phase 4: Grove-Level Defaults and Inheritance (Low Risk, UX Polish)

**Goal**: Reduce friction by having groves define default visibility for new resources.

#### 4a. Grove default visibility setting

Add a `default_agent_visibility` field to groves:

```go
field.String("default_agent_visibility").Default("private")
```

When creating an agent without specifying visibility, inherit from the grove's default.

#### 4b. Visibility ceiling enforcement

An agent's visibility cannot exceed its grove's visibility:

| Grove Visibility | Allowed Agent Visibility |
|-----------------|------------------------|
| private | private |
| team | private, team |
| public | private, team, public |

Enforce at creation and update time. Return a 400 error if the constraint is violated.

#### 4c. Template visibility in grove context

When listing templates with `scope=grove`, templates with `team` visibility in that grove should be visible to grove members. Global `public` templates are visible to all.

**Deliverables**: Sensible defaults, visibility inheritance, constraint enforcement.

---

### Phase 5: Audit and Observability (Low Risk, Operational)

**Goal**: Provide transparency into how visibility affects access decisions.

#### 5a. Authorization decision logging

Include visibility in authorization decision debug logs:

```
authz: ALLOW user=alice resource=agent/code-reviewer action=read
       reason=visibility:team grove_member=true
```

#### 5b. Visibility change events

Emit telemetry/SSE events when resource visibility changes:

```json
{
  "type": "visibility_changed",
  "resourceType": "grove",
  "resourceId": "grove-uuid",
  "oldVisibility": "private",
  "newVisibility": "team",
  "changedBy": "user-uuid"
}
```

#### 5c. API: "who can access" endpoint

Optional: Add an endpoint that resolves effective access for a resource, showing how visibility and policies combine:

```
GET /api/v1/groves/{id}/access
```

**Deliverables**: Operational clarity, audit trail, debugging support.

---

## 4. Key Design Decisions

### 4.1 Visibility as Implicit Policy, Not a Replacement

Visibility should not bypass the policy engine. Instead, it should be evaluated as an implicit policy during the access check flow. This keeps the authorization model unified and allows explicit deny policies to override visibility grants.

### 4.2 Team = Grove Collaborators

The `team` visibility level maps to "users who are members of groups associated with this grove." This leverages the existing group infrastructure (explicit groups, dynamic grove groups) without introducing a new concept. In multi-grove deployments, different groves can have different team compositions.

<!-- feedback; see feedback above on the rename to grove-team - this clarifies that while there may be real world "teams" working across multiple groves, this can not address that.  That use-case will be addressed in teh future with more fine grained policy management, where you can assign a "group" (aka team) access to multple resourcss  -->

### 4.3 Read-Only Implicit Grants

Visibility only provides implicit **read** access. Write, update, and delete actions always require explicit policies or ownership. This prevents visibility from accidentally granting destructive capabilities.

### 4.4 Agent Visibility Bounded by Grove

An agent cannot be more visible than its parent grove. This prevents information leakage where a `public` agent in a `private` grove would expose the grove's existence.

---

## 5. Migration Considerations

- Existing groves default to `private` and will continue to work as-is
- Existing agents are `private` and immutable today; making them configurable is additive
- Templates using `grove` visibility will be normalized to `team` on read (aliased on write for backwards compat)
- No data migration needed -- all changes are backwards-compatible defaults
- Phases are independent and can be implemented incrementally

---

## 6. References

- `hub-api.md` -- Section 10.3 (Authorization Model), Section 2.1 (Agent Model), Section 2.2 (Grove Model)
- `auth/permissions-design.md` -- RBAC policy model, resolution algorithm, open questions on default visibility
- `auth/groups-design.md` -- Principal model, group types, grove groups, delegation
- `access-mvp.md` -- Scope-based authorization patterns for env vars/secrets
- `hosted-templates.md` -- Template visibility model
- `harness-config-hub-storage.md` -- Harness config visibility
- `agent-detail-layout.md` -- Frontend visibility badge
- `pkg/hub/authz.go` -- Current authorization implementation
- `pkg/api/types.go` -- Visibility constants
