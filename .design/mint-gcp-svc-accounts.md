# Design: Hub-Minted GCP Service Accounts

**Status:** Draft  
**Date:** 2026-04-03  
**Related:** [sciontool-gcp-identity.md](hosted/sciontool-gcp-identity.md), [sciontool-gcp-identity-pt2.md](hosted/sciontool-gcp-identity-pt2.md)

## Problem

Today, grove administrators must pre-create GCP service accounts in their own GCP projects and register them with the Hub via `scion grove service-accounts add`. The Hub then verifies it can impersonate the SA (requiring the user to have already granted `roles/iam.serviceAccountTokenCreator` to the Hub's identity on that SA).

This workflow has friction:
1. Users need GCP IAM expertise to create SAs and configure cross-project impersonation.
2. Each user must own a GCP project to host their service accounts.
3. The permission grant is error-prone and hard to debug when it fails.

## Proposal

Allow the Hub to **mint** (create) new GCP service accounts in the Hub's own GCP project on behalf of users. These minted SAs:
- Are created with **no IAM permissions** — they are permissionless by default.
- Are automatically configured so the Hub SA has `roles/iam.serviceAccountTokenCreator` on them.
- Are stored and associated with groves using the existing `GCPServiceAccount` model.
- Can later be granted IAM permissions on the user's own projects by the user (outside of Scion).

This gives users a zero-setup path to GCP identity for their agents while preserving the existing BYOSA (bring-your-own-service-account) flow.

## Architecture

### Hub Prerequisites

The Hub's operating service account needs `roles/iam.serviceAccountAdmin` on the Hub's GCP project (or a custom role with the same permissions):

| Permission | Used for |
|------------|----------|
| `iam.serviceAccounts.create` | Create the minted SA |
| `iam.serviceAccounts.getIamPolicy`, `iam.serviceAccounts.setIamPolicy` | Grant the Hub SA `roles/iam.serviceAccountTokenCreator` on the minted SA, and the minted SA `roles/iam.serviceAccountUser` on itself (`allow_self_act_as`) |
| `iam.serviceAccounts.get`, `iam.serviceAccounts.delete` | Read the SA and delete it again if a follow-up grant fails |

`roles/iam.serviceAccountCreator` alone is not enough: GCP gives the creator no role on the SA it creates, so the policy grants after create fail and the cleanup delete fails too, leaving an orphaned SA. No project-level `roles/iam.serviceAccountTokenCreator` is needed for minted SAs, because the per-SA grant made during minting covers token generation. Token generation goes through `iamcredentials.googleapis.com`, which must be enabled in the Hub project.

The single-node VM deploy script (`scripts/single-node-vm/deploy.sh`) grants `roles/iam.serviceAccountAdmin` to the hub VM's SA by default (config key `hub_sa_minting`). On that tier the hub SA is the VM SA, so agents in `passthrough` identity mode share this role; passthrough is intended for getting started only.

The Hub must also know its own GCP project ID. This is either:
- Auto-detected from the metadata server (when running on GCE/Cloud Run).
- Configured explicitly via a new `GCPProjectID` field on `ServerConfig`.

### New API Endpoint

```
POST /api/v1/groves/{groveId}/gcp-service-accounts/mint
```

**Request Body:**
```json
{
  "account_id": "my-data-pipeline",          // optional, custom SA account ID (slugified, validated)
  "display_name": "My Data Pipeline SA",     // optional, used for SA display name
  "description": "Agent SA for data work"    // optional, SA description
}
```

If `account_id` is omitted, a random ID is generated (`scion-{8-char-hex}`). If provided, it is prefixed with `scion-`, slugified, and validated against GCP's 6-30 char `[a-z][a-z0-9-]*[a-z0-9]` rules. The endpoint returns `409 Conflict` if the account ID already exists in the project.

**Response:** Standard `GCPServiceAccount` object with additional fields:

```json
{
  "id": "uuid",
  "email": "scion-a1b2c3d4@hub-project.iam.gserviceaccount.com",
  "project_id": "hub-project",
  "display_name": "my-data-pipeline",
  "scope": "grove",
  "scope_id": "grove-uuid",
  "verified": true,
  "verification_status": "verified",
  "managed": true,
  "created_by": "user@example.com",
  "created_at": "2026-04-03T..."
}
```

### Service Account Naming

GCP SA account IDs must be 6-30 chars, `[a-z][a-z0-9-]*[a-z0-9]`. Two modes:

- **Custom:** User provides `account_id` (e.g., `my-pipeline`). Prefixed to `scion-my-pipeline`, slugified, and validated. Returns `409` on collision.
- **Auto-generated:** `scion-{8-char-random-hex}` (e.g., `scion-a1b2c3d4`).

The display name is set from the request (or defaults to `"Scion agent ({grove-slug})"`) and the description includes the grove ID and minting user for traceability.

### Data Model Changes

Add a `managed` boolean to `GCPServiceAccount`:

```go
type GCPServiceAccount struct {
    // ... existing fields ...
    Managed   bool   `json:"managed"`              // true = created by Hub, false = BYOSA
    ManagedBy string `json:"managed_by,omitempty"` // Hub instance ID that created it
}
```

```sql
ALTER TABLE gcp_service_accounts ADD COLUMN managed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gcp_service_accounts ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';
```

This flag is informational — it indicates origin, not lifecycle ownership:
- Display in the UI (badge/label distinguishing "Hub-minted" vs. "User-provided").
- The Hub does **not** delete the underlying GCP SA on removal — deletion of GCP resources is the project admin's responsibility.
- Once minted, the SA email can be registered in other groves via the normal `service-accounts add` flow (treated as any other SA email).

### Implementation Components

#### 1. GCP IAM Admin Client (`pkg/hub/gcp_iam_admin.go`)

New interface wrapping the GCP IAM Admin API (`google.golang.org/api/iam/v1`):

```go
type GCPServiceAccountAdmin interface {
    CreateServiceAccount(ctx context.Context, projectID, accountID, displayName, description string) (email string, uniqueID string, err error)
    SetIAMPolicy(ctx context.Context, saEmail string, hubEmail string, role string) error
}
```

- `CreateServiceAccount` calls `iam.projects.serviceAccounts.create`.
- After creation, `SetIAMPolicy` grants `roles/iam.serviceAccountTokenCreator` to the Hub SA on the new SA.

#### 2. Mint Handler (`pkg/hub/handlers_gcp_identity.go`)

New handler method on `Server`:

```go
func (s *Server) mintGCPServiceAccount(w http.ResponseWriter, r *http.Request) {
    // 1. Authorize: require grove admin or hub admin
    // 2. Validate request
    // 3. Generate or validate account ID (custom slug or scion-{random})
    // 4. Call GCPServiceAccountAdmin.CreateServiceAccount() — 409 on collision
    // 5. Call GCPServiceAccountAdmin.SetIAMPolicy() to grant token creator
    // 6. Create GCPServiceAccount record with managed=true, verified=true
    // 7. Audit log the creation
    // 8. Return response
}
```

#### 3. CLI Command

```bash
scion grove service-accounts mint                                    # Mint with auto-generated ID
scion grove service-accounts mint --account-id my-pipeline           # Custom account ID → scion-my-pipeline
scion grove service-accounts mint --name "My Pipeline SA"            # Custom display name
```

#### 4. ServerConfig Addition

```go
type ServerConfig struct {
    // ... existing fields ...
    GCPProjectID string // Project ID for minting SAs (auto-detected if empty)
}
```

### Quota & Limits

GCP imposes a default limit of **100 service accounts per project**. At scale this becomes a concern.

**Mitigations:**
- Track count of minted SAs per grove (enforce a per-grove cap, e.g., 5).
- Enforce a global cap on total minted SAs (configurable on `ServerConfig`).
- Surface the current count on the Hub admin dashboard.
- GCP quota can be raised to 1000+ via support request if needed.

### Lifecycle & Ownership

The Hub **mints** SAs but does not manage their GCP lifecycle beyond creation:
- **Removal from grove:** `scion grove service-accounts remove` unlinks the SA from the grove in the Hub database. The underlying GCP SA is **not** deleted.
- **Grove deletion:** Managed SAs are retained in GCP with a warning printed in the grove delete confirmation. Since an SA may be registered in multiple groves, coupling deletion to any single grove would be incorrect.
- **GCP resource cleanup:** Deletion of the underlying GCP service account is the responsibility of the GCP project admin, outside of Scion.

The `ManagedBy` field records which Hub instance minted the SA, for traceability in multi-hub scenarios.

## Alternatives Considered

### A. Workload Identity Federation Instead of Minted SAs

Rather than creating real SAs, use [Workload Identity Federation](https://cloud.google.com/iam/docs/workload-identity-federation) to issue short-lived tokens tied to agent identity without persistent SAs.

**Pros:** No SA quota concerns; no persistent credentials to manage.  
**Cons:** Significantly more complex setup (WIF pool + provider per hub); users cannot grant IAM bindings to a WIF principal as intuitively as to an SA email; not all GCP services support WIF principals in IAM policies.

**Verdict:** Good long-term evolution but too complex for the initial feature. Can be added later as an alternative identity mode.

### B. Dedicated GCP Project per Grove

Each grove could have its own GCP project for minted SAs, isolating blast radius.

**Pros:** Perfect isolation; no quota sharing.  
**Cons:** Requires project creation permissions (much higher privilege); project quota limits; massive operational overhead.

**Verdict:** Over-engineered for the current scale. The per-grove cap on minted SAs in a shared project is sufficient.

### C. SA Key-Based Approach (Download JSON Keys)

Mint the SA and download a JSON key, storing it as a secret in the Hub.

**Pros:** Doesn't require the Hub to have ongoing token-creator permissions.  
**Cons:** Storing long-lived SA keys is a significant security risk; keys don't expire; contradicts the existing keyless impersonation architecture.

**Verdict:** Rejected. The impersonation-based approach is strictly better for security.

### D. Users Create SAs via Scion CLI in Their Own Projects

Wrap `gcloud iam service-accounts create` behind a `scion` CLI command that also sets up the impersonation grant.

**Pros:** SAs live in user projects; no shared quota.  
**Cons:** Requires users to have GCP projects and IAM admin permissions; still complex; doesn't solve the "zero GCP knowledge" use case.

**Verdict:** Useful as a complementary power-user flow but doesn't replace the hub-minted approach for simplicity.

## Security Considerations

1. **Blast radius of Hub SA compromise:** If the Hub SA is compromised, the attacker can impersonate all minted SAs. This is the same risk as the existing BYOSA model — the Hub SA is already a high-value target. Minting doesn't materially increase this risk since minted SAs are permissionless by default.

2. **Permissionless by default:** Minted SAs have no IAM roles. Users must explicitly grant permissions on their own projects. The Hub does not facilitate this — it is an out-of-band action.

3. **SA email as stable identifier:** Users will use minted SA emails in their own IAM policies. Since the Hub does not delete minted SAs, this is stable. If a GCP project admin deletes a minted SA, GCP's 30-day tombstone prevents email reuse with a different unique ID.

4. **Audit trail:** All mint operations are recorded via the existing audit logging infrastructure.

5. **Multi-tenancy:** The `managed_by` field provides traceability in federated multi-hub deployments.

## Resolved Decisions

1. **Cross-grove usage:** Once minted, an SA is just an email. Users can register it in any grove via `service-accounts add`. No special scoping needed.

2. **SA deletion:** Out of scope for the Hub. Deletion of the underlying GCP resource is the GCP project admin's responsibility.

3. **IAM grant guidance:** Not provided. Users handle IAM grants out-of-band. Keep it simple.

4. **Naming:** Custom account IDs are supported via `--account-id`, with `scion-` prefix, slug enforcement, and `409` on collision. Falls back to `scion-{random}` if omitted.

5. **Quota monitoring:** Reactive. Handle the quota-exceeded error from GCP rather than pre-checking.

6. **Grove deletion cascade:** SAs are retained (not deleted) when a grove is deleted. A warning is shown in the grove delete confirmation.

## Implementation Plan

### Phase 1: Core Minting ✅
- [x] Add `GCPProjectID` to `ServerConfig` with metadata-server auto-detection
- [x] Implement `GCPServiceAccountAdmin` interface and IAM Admin API client
- [x] Add `managed`/`managed_by` columns (new migration)
- [x] Implement `POST .../mint` endpoint with authz, audit logging, slug validation
- [x] Add `scion grove service-accounts mint` CLI command (`--account-id`, `--name`)
- [x] Add grove-delete warning for retained managed SAs
- [x] Unit tests for admin client, handler, and store changes
- [ ] Integration test with IAM API (requires test project)

### Phase 2: Limits & UI ✅
- [x] Per-grove and global mint caps (configurable)
- [x] Web UI: mint button, managed badge
- [x] Quota visibility on admin dashboard
