# Hub User Invitation System

## Status
**Proposed**

**Date:** 2026-05-07

**Related:** [server-auth-design](.design/hosted/auth/server-auth-design.md), [user-access-tokens](.design/user-access-tokens.md), [access-visibility](.design/access-visibility.md)

---

## 1. Problem Statement

Today, the Hub controls user access via two mechanisms:

1. **Authorized Domains** (`server.auth.authorized_domains`) — a domain allowlist checked at login time by `isEmailAuthorized()` in `handlers_auth.go`. If the list is empty, all authenticated users are admitted. If populated, only emails matching a listed domain (or wildcard subdomain) may log in. Admin emails in `admin_emails` bypass the domain check.

2. **User Role/Status** — after login, users have a role (`admin`, `member`, `viewer`) and status (`active`, `suspended`). Admins can suspend users via the `/admin/users` page.

**What's missing:** There is no way to restrict access to *specific invited users* rather than entire domains. A hub operator who wants to grant access to `alice@gmail.com` and `bob@outlook.com` — but not all of gmail.com — has no mechanism to do so. There is also no self-service invite flow: admins cannot generate a shareable link that lets a new user onboard themselves.

### Goals

- Hub admins can enable an **"invited users only"** access mode where only explicitly allow-listed users (plus admin emails) can log in.
- Admins can manage an **explicit user allow list** via both the web UI and CLI.
- Admins can generate **time-expiring invite codes/links** with configurable expiration from preset durations.
- Users who receive an invite link can click it to authenticate and get added to the allow list automatically.
- The feature coexists cleanly with the existing `authorized_domains` mechanism.

### Non-Goals

- Email delivery of invites (the admin copies and shares the link manually).
- Per-grove or per-resource invite scoping (this is hub-level access only).
- Replacing the existing `authorized_domains` mechanism.
- Rate limiting or abuse prevention beyond basic expiration (can be added later).

---

## 2. Research Summary

### 2.1 Current Access Control Flow

All login endpoints (`handleAuthLogin`, `handleAuthToken`, `handleCLIAuthToken`, `completeOAuthLogin`) call the same gate:

```go
// handlers_auth.go:1221
func isEmailAuthorized(email string, authorizedDomains []string, adminEmails []string) bool
```

- If `authorizedDomains` is empty → allow all.
- Admin emails bypass domain check.
- Otherwise, extract domain from email and match against the list (supports `*.example.com` wildcards).

This function is the **single integration point** for the new invite system.

### 2.2 Configuration Path

Hub configuration flows through:

```
settings.yaml (server.auth section)
  → V1AuthConfig (pkg/config/settings_v1.go:380)
  → GlobalConfig.Auth (pkg/config/hub_config.go)
  → hub.ServerConfig (pkg/hub/server.go:53)
  → isEmailAuthorized() at login time
```

Environment variable override: `SCION_SERVER_AUTH_AUTHORIZEDDOMAINS`.

The admin server config API (`PUT /api/v1/admin/server-config`) can update `settings.yaml` and hot-reload some fields. The `authorized_domains` field is **not** currently in the hot-reload path — it requires a server restart. This design proposes storing the allow list in the **database** (not settings.yaml) to enable dynamic updates.

### 2.3 User Data Model

```go
// pkg/store/models.go:451
type User struct {
    ID          string    // UUID primary key
    Email       string
    DisplayName string
    AvatarURL   string
    Role        string    // admin, member, viewer
    Status      string    // active, suspended
    Preferences *UserPreferences
    Created     time.Time
    LastLogin   time.Time
    LastSeen    time.Time
}
```

Store interface provides: `CreateUser`, `GetUser`, `GetUserByEmail`, `UpdateUser`, `DeleteUser`, `ListUsers(filter, opts)`.

Users are created on first login (find-or-create pattern in auth handlers). The `ensureHubMembership()` call adds them to the `hub-members` group.

### 2.4 Existing Token/Code Patterns

The **User Access Token (UAT)** system (`pkg/store/models.go:863`) provides a reference pattern:
- Opaque token with prefix (`scion_pat_`), 32 bytes of randomness, base64url-encoded.
- Stored as SHA-256 hash (never exposed after creation).
- Has expiry, revocation, and last-used tracking.
- CRUD via store interface + service layer + API handlers.

