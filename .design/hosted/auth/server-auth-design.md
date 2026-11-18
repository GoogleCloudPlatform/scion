# Hub Server Authentication Design

## Status
**Proposed**

## 1. Overview

This document specifies how the Scion Hub server validates and processes authentication from all client types. It provides the unified server-side perspective that complements the client-focused documents (web-auth.md, cli-auth.md, sciontool-auth.md).

### 1.1 Authentication Sources

The Hub server must handle authentication from three distinct sources:

| Source | Client | Transport | Token Type | Identity Context |
|--------|--------|-----------|------------|------------------|
| **Web Users** | Browser via Koa proxy | HTTP with cookies + forwarded token | OAuth access token | Full user identity |
| **CLI Users** | Terminal/scripts | Direct HTTP | OAuth access token or API key | Full user identity |
| **Agents** | sciontool in container | Direct HTTP | Hub-issued JWT | Agent identity (scoped) |
| **Development** | Any | Direct or proxied HTTP | Dev token (`scion_dev_*`) | Pseudo-user (admin) |

### 1.2 Goals

1. **Unified Token Validation** - Single middleware chain that handles all token types
2. **Clear Identity Context** - Every request has a resolved identity (user or agent)
3. **Token Type Detection** - Automatic routing based on token format/prefix
4. **Production Ready** - Support for OAuth token validation, JWT verification, API keys
5. **Graceful Degradation** - Dev mode for local development without OAuth setup

### 1.3 Non-Goals

- OAuth provider integration (handled by web frontend and CLI)
- Token issuance for user sessions (handled by web frontend or future Hub auth endpoints)
- Runtime Broker authentication (covered in runtime-broker-auth.md)

---

## 2. Authentication Architecture

### 2.1 Request Flow Diagram

```
                                    ┌─────────────────────────────────────────────────┐
                                    │                   Hub Server                     │
                                    │                                                  │
┌──────────────┐                   │  ┌──────────────────────────────────────────┐   │
│  Web Browser │─────────────────►│  │           Middleware Chain                │   │
│  (via Koa)   │  Cookie + Token   │  │                                          │   │
└──────────────┘                   │  │  ┌────────────────────────────────────┐  │   │
                                    │  │  │ 1. CORS Middleware                 │  │   │
┌──────────────┐                   │  │  └────────────────────────────────────┘  │   │
│     CLI      │─────────────────►│  │                   │                       │   │
│   (direct)   │  Bearer Token     │  │  ┌────────────────────────────────────┐  │   │
└──────────────┘                   │  │  │ 2. Token Detection & Routing       │  │   │
                                    │  │  │    - Detect token type             │  │   │
┌──────────────┐                   │  │  │    - Route to appropriate validator │  │   │
│   sciontool  │─────────────────►│  │  └────────────────────────────────────┘  │   │
│    (agent)   │  JWT Bearer       │  │                   │                       │   │
└──────────────┘                   │  │  ┌────────────────┬───────────┬────────┐ │   │
                                    │  │  │ 3a. Dev Auth   │ 3b. Agent │ 3c.    │ │   │
                                    │  │  │    Validator   │ JWT       │ User   │ │   │
                                    │  │  │                │ Validator │ Token  │ │   │
                                    │  │  │                │           │ Valid. │ │   │
                                    │  │  └────────────────┴───────────┴────────┘ │   │
                                    │  │                   │                       │   │
                                    │  │  ┌────────────────────────────────────┐  │   │
                                    │  │  │ 4. Identity Resolution             │  │   │
                                    │  │  │    - Set user or agent in context  │  │   │
                                    │  │  │    - Apply authorization checks     │  │   │
                                    │  │  └────────────────────────────────────┘  │   │
                                    │  │                   │                       │   │
                                    │  │  ┌────────────────────────────────────┐  │   │
                                    │  │  │ 5. Handler                         │  │   │
                                    │  │  └────────────────────────────────────┘  │   │
                                    │  └──────────────────────────────────────────┘   │
                                    └─────────────────────────────────────────────────┘
```

### 2.2 Token Type Detection

Tokens are identified by their format and prefix:

| Token Pattern | Type | Validator |
|---------------|------|-----------|
| `scion_dev_*` | Development token | DevAuthMiddleware |
| JWT (3 dot-separated base64 segments, `iss: scion-hub`) | Agent JWT | UnifiedAuthMiddleware |
| JWT (3 dot-separated base64 segments, other issuers) | User access token | UserTokenValidator |
| `sk_live_*` | API key | APIKeyValidator |
| `sk_test_*` | Test API key | APIKeyValidator |
| Other | Unknown/Invalid | Reject with 401 |

### 2.3 Header Priority

When multiple authentication headers are present, they are processed in priority order:

1. **`X-Scion-Agent-Token`** - Agent-specific header (highest priority)
2. **`Authorization: Bearer <token>`** - Standard bearer token
3. **`X-API-Key`** - API key header
4. **Forwarded headers from Koa** - `X-Forwarded-User-*` headers (see Section 4)

---

## 3. Server Configuration

### 3.1 Configuration Schema

```yaml
server:
  auth:
    # Mode: "production", "development", "testing"
    # Determines which validators are enabled
    mode: production  # Default

    # Development authentication (interim)
    dev:
      enabled: false
      token: ""  # If empty and enabled, auto-generate
      tokenFile: ""  # Default: ~/.scion/dev-token

    # Agent JWT configuration
    agent:
      # Signing key for agent tokens (HS256)
      # Required in production; auto-generated if empty in development
      signingKey: ""
      # Alternative: use RS256 with key pair
      privateKeyFile: ""
      publicKeyFile: ""
      # Token lifetime
      tokenDuration: 24h

    # User token validation
    user:
      # OAuth token validation mode:
      # - "introspection": Validate tokens via OAuth provider introspection endpoint
      # - "jwt": Validate JWT tokens locally (requires public keys)
      # - "hub-issued": Validate Hub-issued access tokens (recommended for production)
      # - "trusted-proxy": Trust X-Forwarded-User headers from Koa (see Section 4)
      validationMode: hub-issued

      # For introspection mode: OAuth provider endpoints
      introspection:
        endpoint: ""  # e.g., https://oauth2.googleapis.com/tokeninfo
        clientId: ""
        clientSecret: ""

      # For hub-issued mode: Hub signing key (for Hub-issued access tokens)
      signingKey: ""

      # For trusted-proxy mode: Allowed proxy IPs/CIDRs
      trustedProxies:
        - "127.0.0.1"
        - "::1"
        - "10.0.0.0/8"

    # API key validation
    apiKeys:
      enabled: true
      # Key hashing algorithm
      hashAlgorithm: sha256

    # Rate limiting
    rateLimit:
      enabled: true
      requestsPerMinute: 60
      burstSize: 10

    # Security
    requireHttps: true  # Reject non-HTTPS in production
    allowedOrigins:
      - "https://scion.example.com"
```

### 3.2 Environment Variable Mapping

| Variable | Config Path | Description |
|----------|-------------|-------------|
| `SCION_AUTH_MODE` | `server.auth.mode` | Authentication mode |
| `SCION_DEV_AUTH_ENABLED` | `server.auth.dev.enabled` | Enable dev auth |
| `SCION_DEV_TOKEN` | `server.auth.dev.token` | Development token |
| `SCION_AGENT_SIGNING_KEY` | `server.auth.agent.signingKey` | Agent JWT signing key |
| `SCION_USER_VALIDATION_MODE` | `server.auth.user.validationMode` | User token validation mode |
| `SCION_USER_SIGNING_KEY` | `server.auth.user.signingKey` | User JWT signing key |
| `SCION_TRUSTED_PROXIES` | `server.auth.user.trustedProxies` | Comma-separated proxy IPs |

---

## 4. Web User Authentication (via Koa Proxy)

### 4.1 Architecture

Web users authenticate with the Koa frontend (OAuth), which then proxies API requests to the Hub. The challenge is securely conveying the authenticated user's identity to the Hub.

