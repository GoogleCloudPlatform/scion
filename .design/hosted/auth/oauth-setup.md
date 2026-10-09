# OAuth Setup Guide

This document provides step-by-step instructions for obtaining and configuring OAuth credentials for the Scion Web Frontend.

## Overview

The Scion Web Frontend supports OAuth authentication with the following providers:
- **Google OAuth 2.0** - Recommended for organizations using Google Workspace
- **GitHub OAuth** - Useful for developer-focused deployments

You need to configure at least one provider for production use. For local development, use the [dev-auth mode](./hosted/dev-auth.md) instead.

---

## Environment Variables

After completing the setup steps below, you'll need to configure these environment variables:

```bash
# Required for all OAuth flows
SESSION_SECRET=<random-32-character-string>
BASE_URL=http://localhost:8080  # Or your production URL

# Google OAuth (optional if using GitHub)
GOOGLE_CLIENT_ID=<your-google-client-id>
GOOGLE_CLIENT_SECRET=<your-google-client-secret>

# GitHub OAuth (optional if using Google)
GITHUB_CLIENT_ID=<your-github-client-id>
GITHUB_CLIENT_SECRET=<your-github-client-secret>

# Authorization (optional)
SCION_AUTHORIZED_DOMAINS=example.com,company.org  # Comma-separated list of allowed email domains
```

---

## Part 1: Google OAuth Setup

### Step 1: Access Google Cloud Console