The invite code system should follow this same pattern for code generation, storage, and validation.

### 2.5 Web UI Patterns

Admin pages use LitElement + Shoelace web components with consistent patterns:
- Table with pagination, sorting, and action dropdowns (`admin-users.ts`).
- Create/edit dialogs with form validation (`token-list.ts`, `env-var-list.ts`).
- Confirmation dialogs for destructive actions.
- Feedback alerts with auto-clear after 5 seconds.
- API calls via `apiFetch()` wrapper.

### 2.6 CLI Patterns

Hub commands use Cobra framework under `scion hub`:
- Service-oriented hubclient with typed methods.
- `--format json` for machine-readable output, plain text default.
- `--non-interactive` flag for automation.
- Resource resolution by name, slug, or ID.

---

## 3. Design

### 3.1 Access Mode: `user_access_mode`

Add a new hub-level setting that controls how user access is evaluated at login time.

#### Configuration

**Settings YAML** (`server.auth` section):
```yaml
server:
  auth:
    # Existing fields
    authorized_domains: ["example.com"]

    # New field
    user_access_mode: "open"  # "open" (default) | "domain_restricted" | "invite_only"
```

**Environment variable:** `SCION_SERVER_AUTH_USERACCESSMODE`

#### Semantics

| Mode | Behavior |
|------|----------|
| `open` | All authenticated users are allowed (current default when `authorized_domains` is empty). Domain check still applies if `authorized_domains` is set. |
| `domain_restricted` | Equivalent to current behavior when `authorized_domains` is populated. Explicit for clarity. |
| `invite_only` | Only users on the explicit allow list (or in `admin_emails`) may log in. `authorized_domains` is **also** checked if set — the two mechanisms compose (both must pass). |

When `user_access_mode` is `invite_only`, the login flow becomes:

```
1. Authenticate with OAuth provider (unchanged)
2. Check admin_emails bypass → Allow
3. Check authorized_domains (if set) → Deny if domain mismatch
4. Check user allow list → Deny if email not on list
5. Allow
```

#### Config Struct Changes

```go
// pkg/config/settings_v1.go
type V1AuthConfig struct {
    DevMode           bool     `json:"dev_mode,omitempty" yaml:"dev_mode,omitempty"`
    DevToken          string   `json:"dev_token,omitempty" yaml:"dev_token,omitempty"`
    DevTokenFile      string   `json:"dev_token_file,omitempty" yaml:"dev_token_file,omitempty"`
    AuthorizedDomains []string `json:"authorized_domains,omitempty" yaml:"authorized_domains,omitempty"`
    UserAccessMode    string   `json:"user_access_mode,omitempty" yaml:"user_access_mode,omitempty"` // NEW
}

// pkg/hub/server.go — ServerConfig
type ServerConfig struct {
    // ... existing fields ...
    UserAccessMode string // NEW: "open", "domain_restricted", "invite_only"
}
```

The `UserAccessMode` field should be included in the hot-reload path in `reloadSettings()` so it takes effect without a restart.

### 3.2 User Allow List

The allow list stores email addresses of users who are permitted to log in when `invite_only` mode is active. It is stored in the **database** (not settings.yaml) for dynamic management.

#### Data Model

```go
// pkg/store/models.go
type AllowListEntry struct {
    ID        string    `json:"id"`        // UUID
    Email     string    `json:"email"`     // Normalized lowercase email
    Note      string    `json:"note"`      // Admin-provided note (e.g., "contractor, expires Q3")
    AddedBy   string    `json:"addedBy"`   // User ID of the admin who added this entry
    InviteID  string    `json:"inviteId"`  // If added via invite code redemption, the invite ID
    Created   time.Time `json:"created"`
}
```

#### Store Interface

```go
// pkg/store/store.go
type AllowListStore interface {
    AddAllowListEntry(ctx context.Context, entry *AllowListEntry) error
    RemoveAllowListEntry(ctx context.Context, email string) error
    GetAllowListEntry(ctx context.Context, email string) (*AllowListEntry, error)
    ListAllowListEntries(ctx context.Context, opts ListOptions) (*ListResult[AllowListEntry], error)
    IsEmailAllowed(ctx context.Context, email string) (bool, error)
}
```

