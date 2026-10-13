# Server Authentication Setup

This document covers API key authentication, development authentication, Hub API auth endpoints, and security considerations.

---

## 1. API Key Authentication

For programmatic access and CI/CD pipelines, users can create API keys.

### 1.1 API Key Format

```
sk_live_<base64-encoded-payload>
```

Payload structure:
```json
{
  "kid": "key-uuid",
  "uid": "user-uuid",
  "created": "2025-01-01T00:00:00Z"
}
```

### 1.2 API Key Management

```
POST /api/v1/auth/api-keys
  Request:  { name, expiresAt?, scopes? }
  Response: { key, keyId, name, createdAt }

GET /api/v1/auth/api-keys
  Response: { keys: [{ keyId, name, lastUsed, createdAt }] }

DELETE /api/v1/auth/api-keys/{keyId}
  Response: { success: true }
```

### 1.3 API Key Usage

API keys are passed via the `Authorization` header:

```
Authorization: Bearer sk_live_...
```

Or via `X-API-Key` header:

```
X-API-Key: sk_live_...
```

---

## 2. Development Authentication (Interim)

> **Status:** Interim solution for development and local testing until full OAuth is implemented.

Development authentication provides a simple, zero-configuration mechanism for local development and testing. It bridges the gap until full OAuth-based authentication is implemented.

### 2.1 Goals

1. **Zero-config local development** - Start the server and immediately use the CLI
2. **Persistent tokens** - Tokens survive server restarts
3. **Environment variable override** - Easy integration with CI/testing
4. **Clear security boundary** - Obvious when running in dev mode
5. **Builds on existing auth** - Uses the standard `Bearer` authentication mechanism

### 2.2 Token Format

```
scion_dev_<32-character-hex-string>
```

Example: `scion_dev_a1b2c3d4e5f6789012345678901234567890abcd`

The `scion_dev_` prefix makes tokens easily identifiable and grep-able in logs.

### 2.3 Server Configuration

```yaml
server:
  auth:
    # Enable development authentication mode
    # WARNING: Not for production use
    devMode: false  # Default: disabled

    # Explicit token (optional)
    # If empty and devMode=true, auto-generate and persist
    devToken: ""

    # Path to token file (optional)
    # Default: ~/.scion/dev-token
    devTokenFile: ""
```

**Environment Variable Mapping:**

| Variable | Maps To |
|----------|---------|
| `SCION_SERVER_AUTH_DEV_MODE` | `server.auth.devMode` |
| `SCION_SERVER_AUTH_DEV_TOKEN` | `server.auth.devToken` |
| `SCION_SERVER_AUTH_DEV_TOKEN_FILE` | `server.auth.devTokenFile` |

### 2.4 Token Resolution Flow

When the server starts with development authentication enabled:

1. Check if a token is explicitly configured (`server.auth.devToken`)
2. If not, check for an existing token file at `~/.scion/dev-token`
3. If no file exists, generate a new cryptographically secure token
4. Store the token in `~/.scion/dev-token` with `0600` permissions
5. Log the command that loads the token from its file. Startup output shows the dev token file path instead of its value.

**Startup Log Output:**
```
To authenticate CLI commands, run:
  export SCION_DEV_TOKEN="$(cat '/home/user/.scion/dev-token')"
```

The path is the configured `server.auth.dev_token_file`, or `~/.scion/dev-token` by default. If the token is set directly with `server.auth.dev_token`, the output names that setting instead.

### 2.5 Client Token Resolution

The client checks for development tokens in the following order:

1. **Explicit option** - `hubclient.WithBearerToken(token)` or `hubclient.WithDevToken(token)`
2. **Environment variable** - `SCION_DEV_TOKEN`
3. **Token file** - `~/.scion/dev-token`

**Client Environment Variables:**