```
┌─────────┐        ┌─────────────┐        ┌─────────┐
│ Browser │──(1)──►│ Koa Frontend│──(2)──►│   Hub   │
│         │        │   :9820     │        │  :9810  │
└─────────┘        └─────────────┘        └─────────┘
     │                    │                    │
     │ OAuth login        │                    │
     │────────────────►│                    │
     │ Session cookie     │                    │
     │◄────────────────│                    │
     │                    │                    │
     │ API request        │                    │
     │ (with cookie)      │                    │
     │────────────────►│                    │
     │                    │ Forward with       │
     │                    │ user identity      │
     │                    │───────────────►│
     │                    │◄───────────────│
     │◄────────────────│                    │
```

### 4.2 Identity Forwarding Options

There are three approaches for the Koa proxy to forward user identity to the Hub:

#### Option A: Hub-Issued Access Token (Recommended)

The Koa frontend exchanges the OAuth token for a Hub-issued access token during login. This token is then used for all Hub API calls.

**Flow:**
1. User completes OAuth login in Koa frontend
2. Koa calls `POST /api/v1/auth/login` on Hub with OAuth provider token
3. Hub validates OAuth token, creates/updates user record, issues Hub access token
4. Koa stores Hub access token in session
5. For API requests, Koa forwards the Hub access token in `Authorization` header

**Hub Endpoint:**
```
POST /api/v1/auth/login
Request:
{
  "provider": "google",        // or "github"
  "providerToken": "...",      // OAuth access token from provider
  "email": "user@example.com", // From OAuth payload
  "name": "User Name",
  "avatar": "https://..."
}

Response:
{
  "user": {
    "id": "user-uuid",
    "email": "user@example.com",
    "displayName": "User Name",
    "role": "member"
  },
  "accessToken": "eyJ...",   // Hub-issued JWT
  "refreshToken": "eyJ...",
  "expiresIn": 3600
}
```

**Pros:**
- Hub controls all token validation
- No trust delegation to proxy
- Tokens can be revoked centrally
- Works for both web and CLI

**Cons:**
- Requires Hub to call OAuth provider for initial validation
- Additional roundtrip during login

#### Option B: Trusted Proxy Headers

The Koa frontend sets trusted headers that the Hub accepts from known proxy IPs.

**Headers Set by Koa:**
```
X-Forwarded-User-Id: user-uuid
X-Forwarded-User-Email: user@example.com
X-Forwarded-User-Name: User Name
X-Forwarded-User-Role: member
X-Forwarded-Auth-Provider: google
X-Forwarded-Auth-Time: 2025-01-30T12:00:00Z
```

**Hub Validation:**
1. Check if request originates from trusted proxy IP/CIDR
2. Validate required headers are present
3. Check auth time is recent (within session lifetime)
4. Construct user identity from headers

**Security Requirements:**
- Hub MUST only accept these headers from `trustedProxies` IPs
- Hub MUST reject if any required header is missing
- Hub SHOULD validate auth time to prevent replay
- Hub SHOULD add signature verification (HMAC of headers with shared secret)

**Pros:**
- No Hub-side OAuth integration needed
- Simpler implementation
- Lower latency (no token exchange)

**Cons:**
- Trust boundary at network level
- Harder to revoke individual sessions
- Requires careful proxy IP configuration

#### Option C: OAuth Token Passthrough

Koa forwards the OAuth provider's access token, and Hub validates it directly with the provider.

**Pros:**
- Direct token validation with provider
- No Hub-issued tokens to manage

**Cons:**
- Hub must integrate with each OAuth provider
- Rate limiting concerns with provider APIs
- Token format varies by provider

### 4.3 Recommended Approach

For production deployments, **Option A (Hub-Issued Access Token)** is recommended because:

1. Centralized token management in Hub
2. Consistent token format regardless of OAuth provider
3. Supports both web and CLI flows
4. Token revocation is straightforward
5. No trust delegation to proxy layer

For development/testing, the existing dev-auth flow (Option B with dev token) is sufficient.

### 4.4 Koa Proxy Implementation