#### Authorization Integration

Modify the authorization check to consult the allow list:

```go
// pkg/hub/handlers_auth.go
func (s *Server) isUserAuthorized(ctx context.Context, email string) bool {
    // Admin emails always bypass
    emailLower := strings.ToLower(email)
    for _, admin := range s.config.AdminEmails {
        if strings.ToLower(admin) == emailLower {
            return true
        }
    }

    // Domain check (applies in all modes when domains are configured)
    if len(s.config.AuthorizedDomains) > 0 {
        if !isEmailAuthorized(email, s.config.AuthorizedDomains, s.config.AdminEmails) {
            return false
        }
    }

    // Access mode check
    switch s.config.UserAccessMode {
    case "invite_only":
        allowed, _ := s.store.IsEmailAllowed(ctx, emailLower)
        return allowed
    case "domain_restricted":
        // Already checked above; if we got here with domains configured, we passed
        return len(s.config.AuthorizedDomains) > 0
    default: // "open"
        return true
    }
}
```

This replaces the current `isEmailAuthorized()` calls in `handleAuthLogin`, `handleAuthToken`, `handleCLIAuthToken`, and `completeOAuthLogin` (4 call sites).

### 3.3 Invite Codes

Invite codes are short-lived, shareable tokens that allow a new user to join the hub. When redeemed, the user's email is added to the allow list.

#### Data Model

```go
// pkg/store/models.go
type InviteCode struct {
    ID          string     `json:"id"`          // UUID
    Code        string     `json:"-"`           // The code value (only exposed at creation)
    CodeHash    string     `json:"-"`           // SHA-256 hash for lookup
    CodePrefix  string     `json:"codePrefix"`  // First 8 chars for identification

    // Configuration
    MaxUses     int        `json:"maxUses"`     // 0 = unlimited, 1 = single-use
    ExpiresAt   time.Time  `json:"expiresAt"`   // Absolute expiry time

    // Lifecycle
    UseCount    int        `json:"useCount"`    // How many times redeemed
    Revoked     bool       `json:"revoked"`
    CreatedBy   string     `json:"createdBy"`   // Admin user ID who created it
    Created     time.Time  `json:"created"`

    // Optional metadata
    Note        string     `json:"note"`        // Admin-provided description
}
```

#### Code Format

```
scion_inv_<base64url-encoded-random-24-bytes>
```

- **Prefix `scion_inv_`**: Distinguishes from PATs (`scion_pat_`), dev tokens (`scion_dev_`).
- **Body**: 24 bytes of cryptographic randomness, base64url-encoded (32 chars).
- **Full length**: ~42 characters.

The prefix allows quick identification without database lookup. The code is shown once at creation; only the hash is stored.

#### Expiration Presets

The UI and CLI offer these preset durations (configurable at creation):

| Label | Duration |
|-------|----------|
| 5 minutes | 5m |
| 15 minutes | 15m |
| 30 minutes | 30m |
| 1 hour | 1h |
| 4 hours | 4h |
| 12 hours | 12h |
| 24 hours | 24h |
| 3 days | 72h |
| 5 days | 120h |

Maximum expiration is 5 days. These are **presets only** — the API accepts any `expiresAt` timestamp up to 5 days, allowing custom durations via CLI.

#### Single-Use vs. Multi-Use

- **Single-use** (`maxUses: 1`): Default. The invite is consumed after one redemption. Suitable for inviting a specific person.
- **Multi-use** (`maxUses: 0` for unlimited, or a specific count): For onboarding a batch of users, e.g. a workshop or team. Each redemption increments `useCount`.

When `useCount >= maxUses` (and `maxUses > 0`), the invite is considered exhausted.

#### Store Interface

```go
type InviteCodeStore interface {
    CreateInviteCode(ctx context.Context, invite *InviteCode) error
    GetInviteCodeByHash(ctx context.Context, codeHash string) (*InviteCode, error)
    ListInviteCodes(ctx context.Context, opts ListOptions) (*ListResult[InviteCode], error)
    IncrementInviteUseCount(ctx context.Context, id string) error
    RevokeInviteCode(ctx context.Context, id string) error
    DeleteInviteCode(ctx context.Context, id string) error
}
```