1. Go to the [Google Cloud Console](https://console.cloud.google.com/)
2. Sign in with a Google account that has permission to create projects
3. If this is your first time, accept the Terms of Service

### Step 2: Create or Select a Project

1. Click the project dropdown at the top of the page (next to "Google Cloud")
2. Either:
   - **Select an existing project** if you have one for Scion
   - **Create a new project** by clicking "New Project":
     - Project name: `scion-web` (or your preferred name)
     - Organization: Select your organization if applicable
     - Location: Leave as default or select a folder
     - Click "Create"
3. Wait for the project to be created (you'll see a notification)
4. Make sure the new project is selected in the dropdown

### Step 3: Configure OAuth Consent Screen

Before creating credentials, you must configure the consent screen:

1. In the left sidebar, navigate to **APIs & Services** → **OAuth consent screen**
2. Select the user type:
   - **Internal**: Only users within your Google Workspace organization (recommended for internal tools)
   - **External**: Any Google user can authenticate (requires verification for production)
3. Click "Create"

4. Fill in the **OAuth consent screen** form:
   - **App name**: `Scion` (or your preferred name)
   - **User support email**: Select your email
   - **App logo**: Optional, upload a logo
   - **App domain**: Leave blank for development
   - **Authorized domains**: Add your production domain (e.g., `scion.example.com`)
   - **Developer contact information**: Enter your email address
5. Click "Save and Continue"

6. **Scopes** page:
   - Click "Add or Remove Scopes"
   - Select the following scopes:
     - `email` - View your email address
     - `profile` - View your basic profile info
     - `openid` - Associate you with your personal info
   - Click "Update"
   - Click "Save and Continue"

7. **Test users** page (for External apps only):
   - Add email addresses of users who can test before verification
   - Click "Save and Continue"

8. Review the summary and click "Back to Dashboard"

### Step 4: Create OAuth Credentials

1. In the left sidebar, navigate to **APIs & Services** → **Credentials**
2. Click "+ Create Credentials" at the top
3. Select "OAuth client ID"

4. Configure the OAuth client:
   - **Application type**: Web application
   - **Name**: `Scion Web Frontend` (or your preferred name)

5. Add **Authorized JavaScript origins**:
   ```
   http://localhost:8080
   https://your-production-domain.com
   ```

6. Add **Authorized redirect URIs**:
   ```
   http://localhost:8080/auth/callback/google
   https://your-production-domain.com/auth/callback/google
   ```

   > **Important**: The redirect URI must match exactly. Include both HTTP (for local dev) and HTTPS (for production).

7. Click "Create"

8. A dialog will show your credentials:
   - **Client ID**: Copy this value → `GOOGLE_CLIENT_ID`
   - **Client Secret**: Copy this value → `GOOGLE_CLIENT_SECRET`

   > **Warning**: The client secret is shown only once. Download the JSON or copy it immediately.

### Step 5: Save Your Google Credentials

Store your credentials securely:

```bash
# For local development, create a .env file (add to .gitignore!)
echo "GOOGLE_CLIENT_ID=your-client-id-here.apps.googleusercontent.com" >> web/.env
echo "GOOGLE_CLIENT_SECRET=your-client-secret-here" >> web/.env
```

---

## Part 2: GitHub OAuth Setup

### Step 1: Access GitHub Developer Settings

1. Go to [GitHub](https://github.com/) and sign in
2. Click your profile picture in the top-right corner
3. Click "Settings"
4. Scroll down and click "Developer settings" in the left sidebar
5. Click "OAuth Apps"

### Step 2: Create a New OAuth App

1. Click "New OAuth App" (or "Register a new application")

2. Fill in the application form:
   - **Application name**: `Scion` (or your preferred name)
   - **Homepage URL**: `http://localhost:8080` (or your production URL)
   - **Application description**: Optional description
   - **Authorization callback URL**: `http://localhost:8080/auth/callback/github`

   > **Note**: You can only specify one callback URL per OAuth app. For multiple environments (dev, staging, prod), create separate OAuth apps.

3. Click "Register application"

### Step 3: Get Your Credentials

1. After registration, you'll see your app's settings page
2. Copy the **Client ID** → `GITHUB_CLIENT_ID`
3. Click "Generate a new client secret"
4. Copy the generated secret → `GITHUB_CLIENT_SECRET`

   > **Warning**: The client secret is shown only once. Copy it immediately.

### Step 4: Configure for Multiple Environments

For production, you'll need a separate OAuth app:

1. Return to "OAuth Apps" and click "New OAuth App"
2. Create another app with:
   - **Application name**: `Scion (Production)`
   - **Homepage URL**: `https://your-production-domain.com`
   - **Authorization callback URL**: `https://your-production-domain.com/auth/callback/github`

### Step 5: Save Your GitHub Credentials

```bash
# For local development
echo "GITHUB_CLIENT_ID=your-github-client-id" >> web/.env
echo "GITHUB_CLIENT_SECRET=your-github-client-secret" >> web/.env
```

---

## Part 3: Session Secret Generation

The session secret is used to sign session cookies. It must be:
- At least 32 characters long
- Cryptographically random
- Kept secret and never committed to version control

### Generate a Secure Session Secret

**Option 1: Using OpenSSL (recommended)**
```bash
openssl rand -base64 32
# Example output: K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv72ol/pe/Unols=
```

**Option 2: Using Node.js**
```bash
node -e "console.log(require('crypto').randomBytes(32).toString('base64'))"
```

**Option 3: Using Python**
```bash
python3 -c "import secrets; print(secrets.token_urlsafe(32))"
```

Save the generated secret:
```bash
echo "SESSION_SECRET=your-generated-secret-here" >> web/.env
```

---

## Part 4: Authorization Configuration

The web frontend supports basic domain-based authorization. Users can only log in if their email domain matches one of the authorized domains.

### Configure Authorized Domains

Set the `SCION_AUTHORIZED_DOMAINS` environment variable with a comma-separated list:

```bash
# Allow users from these email domains
SCION_AUTHORIZED_DOMAINS=example.com,mycompany.org

# For development, allow all domains (not recommended for production)
SCION_AUTHORIZED_DOMAINS=*
```

### How It Works

1. User authenticates via Google or GitHub
2. The frontend extracts the email domain from the user's email
3. If the domain matches any in `SCION_AUTHORIZED_DOMAINS`, access is granted
4. Otherwise, the user sees an "Unauthorized" error page

---

## Part 5: Complete Configuration

### Local Development (.env file)

Create `web/.env` with all required variables:

```bash
# Server configuration
PORT=8080
NODE_ENV=development

# Hub API (if running locally)
HUB_API_URL=http://localhost:9810

# Session
SESSION_SECRET=your-32-character-or-longer-secret-here

# OAuth - Base URL for callbacks
BASE_URL=http://localhost:8080

# Google OAuth
GOOGLE_CLIENT_ID=your-google-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=your-google-client-secret

# GitHub OAuth
GITHUB_CLIENT_ID=your-github-client-id
GITHUB_CLIENT_SECRET=your-github-client-secret

# Authorization
SCION_AUTHORIZED_DOMAINS=example.com,mycompany.org
```

> **Important**: Add `web/.env` to your `.gitignore` file!

### Production (Cloud Run / Kubernetes)

For production deployments, use secret management:

**Cloud Run with Secret Manager:**
```yaml
# cloudrun.yaml
env:
  - name: SESSION_SECRET
    valueFrom:
      secretKeyRef:
        name: scion-secrets
        key: session-secret
  - name: GOOGLE_CLIENT_ID
    valueFrom:
      secretKeyRef:
        name: scion-secrets
        key: google-client-id
  - name: GOOGLE_CLIENT_SECRET
    valueFrom:
      secretKeyRef:
        name: scion-secrets
        key: google-client-secret
```

**Create secrets in Google Cloud:**
```bash
# Create secrets
echo -n "your-session-secret" | gcloud secrets create session-secret --data-file=-
echo -n "your-google-client-id" | gcloud secrets create google-client-id --data-file=-
echo -n "your-google-client-secret" | gcloud secrets create google-client-secret --data-file=-
```

---

## Troubleshooting

### Google OAuth Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `redirect_uri_mismatch` | Callback URL doesn't match | Verify the redirect URI in Google Console exactly matches your `BASE_URL/auth/callback/google` |
| `invalid_client` | Wrong client ID/secret | Double-check credentials, ensure no extra whitespace |
| `access_denied` | User denied consent | User clicked "Cancel" on consent screen |
| `org_internal` | External user trying internal app | Change consent screen to "External" or add user as test user |

### GitHub OAuth Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `bad_verification_code` | Code expired or already used | OAuth codes expire quickly; retry the flow |
| `incorrect_client_credentials` | Wrong client ID/secret | Verify credentials match the OAuth app |
| `redirect_uri_mismatch` | Callback URL doesn't match | Update the callback URL in GitHub OAuth app settings |

### Session Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `Invalid session` | Session cookie tampered or expired | Clear cookies and retry login |
| `Session not found` | Server restarted (in-memory store) | Use Redis for persistent sessions in production |

---

## Security Best Practices

1. **Never commit secrets**: Add `.env` to `.gitignore`
2. **Use HTTPS in production**: Many OAuth providers require HTTPS redirect URIs for production clients. This is the provider's rule, not the Hub's: the Hub works over plain HTTP, and it marks the session cookie `Secure` only when its base URL starts with `https://`
3. **Rotate secrets regularly**: Generate new client secrets periodically
4. **Limit authorized domains**: Only allow domains you trust
5. **Use short session expiry**: 24 hours is a good default
6. **Enable CSRF protection**: Included in the auth middleware
7. **Review OAuth app permissions**: Only request necessary scopes

---

## Part 6: Authentication Mode Toggle

The web frontend supports two authentication modes:

| Mode | Use Case | How It Works |
|------|----------|--------------|
| **Dev Auth** | Local development | Auto-login using a shared dev token from `~/.scion/dev-token` |
| **OAuth** | Production / Staging | Users authenticate via Google or GitHub |

### Controlling Authentication Mode

The authentication mode is controlled by the `SCION_DEV_AUTH_ENABLED` environment variable:

```bash
# Explicitly enable dev-auth (useful for staging/testing)
SCION_DEV_AUTH_ENABLED=true

# Explicitly disable dev-auth (forces OAuth even in development)
SCION_DEV_AUTH_ENABLED=false
```

**Default behavior:**
- `NODE_ENV=development` (or unset): Dev-auth **enabled** by default
- `NODE_ENV=production`: Dev-auth **disabled** by default (OAuth required)

### How It Works

1. On server startup, the web frontend checks for dev-auth eligibility
2. If dev-auth is enabled AND a valid token exists in `~/.scion/dev-token`:
   - Users are automatically logged in as "Development User"
   - No OAuth flow is triggered
   - The dev token is forwarded to the Hub API for authorization
3. If dev-auth is disabled OR no token exists:
   - Users must authenticate via OAuth (Google or GitHub)
   - Session-based authentication is used

### Token Resolution Order

The dev token is resolved in this order:
1. `SCION_DEV_TOKEN` environment variable
2. File at `SCION_DEV_TOKEN_FILE` path (if set)
3. Default file: `~/.scion/dev-token`

---

## Appendix A: Sample Configuration Files

### ~/.scion/settings.yaml

This file configures the Scion CLI and can include web frontend settings:

```yaml
# ~/.scion/settings.yaml
# Scion configuration file

# Hub API configuration
hub:
  # URL where the Hub API is running
  url: http://localhost:9810

# Web Frontend configuration
web:
  # Port for the web server
  port: 8080

  # Base URL for OAuth callbacks (must match OAuth app configuration)
  base_url: http://localhost:8080

  # Authentication settings
  auth:
    # Google OAuth credentials
    google:
      client_id: "123456789-abcdefghijklmnop.apps.googleusercontent.com"
      client_secret: "GOCSPX-xxxxxxxxxxxxxxxxxxxxxxxx"

    # GitHub OAuth credentials
    github:
      client_id: "Iv1.abcdef1234567890"
      client_secret: "0123456789abcdef0123456789abcdef01234567"

    # Authorized email domains (empty = allow all)
    authorized_domains:
      - example.com
      - mycompany.org

  # Session configuration
  session:
    # Secret for signing session cookies (generate with: openssl rand -base64 32)
    secret: "K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv72ol/pe/Unols="
    # Session max age in hours
    max_age_hours: 24

# Development settings
dev:
  # Enable dev-auth mode (overrides NODE_ENV default)
  auth_enabled: true
  # Explicit dev token (optional, normally read from ~/.scion/dev-token)
  # token: "scion_dev_abc123..."
```

> **Note**: The web frontend currently reads configuration from environment variables. This settings.yaml format is for documentation and future CLI integration. Convert to environment variables for current use.

### Environment Variable Mapping

| settings.yaml Path | Environment Variable |
|--------------------|---------------------|
| `web.port` | `PORT` |
| `web.base_url` | `BASE_URL` |
| `web.auth.google.client_id` | `GOOGLE_CLIENT_ID` |
| `web.auth.google.client_secret` | `GOOGLE_CLIENT_SECRET` |
| `web.auth.github.client_id` | `GITHUB_CLIENT_ID` |
| `web.auth.github.client_secret` | `GITHUB_CLIENT_SECRET` |
| `web.auth.authorized_domains` | `SCION_AUTHORIZED_DOMAINS` (comma-separated) |
| `web.session.secret` | `SESSION_SECRET` |
| `web.session.max_age_hours` | `SESSION_MAX_AGE` (in milliseconds) |
| `dev.auth_enabled` | `SCION_DEV_AUTH_ENABLED` |
| `dev.token` | `SCION_DEV_TOKEN` |
| `hub.url` | `HUB_API_URL` |

---

## References

- [Google OAuth 2.0 Documentation](https://developers.google.com/identity/protocols/oauth2)
- [GitHub OAuth Documentation](https://docs.github.com/en/developers/apps/building-oauth-apps)
- [koa-session Documentation](https://github.com/koajs/session)
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