```typescript
// web/src/server/routes/api.ts (updated)

async function proxyToHub(ctx: Context) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Request-ID': ctx.state.requestId,
    'X-Forwarded-For': ctx.ip,
  };

  // Priority 1: Development token (dev mode only)
  if (ctx.state.devToken) {
    headers['Authorization'] = `Bearer ${ctx.state.devToken}`;
  }
  // Priority 2: Hub access token from session (production)
  else if (ctx.session?.hubAccessToken) {
    headers['Authorization'] = `Bearer ${ctx.session.hubAccessToken}`;

    // Check if token needs refresh
    if (ctx.session.hubTokenExpiry && Date.now() > ctx.session.hubTokenExpiry - 60000) {
      const newToken = await refreshHubToken(ctx.session.hubRefreshToken);
      if (newToken) {
        ctx.session.hubAccessToken = newToken.accessToken;
        ctx.session.hubRefreshToken = newToken.refreshToken;
        ctx.session.hubTokenExpiry = Date.now() + newToken.expiresIn * 1000;
        headers['Authorization'] = `Bearer ${newToken.accessToken}`;
      }
    }
  }
  // Priority 3: Client-provided authorization (passthrough)
  else if (ctx.headers.authorization) {
    headers['Authorization'] = ctx.headers.authorization;
  }

  // ... rest of proxy logic
}
```

---

## 5. CLI User Authentication

### 5.1 Token Flow

CLI users authenticate directly with the Hub using OAuth (localhost callback) or API keys.

```
┌───────────┐                              ┌─────────┐
│    CLI    │                              │   Hub   │
│           │                              │  :9810  │
└───────────┘                              └─────────┘
      │                                          │
      │ GET /api/v1/auth/authorize               │
      │ (request OAuth URL)                      │
      │─────────────────────────────────────────►│
      │◄─────────────────────────────────────────│
      │ { authUrl, state }                       │
      │                                          │
      │ [User completes OAuth in browser]        │
      │                                          │
      │ POST /api/v1/auth/token                  │
      │ { code, redirectUri, grantType }         │
      │─────────────────────────────────────────►│
      │                                          │ (Hub validates with OAuth provider)
      │◄─────────────────────────────────────────│
      │ { accessToken, refreshToken, user }      │
      │                                          │
      │ [Store in ~/.scion/credentials.json]     │
      │                                          │
      │ GET /api/v1/agents                       │
      │ Authorization: Bearer <accessToken>      │
      │─────────────────────────────────────────►│
      │                                          │ (Hub validates token)
      │◄─────────────────────────────────────────│
```

### 5.2 Hub-Issued Tokens

For CLI users, the Hub issues its own JWT tokens after validating the OAuth code:

```go
type UserTokenClaims struct {
    jwt.RegisteredClaims

    UserID     string `json:"uid"`
    Email      string `json:"email"`
    Role       string `json:"role"`
    TokenType  string `json:"type"`   // "access" or "refresh"
    ClientType string `json:"client"` // "cli", "web", "api"
}
```

**Token Lifetimes:**

| Token Type | Lifetime | Use Case |
|------------|----------|----------|
| CLI Access Token | 30 days | Long-lived for CLI convenience |
| Web Access Token | 15 minutes | Short-lived for web sessions |
| Refresh Token | 7 days | Token renewal |

### 5.3 Token Validation Endpoint

```
POST /api/v1/auth/validate
Request:
{
  "token": "eyJ..."
}

Response:
{
  "valid": true,
  "user": {
    "id": "user-uuid",
    "email": "user@example.com",
    "displayName": "User Name",
    "role": "member"
  },
  "expiresAt": "2025-02-28T12:00:00Z",
  "tokenType": "access",
  "clientType": "cli"
}
```

---

## 6. Agent Authentication (sciontool)

### 6.1 Token Flow

Agents receive a Hub-issued JWT during provisioning. See [sciontool-auth.md](sciontool-auth.md) for detailed token format.

```
┌───────────┐         ┌──────────────┐         ┌─────────┐
│    Hub    │──(1)───►│ Runtime Broker │──(2)───►│  Agent  │
│           │ Provision│             │ Start   │Container│
└───────────┘ +token  └──────────────┘ +env    └─────────┘
                                                     │
     ┌───────────────────────────────────────────────┘
     │
     │ POST /api/v1/agents/{id}/status
     │ Authorization: Bearer <JWT>
     │ X-Scion-Agent-Token: <JWT>
     │────────────────────────────────────────────►│
                                                    │ Hub
                                                    │ validates
                                                    │ JWT
```

### 6.2 Agent Token Validation