### 3.4 Invite Link Format

The invite link encodes the code in a URL that the hub's web frontend can handle:

```
https://<hub-url>/invite?code=scion_inv_<base64url-random>
```

The web frontend handles this route by:
1. Extracting the code from the query parameter.
2. If the user is not authenticated → redirecting to OAuth login, preserving the invite code in session/state.
3. After authentication → calling the invite redemption API.
4. On success → redirecting to the dashboard with a welcome message.

### 3.5 Invite Redemption Flow

```
User clicks invite link
  → GET /invite?code=scion_inv_xxx
  → Web frontend checks auth state
  │
  ├─ Not authenticated:
  │    → Store code in sessionStorage
  │    → Redirect to /login
  │    → After OAuth callback, retrieve code from sessionStorage
  │    → POST /api/v1/auth/invite/redeem { "code": "scion_inv_xxx" }
  │
  └─ Already authenticated:
       → POST /api/v1/auth/invite/redeem { "code": "scion_inv_xxx" }
       │
       ├─ User email already on allow list → 200 OK (idempotent)
       ├─ Code valid, not expired, not exhausted:
       │    → Add email to allow list
       │    → Increment useCount
       │    → 200 OK { "user": {...}, "message": "You've been added" }
       ├─ Code expired → 410 Gone
       ├─ Code revoked → 410 Gone
       ├─ Code exhausted (useCount >= maxUses) → 410 Gone
       └─ Code not found → 404
```

#### Redemption Handler

```go
// POST /api/v1/auth/invite/redeem
type InviteRedeemRequest struct {
    Code string `json:"code"`
}

type InviteRedeemResponse struct {
    User    *UserResponse `json:"user"`
    Message string        `json:"message"`
}
```

The redemption endpoint requires authentication (the user must have completed OAuth). The flow:

1. Validate the code format (must start with `scion_inv_`).
2. Hash the code and look up the invite.
3. Check expiry, revocation, and use count.
4. Add the authenticated user's email to the allow list (idempotent — skip if already present).
5. Increment the invite's use count.
6. If user was previously denied due to allow list (their User record may not exist), create the user record now.

**Important:** The redemption endpoint performs its own `isEmailAuthorized` domain check — a user from a non-authorized domain cannot bypass domain restrictions via an invite code. The invite only adds them to the allow list; the domain check is a separate gate.

### 3.6 API Endpoints

#### Allow List Management (admin-only)

```
GET    /api/v1/admin/allow-list              List allow list entries (paginated)
POST   /api/v1/admin/allow-list              Add email to allow list
DELETE /api/v1/admin/allow-list/{email}       Remove email from allow list
```

Request/response types:

```go
// POST /api/v1/admin/allow-list
type AllowListAddRequest struct {
    Email string `json:"email"` // Required
    Note  string `json:"note"`  // Optional
}

// GET /api/v1/admin/allow-list
type AllowListResponse struct {
    Items      []AllowListEntry `json:"items"`
    TotalCount int              `json:"totalCount"`
    NextCursor string           `json:"nextCursor,omitempty"`
}
```

#### Invite Code Management (admin-only)

```
GET    /api/v1/admin/invites                 List invite codes (paginated)
POST   /api/v1/admin/invites                 Create invite code
GET    /api/v1/admin/invites/{id}            Get invite code details
POST   /api/v1/admin/invites/{id}/revoke     Revoke invite code
DELETE /api/v1/admin/invites/{id}            Delete invite code
```

Request/response types:

```go
// POST /api/v1/admin/invites
type InviteCreateRequest struct {
    ExpiresIn string `json:"expiresIn"` // Duration string: "5m", "1h", "24h", etc.
    MaxUses   int    `json:"maxUses"`   // 0 = unlimited, default 1
    Note      string `json:"note"`      // Optional description
}

type InviteCreateResponse struct {
    Code      string      `json:"code"`      // Full code, shown once
    InviteURL string      `json:"inviteUrl"` // Complete invite link
    Invite    *InviteCode `json:"invite"`    // Metadata (without code)
}

// GET /api/v1/admin/invites
type InviteListResponse struct {
    Items      []InviteCode `json:"items"`
    TotalCount int          `json:"totalCount"`
    NextCursor string       `json:"nextCursor,omitempty"`
}
```