| Variable | Purpose |
|----------|---------|
| `SCION_DEV_TOKEN` | Development token value |
| `SCION_DEV_TOKEN_FILE` | Path to token file (default: `~/.scion/dev-token`) |

### 2.6 Wire Protocol

Development tokens use the standard Bearer authentication scheme:

```http
GET /api/v1/agents HTTP/1.1
Host: localhost:9810
Authorization: Bearer scion_dev_a1b2c3d4e5f6789012345678901234567890abcd
```

This is identical to production Bearer token authentication, ensuring no code path differences between dev and production auth flows.

### 2.7 Implementation

#### Server-Side Token Management

```go
package auth

import (
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "os"
    "path/filepath"
    "strings"
)

const (
    devTokenPrefix = "scion_dev_"
    devTokenLength = 32 // bytes, results in 64 hex chars
)

// DevAuthConfig holds development authentication settings.
type DevAuthConfig struct {
    Enabled   bool   `koanf:"devMode"`
    Token     string `koanf:"devToken"`
    TokenFile string `koanf:"devTokenFile"`
}

// InitDevAuth initializes development authentication.
// Returns the token to use and any error encountered.
func InitDevAuth(cfg DevAuthConfig, scionDir string) (string, error) {
    if !cfg.Enabled {
        return "", nil
    }

    // Priority 1: Explicit token in config
    if cfg.Token != "" {
        return cfg.Token, nil
    }

    // Determine token file path
    tokenFile := cfg.TokenFile
    if tokenFile == "" {
        tokenFile = filepath.Join(scionDir, "dev-token")
    }

    // Priority 2: Existing token file
    if data, err := os.ReadFile(tokenFile); err == nil {
        token := strings.TrimSpace(string(data))
        if token != "" {
            return token, nil
        }
    }

    // Priority 3: Generate new token
    token, err := generateDevToken()
    if err != nil {
        return "", fmt.Errorf("failed to generate dev token: %w", err)
    }

    // Persist token
    if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
        return "", fmt.Errorf("failed to write dev token file: %w", err)
    }

    return token, nil
}

// generateDevToken creates a new cryptographically secure development token.
func generateDevToken() (string, error) {
    bytes := make([]byte, devTokenLength)
    if _, err := rand.Read(bytes); err != nil {
        return "", err
    }
    return devTokenPrefix + hex.EncodeToString(bytes), nil
}

// IsDevToken returns true if the token appears to be a development token.
func IsDevToken(token string) bool {
    return strings.HasPrefix(token, devTokenPrefix)
}
```

#### Client-Side Token Resolution

```go
package hubclient

import (
    "os"
    "path/filepath"
    "strings"

    "github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// WithDevToken sets a development token for authentication.
func WithDevToken(token string) Option {
    return func(c *client) {
        c.auth = &apiclient.BearerAuth{Token: token}
    }
}

// WithAutoDevAuth attempts to load a development token automatically.
// Checks SCION_DEV_TOKEN env var, then ~/.scion/dev-token file.
func WithAutoDevAuth() Option {
    return func(c *client) {
        token := resolveDevToken()
        if token != "" {
            c.auth = &apiclient.BearerAuth{Token: token}
        }
    }
}

// resolveDevToken finds a development token from environment or file.
func resolveDevToken() string {
    // Priority 1: Environment variable
    if token := os.Getenv("SCION_DEV_TOKEN"); token != "" {
        return token
    }

    // Priority 2: Custom token file from env
    if tokenFile := os.Getenv("SCION_DEV_TOKEN_FILE"); tokenFile != "" {
        if data, err := os.ReadFile(tokenFile); err == nil {
            return strings.TrimSpace(string(data))
        }
    }

    // Priority 3: Default token file
    home, err := os.UserHomeDir()
    if err != nil {
        return ""
    }

    tokenFile := filepath.Join(home, ".scion", "dev-token")
    if data, err := os.ReadFile(tokenFile); err == nil {
        return strings.TrimSpace(string(data))
    }

    return ""
}
```