`UnifiedAuthMiddleware` (`pkg/hub/auth.go`, Section 7) handles agent JWT validation directly:

- Extracts a candidate token via `extractAgentToken(r)` (`X-Scion-Agent-Token` first, otherwise `Authorization: Bearer`).
- Validates the JWT signature and claims via `cfg.AgentTokenSvc.ValidateAgentToken(token)` and checks credential status and run scope (`verifyAgentTokenCredential` in `pkg/hub/agent_token_verify.go`).
- Stores the validated claims and `AgentIdentity` on the request context before invoking downstream handlers (rejecting invalid `X-Scion-Agent-Token` headers, or falling through to user token validation when `Authorization: Bearer` is not an agent JWT).

### 6.3 Agent vs User Context

Handlers must distinguish between agent and user contexts:

```go
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
    // Check for agent context first
    if agentClaims := GetAgentFromContext(r.Context()); agentClaims != nil {
        // Agent is reporting its own status
        // Verify agent can only update its own status
        if agentClaims.Subject != extractID(r, "/api/v1/agents/") {
            writeError(w, http.StatusForbidden, ErrCodeForbidden,
                "agents can only update their own status", nil)
            return
        }
        s.handleAgentStatusUpdate(w, r, agentClaims)
        return
    }

    // Check for user context
    if user := GetUserFromContext(r.Context()); user != nil {
        // User is querying agent status (read permission check applies)
        s.handleAgentStatusQuery(w, r, user)
        return
    }

    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
        "authentication required", nil)
}
```

---

## 7. Unified Authentication Middleware

### 7.1 Middleware Chain Order

```go
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
    // Apply in reverse order (last applied runs first)

    // 5. Recovery (innermost - catches panics)
    h = s.recoveryMiddleware(h)

    // 4. Logging
    h = s.loggingMiddleware(h)

    // 3. Authorization (checks permissions after identity is resolved)
    if s.authzService != nil {
        h = AuthzMiddleware(s.authzService)(h)
    }

    // 2. Authentication (resolves identity)
    h = s.authMiddleware(h)

    // 1. CORS (outermost - handles preflight)
    if s.config.CORSEnabled {
        h = s.corsMiddleware(h)
    }

    return h
}
```

### 7.2 Unified Auth Middleware