#### Invite Redemption (authenticated users)

```
POST   /api/v1/auth/invite/redeem            Redeem an invite code
```

### 3.7 Web UI

#### Admin Users Page Enhancement

The existing `/admin/users` page (`scion-page-admin-users`) gains a new section or tab for managing the allow list and invites. Two approaches:

**Option A (Recommended): Extend the existing Admin Users page** with a tabbed interface:
- **Tab 1: Users** — existing user management table (current functionality).
- **Tab 2: Allow List** — manage the explicit email allow list.
- **Tab 3: Invites** — create and manage invite codes.

**Option B: Separate admin page** at `/admin/invites` — keeps pages focused but adds navigation complexity.

#### Allow List Tab

- Table showing: email, note, added by, added at, source (manual / invite).
- "Add Email" button opens dialog with email input + optional note.
- Delete action per row with confirmation.
- Search/filter by email.

#### Invites Tab

- Table showing: code prefix, note, status (active/expired/revoked/exhausted), uses (count/max), expires at, created by, created at.
- "Create Invite" button opens dialog:
  - Expiration dropdown (preset durations from §3.3).
  - Max uses selector: single-use (default) / multi-use (number input) / unlimited.
  - Optional note field.
- On create success: reveal dialog showing the full invite link with a "Copy Link" button (following the token creation reveal pattern from `token-list.ts`).
- Per-row actions: revoke (if active), delete.

#### Access Mode Setting

In the **Admin Server Config** page (`/admin/server-config`), under the Auth section:
- Add a `user_access_mode` dropdown/radio group with the three options: Open, Domain Restricted, Invite Only.
- When "Invite Only" is selected, show an informational note linking to the Allow List management.

#### Invite Landing Page

New page at `/invite`:
- If not authenticated: show a branded page with "You've been invited to join [Hub Name]" and a "Sign in to accept" button.
- If authenticated: auto-redeem the code and show success/error state.
- On success: "Welcome! You now have access." with a "Go to Dashboard" button.
- On error (expired/revoked): "This invite is no longer valid. Contact your hub administrator."

### 3.8 CLI

New `scion hub` subcommands:

#### Allow List Commands

```
scion hub allow-list                      # List allow list entries
scion hub allow-list add <email>          # Add email to allow list
  --note "description"                    # Optional note
scion hub allow-list remove <email>       # Remove email from allow list
  --yes                                   # Skip confirmation
```

#### Invite Commands

```
scion hub invite create                   # Create a new invite code
  --expires 1h                            # Expiration (required, from preset or custom)
  --max-uses 1                            # Max uses (default 1, 0=unlimited)
  --note "For the new contractor"         # Optional note

scion hub invite list                     # List invite codes
  --format json                           # JSON output

scion hub invite revoke <id>              # Revoke an invite code
  --yes                                   # Skip confirmation

scion hub invite delete <id>              # Delete an invite code
  --yes                                   # Skip confirmation
```

**Output examples:**

```
$ scion hub invite create --expires 1h --note "Onboarding"
Invite created successfully.

  Code:    scion_inv_dGhpcyBpcyBhIHRlc3QgY29kZQ
  Link:    https://hub.example.com/invite?code=scion_inv_dGhpcyBpcyBhIHRlc3QgY29kZQ
  Expires: 2026-05-07 15:30:00 UTC (in 1 hour)
  Max uses: 1 (single-use)

Share this link with the person you want to invite.
The code will not be shown again.
```

```
$ scion hub invite list
ID                                    PREFIX          STATUS    USES    EXPIRES              NOTE
a1b2c3d4-...                          scion_in...     active    0/1     2026-05-07 15:30 UTC  Onboarding
e5f6g7h8-...                          scion_in...     expired   3/0     2026-05-06 10:00 UTC  Workshop
```

#### Hub Client Interface

```go
// pkg/hubclient/client.go
type Client interface {
    // ... existing methods ...
    AllowList() AllowListService
    Invites() InviteService
}

type AllowListService interface {
    List(ctx context.Context, opts *ListOptions) (*AllowListResponse, error)
    Add(ctx context.Context, email, note string) (*AllowListEntry, error)
    Remove(ctx context.Context, email string) error
}

type InviteService interface {
    Create(ctx context.Context, req *InviteCreateRequest) (*InviteCreateResponse, error)
    List(ctx context.Context, opts *ListOptions) (*InviteListResponse, error)
    Get(ctx context.Context, id string) (*InviteCode, error)
    Revoke(ctx context.Context, id string) error
    Delete(ctx context.Context, id string) error
}
```