### 2.8 Usage Examples

#### Starting the Server

```bash
# Start Hub with dev auth (token auto-generated)
scion server start --enable-hub --dev-auth

# Or via config
cat > ~/.scion/server.yaml << EOF
server:
  hub:
    enabled: true
  auth:
    devMode: true
EOF

scion server start --config ~/.scion/server.yaml
```

#### Using the CLI

```bash
# Option 1: Set environment variable (explicit)
export SCION_DEV_TOKEN=scion_dev_a1b2c3d4e5f6789012345678901234567890abcd
scion agent list --hub http://localhost:9810

# Option 2: Automatic (reads from ~/.scion/dev-token)
scion agent list --hub http://localhost:9810

# Option 3: One-liner
SCION_DEV_TOKEN=$(cat ~/.scion/dev-token) scion agent list --hub http://localhost:9810
```

#### CI/Testing Integration

```yaml
# GitHub Actions example
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Start Scion Hub
        run: |
          scion server start --enable-hub --dev-auth --background
          echo "SCION_DEV_TOKEN=$(cat ~/.scion/dev-token)" >> $GITHUB_ENV

      - name: Run integration tests
        run: go test ./integration/...
        env:
          SCION_HUB_URL: http://localhost:9810
          # SCION_DEV_TOKEN already set above
```

### 2.9 Security Constraints

**The server MUST:**

1. Log a clear warning when dev auth is enabled
2. Refuse to start with dev auth if binding to non-localhost AND TLS is disabled
3. Include "dev-mode" in health check responses

```go
func validateDevAuthConfig(cfg *ServerConfig) error {
    if !cfg.Auth.DevMode {
        return nil
    }

    // Warn about dev mode
    log.Warn("Development authentication enabled - not for production use")

    // Block dangerous configurations
    if !cfg.TLS.Enabled && !isLocalhost(cfg.Host) {
        return fmt.Errorf("dev auth requires TLS when binding to non-localhost address")
    }

    return nil
}
```

**Token File Permissions:**
- Token file MUST be created with `0600` permissions (owner read/write only)
- Client SHOULD warn if token file has overly permissive permissions

**Token Entropy:**
- Tokens use 32 bytes (256 bits) of cryptographic randomness
- This provides sufficient entropy to prevent brute-force attacks

**No Token in URLs:**
- Tokens MUST NOT be passed in URL query parameters
- This prevents token leakage in server logs, browser history, and referrer headers

### 2.10 Migration to Production Auth

When OAuth authentication is fully implemented:

1. Dev auth remains available but disabled by default
2. Production deployments set `devMode: false` explicitly
3. The `WithAutoDevAuth()` client option becomes a no-op when `SCION_DEV_TOKEN` is unset and no token file exists
4. Dev tokens are rejected by production servers (check for `scion_dev_` prefix)

---

## 3. Domain Authorization

Scion supports restricting authentication to specific email domains. This provides an additional layer of access control beyond OAuth authentication.

### 3.1 Configuration

Set the `SCION_AUTHORIZED_DOMAINS` environment variable with a comma-separated list of allowed domains:

```bash
# Allow users from specific email domains
export SCION_AUTHORIZED_DOMAINS="example.com,mycompany.org"

# Leave empty to allow all domains (default)
export SCION_AUTHORIZED_DOMAINS=""
```

Alternatively, configure via `server.yaml`:

```yaml
auth:
  authorizedDomains:
    - example.com
    - mycompany.org
```

**Environment Variable Mapping:**

| Variable | Maps To |
|----------|---------|
| `SCION_AUTHORIZED_DOMAINS` | `auth.authorizedDomains` |

### 3.2 How It Works

Domain authorization is enforced at multiple layers for defense in depth:

1. **Web Frontend**: After OAuth callback, the frontend checks the user's email domain before creating a session
2. **Hub API**: The `/api/v1/auth/login` and `/api/v1/auth/cli/token` endpoints verify the email domain before issuing tokens