```go
// pkg/hub/auth.go

// AuthConfig holds authentication configuration.
type AuthConfig struct {
    Mode            string // "production", "development", "testing"
    DevAuthEnabled  bool
    DevAuthToken    string
    AgentTokenSvc   *AgentTokenService
    UserTokenSvc    *UserTokenService
    TrustedProxies  []string
    Debug           bool
}

// authMiddleware creates the unified authentication middleware.
func authMiddleware(cfg AuthConfig) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ctx := r.Context()

            // Skip auth for health endpoints
            if isHealthEndpoint(r.URL.Path) {
                next.ServeHTTP(w, r)
                return
            }

            // Step 1: Try agent token (X-Scion-Agent-Token or agent JWT in Bearer)
            if token := extractAgentToken(r); token != "" {
                if claims, err := cfg.AgentTokenSvc.ValidateAgentToken(token); err == nil {
                    ctx = context.WithValue(ctx, agentContextKey{}, claims)
                    if cfg.Debug {
                        log.Printf("[Auth] Agent authenticated: %s", claims.Subject)
                    }
                    next.ServeHTTP(w, r.WithContext(ctx))
                    return
                } else if r.Header.Get("X-Scion-Agent-Token") != "" {
                    // Agent token header was present but invalid
                    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                        "invalid agent token", nil)
                    return
                }
                // Bearer token wasn't an agent token, continue to user auth
            }

            // Step 2: Extract bearer token
            token := extractBearerToken(r)
            if token == "" {
                // Step 2a: Check for trusted proxy headers
                if isTrustedProxy(r, cfg.TrustedProxies) {
                    if user := extractProxyUser(r); user != nil {
                        ctx = context.WithValue(ctx, userContextKey{}, user)
                        if cfg.Debug {
                            log.Printf("[Auth] Proxy user: %s", user.Email())
                        }
                        next.ServeHTTP(w, r.WithContext(ctx))
                        return
                    }
                }

                writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                    "missing authorization header", nil)
                return
            }

            // Step 3: Detect token type and validate
            switch detectTokenType(token) {
            case tokenTypeDev:
                if !cfg.DevAuthEnabled {
                    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                        "development authentication is not enabled", nil)
                    return
                }
                if !apiclient.ValidateDevToken(token, cfg.DevAuthToken) {
                    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                        "invalid development token", nil)
                    return
                }
                ctx = context.WithValue(ctx, userContextKey{}, &DevUser{id: "dev-user"})
                if cfg.Debug {
                    log.Printf("[Auth] Dev user authenticated")
                }

            case tokenTypeUser:
                claims, err := cfg.UserTokenSvc.ValidateUserToken(token)
                if err != nil {
                    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                        "invalid access token: "+err.Error(), nil)
                    return
                }
                user := &AuthenticatedUser{
                    id:          claims.UserID,
                    email:       claims.Email,
                    displayName: claims.DisplayName,
                    role:        claims.Role,
                }
                ctx = context.WithValue(ctx, userContextKey{}, user)
                if cfg.Debug {
                    log.Printf("[Auth] User authenticated: %s", user.Email())
                }

            case tokenTypeAPIKey:
                user, err := cfg.APIKeySvc.ValidateAPIKey(ctx, token)
                if err != nil {
                    writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                        "invalid API key", nil)
                    return
                }
                ctx = context.WithValue(ctx, userContextKey{}, user)
                if cfg.Debug {
                    log.Printf("[Auth] API key authenticated: %s", user.Email())
                }

            default:
                writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
                    "unrecognized token format", nil)
                return
            }

            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}

// Token type detection
type tokenType int

const (
    tokenTypeUnknown tokenType = iota
    tokenTypeDev
    tokenTypeUser
    tokenTypeAPIKey
    tokenTypeAgent
)

func detectTokenType(token string) tokenType {
    switch {
    case strings.HasPrefix(token, "scion_dev_"):
        return tokenTypeDev
    case strings.HasPrefix(token, "sk_live_"), strings.HasPrefix(token, "sk_test_"):
        return tokenTypeAPIKey
    case looksLikeJWT(token):
        // Could be user or agent JWT - need to inspect claims
        // For now, assume user token (agent tokens use X-Scion-Agent-Token)
        return tokenTypeUser
    default:
        return tokenTypeUnknown
    }
}

func looksLikeJWT(token string) bool {
    parts := strings.Split(token, ".")
    return len(parts) == 3
}
```

### 7.3 Identity Interface

```go
// pkg/hub/identity.go

// Identity represents an authenticated identity (user or agent).
type Identity interface {
    ID() string
    Type() string // "user", "agent", "dev"
}

// UserIdentity represents an authenticated user.
type UserIdentity interface {
    Identity
    Email() string
    DisplayName() string
    Role() string
}

// AgentIdentity represents an authenticated agent.
type AgentIdentity interface {
    Identity
    GroveID() string
    Scopes() []AgentTokenScope
    HasScope(scope AgentTokenScope) bool
}

// AuthenticatedUser implements UserIdentity.
type AuthenticatedUser struct {
    id          string
    email       string
    displayName string
    role        string
}

func (u *AuthenticatedUser) ID() string          { return u.id }
func (u *AuthenticatedUser) Type() string        { return "user" }
func (u *AuthenticatedUser) Email() string       { return u.email }
func (u *AuthenticatedUser) DisplayName() string { return u.displayName }
func (u *AuthenticatedUser) Role() string        { return u.role }

// GetIdentityFromContext returns the authenticated identity (user or agent).
func GetIdentityFromContext(ctx context.Context) Identity {
    if user := GetUserFromContext(ctx); user != nil {
        return user
    }
    if agent := GetAgentFromContext(ctx); agent != nil {
        return &agentIdentityWrapper{agent}
    }
    return nil
}
```

---

## 8. API Key Authentication

### 8.1 API Key Format

```
sk_live_<base64-encoded-payload>
```

**Payload structure:**
```json
{
  "kid": "key-uuid",
  "uid": "user-uuid",
  "created": "2025-01-01T00:00:00Z"
}
```

