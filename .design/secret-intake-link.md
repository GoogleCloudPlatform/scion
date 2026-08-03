# Secret Intake Links — Secure Secret Submission from Chat

**Status:** Approved — implementing
**Created:** 2026-06-05
**Updated:** 2026-06-06
**Author:** Coordinator agent
**Related:** [secrets.md](./hosted/secrets.md), [agent-progeny-secret-access.md](./agent-progeny-secret-access.md)

---

## 1. Problem

When users interact with Scion agents via chat channels (Telegram, Discord, Slack,
Google Chat), agents sometimes need secrets (API keys, tokens, credentials). Today
the only paths to set a secret are the CLI (`scion hub secret set`) or the web UI
(requires login). Both require leaving the chat context.

The practical result: users paste raw secrets into chat messages. This happened in
the project's own history — a GitHub PAT was pasted directly into Telegram. Chat
messages are logged, cached, and potentially indexed. Once a secret is in a chat
log, it's compromised.

## 2. Solution: Authenticated Secret Intake Links

A short-lived, one-time-use URL that deep-links into the authenticated Hub web UI.
The user clicks the link, logs in if needed, pastes the secret value, and submits.
The secret is stored immediately — no pending/confirmation flow.

### User flow

```
1. Agent needs GITHUB_TOKEN for project "my-app"

2. Agent (or coordinator) calls Hub API:
   POST /api/v1/secret-intake
   {key: "GITHUB_TOKEN", scope: "project", scopeId: "my-app",
    type: "environment", description: "GitHub PAT for repo access"}

3. Hub returns: {url: "https://hub.example.com/intake#<JWT>", expiresAt: "..."}

4. Agent sends in chat:
   "I need your GitHub token. Paste it here (expires in 15 min):
    https://hub.example.com/intake#eyJ..."

5. User clicks link → logs in if needed → sees focused form:
   "Secret: GITHUB_TOKEN
    Why: GitHub PAT for repo access
    Where: Project my-app
    Type: Environment variable
    Paste your value here: [______]
    [Submit]"

6. User pastes value and submits → Hub stores secret immediately via secretBackend.Set()

7. Hub sends notification to originating chat channel:
   "Secret GITHUB_TOKEN stored for project my-app"

8. Done — secret is active and available to agents
```

### Why this is simpler and better

- **Zero friction:** click link, paste, submit — one step
- **Authenticated:** user must be logged in (Hub session), no anonymous endpoints
- **No pending state:** secret is stored immediately on submit
- **No confirmation step:** login IS the authentication
- **No OTP, no IP tracking, no rate limiting on submit:** login is the gate

## 3. Design

### 3.1 API: Create Intake Link

```
POST /api/v1/secret-intake
Authorization: Bearer <user-or-agent-token>

{
  "key":         "GITHUB_TOKEN",
  "scope":       "project",
  "scope_id":    "my-app-project-id",
  "type":        "environment",
  "target":      "",
  "description": "GitHub PAT for repo access",
  "ttl_seconds": 900,
  "channel":     "telegram",
  "channel_context": "group-chat-id-or-thread"
}

Response 201:
{
  "url":        "https://hub.example.com/intake#<JWT>",
  "expires_at": "2026-06-05T22:30:00Z",
  "intake_id":  "uuid"
}
```

**Authorization:** Requires user or agent token. The resulting secret will be
created under the authenticated user's identity.

### 3.2 JWT Structure

The JWT is in the URL fragment — never sent to the server in HTTP requests. The
intake page decodes it client-side to display context.

```json
{
  "iss": "scion-hub",
  "sub": "secret-intake",
  "iat": 1749163800,
  "exp": 1749164700,
  "jti": "<intake-id>",
  "key": "GITHUB_TOKEN",
  "scope": "project",
  "scope_id": "my-app-project-id",
  "type": "environment",
  "target": "",
  "description": "GitHub PAT for repo access"
}
```