---

## 4. Security Considerations

### 4.1 Code Entropy

Invite codes use 24 bytes (192 bits) of `crypto/rand` randomness, base64url-encoded. This provides sufficient entropy to prevent brute-force guessing. At 192 bits, an attacker making 1 billion guesses per second would need ~2×10^49 years to find a valid code.

### 4.2 Code Storage

Following the UAT pattern, codes are stored as SHA-256 hashes. The plaintext code is returned once at creation and never stored or logged. The `codePrefix` (first 8 chars) allows admins to identify codes without exposing the full value.

### 4.3 Expiration Enforcement

Expiry is checked at redemption time, not via background cleanup. Expired codes remain in the database for audit purposes but cannot be redeemed. Admins can delete them to clean up.

### 4.4 Domain Check Composition

Invite codes **do not bypass** the `authorized_domains` check. If both are configured, a user must pass both gates. This prevents an invite code from granting access to a user from a blocked domain.

### 4.5 Invite-Only Mode and Existing Users

When switching to `invite_only` mode:
- Users already logged in retain their sessions until tokens expire.
- On next login attempt, users not on the allow list will be denied.
- Existing users in the database are **not** automatically added to the allow list.
- Admins should populate the allow list before enabling `invite_only` mode.

The admin UI should warn about this when changing the access mode.

### 4.6 Redemption Rate Limiting

The initial implementation does not include rate limiting on the redemption endpoint. Since redemption requires OAuth authentication first, the attack surface is limited to authenticated users guessing codes. The high entropy of codes (192 bits) makes this impractical. Rate limiting can be added later if needed.

### 4.7 Audit Trail

All allow list and invite operations should emit structured log entries:

```
authz: allow_list_add email=alice@example.com added_by=admin-uuid
authz: invite_created id=invite-uuid expires=2026-05-07T15:30:00Z created_by=admin-uuid
authz: invite_redeemed id=invite-uuid email=bob@example.com
authz: login_denied email=charlie@example.com reason=not_on_allow_list
```

---

## 5. Database Schema

### SQLite (primary)

```sql
CREATE TABLE allow_list (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    note TEXT DEFAULT '',
    added_by TEXT NOT NULL,
    invite_id TEXT DEFAULT '',
    created DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_allow_list_email ON allow_list(email);

CREATE TABLE invite_codes (
    id TEXT PRIMARY KEY,
    code_hash TEXT NOT NULL UNIQUE,
    code_prefix TEXT NOT NULL,
    max_uses INTEGER NOT NULL DEFAULT 1,
    use_count INTEGER NOT NULL DEFAULT 0,
    expires_at DATETIME NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_by TEXT NOT NULL,
    note TEXT DEFAULT '',
    created DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_invite_codes_hash ON invite_codes(code_hash);
CREATE INDEX idx_invite_codes_expires ON invite_codes(expires_at);
```

---

## 6. Implementation Phases

### Phase 1: Allow List Foundation

**Goal:** Enable `invite_only` mode with manual allow list management.

1. Add `user_access_mode` to `V1AuthConfig`, `GlobalConfig.Auth`, `ServerConfig`.
2. Create `allow_list` database table and store interface.
3. Implement `isUserAuthorized()` replacing `isEmailAuthorized()` in auth handlers.
4. Add hot-reload for `user_access_mode` in `reloadSettings()`.
5. Admin API endpoints: `GET/POST/DELETE /api/v1/admin/allow-list`.
6. CLI commands: `scion hub allow-list [add|remove|list]`.
7. Web UI: Allow List tab on Admin Users page.
8. Access mode selector in Admin Server Config page.

**Deliverables:** Admins can enable invite-only mode and manually add/remove emails.

### Phase 2: Invite Codes

**Goal:** Generate and manage invite codes/links.