### 8.2 API Key Storage

API keys are stored hashed in the database:

```go
type APIKey struct {
    ID          string            `json:"id"`
    UserID      string            `json:"userId"`
    Name        string            `json:"name"`
    KeyHash     string            `json:"-"`  // SHA-256 hash of the key
    Prefix      string            `json:"prefix"` // First 8 chars for identification
    Scopes      []string          `json:"scopes,omitempty"`
    LastUsed    *time.Time        `json:"lastUsed,omitempty"`
    ExpiresAt   *time.Time        `json:"expiresAt,omitempty"`
    Created     time.Time         `json:"created"`
}
```

### 8.3 API Key Validation

```go
func (s *APIKeyService) ValidateAPIKey(ctx context.Context, key string) (UserIdentity, error) {
    // Parse the key
    if !strings.HasPrefix(key, "sk_live_") && !strings.HasPrefix(key, "sk_test_") {
        return nil, ErrInvalidKeyFormat
    }

    // Hash the key
    hash := sha256.Sum256([]byte(key))
    hashStr := hex.EncodeToString(hash[:])

    // Look up by hash
    apiKey, err := s.store.GetAPIKeyByHash(ctx, hashStr)
    if err != nil {
        return nil, ErrInvalidAPIKey
    }

    // Check expiration
    if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
        return nil, ErrAPIKeyExpired
    }

    // Update last used (async)
    go s.store.UpdateAPIKeyLastUsed(context.Background(), apiKey.ID)

    // Look up user
    user, err := s.store.GetUser(ctx, apiKey.UserID)
    if err != nil {
        return nil, ErrUserNotFound
    }

    return &AuthenticatedUser{
        id:          user.ID,
        email:       user.Email,
        displayName: user.DisplayName,
        role:        user.Role,
    }, nil
}
```

---

## 9. Hub Auth Endpoints

### 9.1 Endpoint Summary

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/auth/authorize` | GET | Get OAuth authorization URL |
| `/api/v1/auth/token` | POST | Exchange code for tokens |
| `/api/v1/auth/refresh` | POST | Refresh access token |
| `/api/v1/auth/validate` | POST | Validate a token |
| `/api/v1/auth/logout` | POST | Invalidate tokens |
| `/api/v1/auth/me` | GET | Get current user info |
| `/api/v1/auth/api-keys` | GET/POST | List/create API keys |
| `/api/v1/auth/api-keys/{id}` | DELETE | Delete API key |

### 9.2 OAuth Authorization

```
GET /api/v1/auth/authorize?redirect_uri=http://localhost:18271/callback&state=abc123

Response:
{
  "authUrl": "https://accounts.google.com/o/oauth2/v2/auth?...",
  "state": "abc123"
}
```

### 9.3 Token Exchange

```
POST /api/v1/auth/token
Request:
{
  "code": "4/0AX4...",
  "redirectUri": "http://localhost:18271/callback",
  "grantType": "authorization_code",
  "codeVerifier": "abc123..."  // PKCE
}

Response:
{
  "accessToken": "eyJ...",
  "refreshToken": "eyJ...",
  "expiresIn": 3600,
  "tokenType": "Bearer",
  "user": {
    "id": "user-uuid",
    "email": "user@example.com",
    "displayName": "User Name",
    "role": "member"
  }
}
```

### 9.4 Token Refresh

```
POST /api/v1/auth/refresh
Request:
{
  "refreshToken": "eyJ..."
}

Response:
{
  "accessToken": "eyJ...",
  "refreshToken": "eyJ...",
  "expiresIn": 3600
}
```

### 9.5 Logout / Token Revocation

```
POST /api/v1/auth/logout
Request:
{
  "refreshToken": "eyJ..."  // Optional: revoke specific token
}