Signed with the Hub's user signing key (HS256).

### 3.3 Intake Record (Server State)

```go
type SecretIntake struct {
    ID              string
    Key             string
    Scope           string
    ScopeID         string
    SecretType      string
    Target          string
    Description     string
    UserID          string     // creator of the link
    Channel         string     // originating chat channel (telegram, discord, etc.)
    ChannelContext  string     // channel-specific routing info (group ID, thread ID)
    ExpiresAt       time.Time
    CreatedAt       time.Time
    Consumed        bool
    ConsumedAt      *time.Time
}
```

Lifecycle: created → consumed (on store) or expired (janitor).

### 3.4 API: Store Secret via Intake

```
POST /api/v1/secret-intake/{id}/store
Authorization: Bearer <user-token> (user must be logged in)

{
  "token": "<JWT from URL fragment>",
  "value": "ghp_abc123..."
}

Response 200: {"status": "stored", "key": "GITHUB_TOKEN"}
Response 400: {"error": "invalid or expired token"}
Response 401: {"error": "unauthorized — login required"}
Response 404: {"error": "intake not found or expired"}
Response 410: {"error": "intake already used"}
```

Server-side:
1. Verify user is authenticated (login required)
2. Verify JWT signature + expiry
3. Verify JWT jti matches intake ID in URL
4. Atomically consume the intake (check not expired, not already consumed)
5. Store secret via `secretBackend.Set()`
6. Send notification to originating chat channel

### 3.5 Cleanup

Janitor (background goroutine):
- Intakes older than 1 hour past `ExpiresAt` → delete record

### 3.6 Intake Page (Web UI)

Single page at `/intake` (requires login):
1. Reads JWT from URL fragment (client-side)
2. Decodes payload — displays key, description, scope, type
3. Shows textarea for the secret value + submit button
4. On submit: POST to `/api/v1/secret-intake/{jti}/store` with auth credentials
5. Shows: "Secret stored successfully."
6. If not logged in, shows "Please log in" with login link

### 3.7 CLI Command

```bash
scion hub secret intake GITHUB_TOKEN --scope project --project my-app \
  --type environment --description "GitHub PAT for repo access"

# Output:
# Intake link (expires in 15 minutes):
#   https://hub.example.com/intake#eyJ...
#
# Send this link to the user. After they paste the value,
# you will be asked to confirm in this channel.
```

## 4. Security Analysis

| Threat | Mitigation |
|--------|-----------|
| Link intercepted | User must be logged in to submit; one-time use |
| Unauthorized submission | Requires authenticated Hub session |
| Replay after use | One-time use, consumed flag |
| Stale links | TTL (15 min default, 1 hour max), janitor cleanup |
| JWT tampering | HS256 signature verified server-side |
| Secret in server logs | Value only in POST body, never logged |

## 5. Implementation Plan

### Files to create/modify
- `pkg/hub/secret_intake.go` — handlers, intake lifecycle
- `pkg/hub/secret_intake_test.go` — tests
- `web/src/components/pages/secret-intake.ts` — intake page
- `cmd/hub_secret_intake.go` — CLI command
- `pkg/hub/auth.go` — remove submit endpoint auth exemption

### Files unchanged
- `cmd/hub_secret_intake.go` — CLI command (unchanged)

## 6. Testing

- Given an intake link for GITHUB_TOKEN
  When the user clicks the link, logs in, and pastes a value
  Then the secret is stored immediately and a notification is sent

- Given an intake link
  When a user who is NOT logged in tries to submit
  Then they receive a 401

- Given an expired intake link
  When the user tries to store
  Then they receive a 404

- Given a consumed intake link
  When the user tries to store again
  Then they receive a 410

- Given a tampered JWT
  When the user tries to store
  Then they receive a 400

## 7. Scope

- **In:** API (create/store), intake page, CLI, cleanup
- **Out:** Chat inline buttons (follow-up), secret rotation, bulk intake