1. Create `invite_codes` database table and store interface.
2. Invite code generation service (following UAT pattern).
3. Admin API endpoints: `GET/POST/DELETE /api/v1/admin/invites`, `POST .../revoke`.
4. Invite redemption endpoint: `POST /api/v1/auth/invite/redeem`.
5. CLI commands: `scion hub invite [create|list|revoke|delete]`.
6. Web UI: Invites tab on Admin Users page.
7. Invite landing page (`/invite`).
8. Frontend invite redemption flow (sessionStorage code preservation across OAuth redirect).

**Deliverables:** Full invite code lifecycle with web and CLI management.

### Phase 3: Polish and Observability

**Goal:** Production hardening.

1. Structured audit logging for all allow list and invite operations.
2. SSE events for invite creation/redemption (for real-time UI updates).
3. Bulk allow list import (CSV upload or CLI batch).
4. Admin dashboard widget showing pending invites and recent redemptions.
5. Email domain suggestions when adding to allow list (autocomplete from existing users).

**Deliverables:** Audit trail, real-time updates, bulk operations.

---

## 7. Open Questions (Resolved)

1. **Auto-populate allow list on mode switch?** When switching to `invite_only`, should all existing active users be automatically added to the allow list? ~~**Recommendation:** Yes, with a confirmation dialog that lists the users being added.~~ **Resolved:** Yes, auto-populate from existing users. The UI shows the count of users that will be added, not the full list.

2. **Allow list + domain interaction.** If `authorized_domains: ["example.com"]` and `user_access_mode: "invite_only"`, should an invited user from `gmail.com` be admitted? ~~Current design says no (both gates must pass). This is safer but may surprise admins. **Recommendation:** Keep the composition (both must pass), but show a clear warning in the UI when both are configured.~~ **Resolved:** Both invite-only AND authorized-domains must pass. Users cannot be added to the allow list if their email is outside the authorized domains. The UI shows a clear warning when both are configured.

3. **Invite code for CLI login.** Should CLI login (`scion hub auth login`) support passing an invite code? ~~**Recommendation:** Yes, add `--invite-code` flag to `scion hub auth login` that calls the redemption endpoint after authentication.~~ **Resolved:** Yes, support CLI for invite management. CLI commands provided for allow-list and invite management.

4. **Max invite code lifetime.** Should there be a hard ceiling on invite code expiration (e.g., no more than 24 hours)? ~~**Recommendation:** The 24-hour preset is the maximum in the UI, but the API allows custom values up to 7 days. Admin discretion.~~ **Resolved:** Maximum expiration is 5 days. Presets updated accordingly: 5m, 15m, 30m, 1h, 4h, 12h, 24h, 3d, 5d.

5. **Revoke all invites on mode change.** When switching from `invite_only` back to `open`, should outstanding invite codes be automatically revoked? ~~**Recommendation:** No — the codes become inert (the allow list isn't checked in `open` mode), and the admin may want to switch back later.~~ **Resolved:** No. Unused codes stay inert when invite-only is disabled. Leave them in place so the admin can switch back later.

6. **Allow list scope.** Should the allow list support wildcards (e.g., `*@team.example.com`) like `authorized_domains`? ~~**Recommendation:** No — keep the allow list as exact emails only. Wildcards belong in `authorized_domains`. This maintains a clean separation of concerns.~~ **Resolved:** Exact emails only. No wildcards — those belong in authorized-domains. Clean separation of concerns.

---

## 8. References

- `pkg/hub/handlers_auth.go` — Authentication handlers, `isEmailAuthorized()`
- `pkg/hub/server.go` — `ServerConfig` struct
- `pkg/config/settings_v1.go` — `V1AuthConfig`, `V1ServerConfig`
- `pkg/config/hub_config.go` — Configuration loading, env var mapping
- `pkg/store/models.go` — `User`, `UserAccessToken` models
- `pkg/store/store.go` — Store interface
- `pkg/hub/admin_settings.go` — Admin server config API
- `web/src/components/pages/admin-users.ts` — Admin Users page
- `web/src/components/shared/token-list.ts` — Token management UI pattern
- `.design/user-access-tokens.md` — UAT design (code generation/storage pattern)
- `.design/access-visibility.md` — Visibility and access control design
- `.design/hosted/auth/server-auth-design.md` — Server authentication architecture