Response:
{
  "success": true
}
```

---

## 10. Security Considerations

### 10.1 Token Security

| Token Type | Storage | Transmission | Revocation |
|------------|---------|--------------|------------|
| User access | Client-side (memory/secure storage) | HTTPS only | Expiration + blacklist |
| User refresh | Client-side (secure storage) | HTTPS only | Explicit revocation |
| Agent JWT | Environment variable | HTTPS only | Expiration |
| API key | Client-managed | HTTPS only | Delete from Hub |
| Dev token | Local file (0600) | HTTPS (enforced) | Delete file |

### 10.2 HTTPS Enforcement

In production mode, the Hub MUST:
1. Reject HTTP requests (unless behind a TLS-terminating proxy)
2. Set `Strict-Transport-Security` header
3. Require `Secure` flag on any cookies

```go
func (s *Server) enforceHTTPS(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if s.config.RequireHTTPS && !isTLS(r) {
            writeError(w, http.StatusForbidden, ErrCodeForbidden,
                "HTTPS required", nil)
            return
        }
        next.ServeHTTP(w, r)
    })
}

func isTLS(r *http.Request) bool {
    // Check direct TLS
    if r.TLS != nil {
        return true
    }
    // Check proxy headers
    if r.Header.Get("X-Forwarded-Proto") == "https" {
        return true
    }
    return false
}
```

### 10.3 Rate Limiting

Authentication endpoints have stricter rate limits:

| Endpoint | Limit | Window | Scope |
|----------|-------|--------|-------|
| `/auth/token` | 10 | 1 minute | IP |
| `/auth/authorize` | 10 | 1 minute | IP |
| `/auth/refresh` | 20 | 1 minute | Token |
| `/auth/validate` | 60 | 1 minute | Token |
| Other API | 60 | 1 minute | User |

### 10.4 Audit Logging

All authentication events are logged:

```go
type AuthEvent struct {
    EventType  string    `json:"eventType"`  // login, logout, token_refresh, api_key_created
    UserID     string    `json:"userId,omitempty"`
    AgentID    string    `json:"agentId,omitempty"`
    ClientType string    `json:"clientType"` // web, cli, api, agent
    IPAddress  string    `json:"ipAddress"`
    UserAgent  string    `json:"userAgent"`
    Success    bool      `json:"success"`
    FailReason string    `json:"failReason,omitempty"`
    Timestamp  time.Time `json:"timestamp"`
    RequestID  string    `json:"requestId"`
}
```

### 10.5 Token Blacklist

For immediate token revocation (logout, security incidents):

```go
type TokenBlacklist interface {
    // Add token to blacklist until its expiration
    Add(ctx context.Context, tokenID string, expiry time.Time) error
    // Check if token is blacklisted
    IsBlacklisted(ctx context.Context, tokenID string) (bool, error)
    // Cleanup expired entries (periodic job)
    Cleanup(ctx context.Context) error
}
```

---

## 11. Implementation Checklist

### Phase 1: User Token Service
- [ ] Implement `UserTokenService` for Hub-issued JWTs
- [ ] Add `/api/v1/auth/login` endpoint (OAuth token exchange)
- [ ] Add `/api/v1/auth/token` endpoint (code exchange)
- [ ] Add `/api/v1/auth/refresh` endpoint
- [ ] Add `/api/v1/auth/validate` endpoint
- [ ] Update Koa proxy to use Hub-issued tokens

### Phase 2: Unified Middleware
- [ ] Implement unified `authMiddleware`
- [ ] Implement token type detection
- [ ] Implement `Identity` interface
- [ ] Add trusted proxy support
- [ ] Migrate from dev-only to unified middleware

### Phase 3: API Keys
- [ ] Implement `APIKeyService`
- [ ] Add API key CRUD endpoints
- [ ] Add API key validation in middleware
- [ ] Add API key management UI

### Phase 4: Security Hardening
- [ ] Implement token blacklist
- [ ] Add rate limiting on auth endpoints
- [ ] Add audit logging
- [ ] Add HTTPS enforcement
- [ ] Add security headers

---

## 12. Related Documents

- [Auth Overview](auth-overview.md) - Identity model and token types
- [Web Authentication](web-auth.md) - Browser OAuth flows
- [CLI Authentication](cli-auth.md) - Terminal authentication
- [Agent Authentication](sciontool-auth.md) - Agent-to-Hub JWT
- [Server Auth Setup](server-auth-setup.md) - API keys, dev auth
- [Permissions Design](permissions-design.md) - Authorization system
- [OAuth Setup](oauth-setup.md) - OAuth provider configuration