If a user's email domain is not in the authorized list, they receive a "your email domain is not authorized" error.

### 3.3 Behavior

- **Empty list**: All email domains are allowed (default)
- **Non-empty list**: Only emails from listed domains can authenticate
- **Case insensitive**: Domain matching is case-insensitive (`Example.COM` matches `example.com`)
- **Exact match**: Subdomains are not automatically included (`sub.example.com` does NOT match `example.com`)

### 3.4 Security Considerations

- Domain authorization is checked after OAuth authentication succeeds, so the OAuth provider validates the user's identity first
- Both web and API layers enforce the check, providing defense in depth
- Authorized domains should be kept to a minimum to reduce attack surface

---

## 4. Hub API Auth Endpoints

### 3.1 OAuth Initiation (for CLI)

```
GET /api/v1/auth/authorize
  Query: redirect_uri, state
  Response: { authUrl, state }
```

### 3.2 Token Exchange

```
POST /api/v1/auth/token
  Request:  { code, redirectUri, grantType: "authorization_code" }
  Response: { accessToken, refreshToken, expiresIn, user }

POST /api/v1/auth/token
  Request:  { refreshToken, grantType: "refresh_token" }
  Response: { accessToken, refreshToken, expiresIn }
```

### 3.3 Token Validation

```
POST /api/v1/auth/validate
  Request:  { token }
  Response: { valid: true, user, expiresAt }
```

---

## 4. Security Considerations

### 4.1 Token Security

| Aspect | Web | CLI | API Key |
|--------|-----|-----|---------|
| Storage | HTTP-only cookie | Local file (0600) | Local file or env var |
| Transmission | HTTPS only | HTTPS only | HTTPS only |
| Lifetime | 24 hours (session) | 30 days (renewable) | Configurable |
| Revocation | Logout endpoint | Logout command | Dashboard |

### 4.2 PKCE for CLI

CLI authentication uses PKCE (Proof Key for Code Exchange) for additional security:

```go
type PKCEChallenge struct {
    Verifier  string  // Random 43-128 character string
    Challenge string  // SHA256(verifier), base64url encoded
    Method    string  // "S256"
}

func GeneratePKCE() *PKCEChallenge {
    verifier := generateRandomString(64)
    hash := sha256.Sum256([]byte(verifier))
    challenge := base64.RawURLEncoding.EncodeToString(hash[:])

    return &PKCEChallenge{
        Verifier:  verifier,
        Challenge: challenge,
        Method:    "S256",
    }
}
```

### 4.3 Rate Limiting

Authentication endpoints are rate-limited to prevent brute force attacks:

| Endpoint | Limit | Window |
|----------|-------|--------|
| `/auth/login` | 10 | 1 minute |
| `/auth/token` | 20 | 1 minute |
| `/auth/authorize` | 10 | 1 minute |

### 4.4 Audit Logging

All authentication events are logged:

```go
type AuthEvent struct {
    EventType   string    `json:"eventType"`   // login, logout, token_refresh, api_key_created
    UserID      string    `json:"userId"`
    ClientType  string    `json:"clientType"`  // web, cli, api
    IPAddress   string    `json:"ipAddress"`
    UserAgent   string    `json:"userAgent"`
    Success     bool      `json:"success"`
    FailReason  string    `json:"failReason,omitempty"`
    Timestamp   time.Time `json:"timestamp"`
}
```

---

## Related Documents

- [Auth Overview](auth-overview.md) - Identity model and token types
- [CLI Authentication](cli-auth.md) - Terminal-based authentication
- [Server Authentication](server-auth-design.md) - Hub server-side auth handling
- [Runtime Broker Auth](runtime-broker-auth.md) - Broker registration and mutual TLS
- [Implementation Milestones](auth-milestones.md) - Phased implementation plan
