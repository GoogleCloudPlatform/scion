# Local OAuth Setup Walkthrough

This guide explains how to set up and test real OAuth authentication (Google or GitHub) in your local development environment.

## 1. OAuth Provider Setup

First, you need to create credentials in your provider's developer console.

### Google OAuth Setup
1. Go to the [Google Cloud Console](https://console.cloud.google.com/).
2. Create or select a project.
3. Navigate to **APIs & Services > Credentials**.
4. Click **Create Credentials > OAuth client ID**.
5. Select **Web application** as the application type.
6. Add the following **Authorized redirect URIs**:
   - `http://localhost:8080/auth/callback/google`
7. Note your **Client ID** and **Client Secret**.

### GitHub OAuth Setup
1. Go to your GitHub **Settings > Developer settings > OAuth Apps**.
2. Click **New OAuth App**.
3. Set **Homepage URL** to `http://localhost:8080`.
4. Set **Authorization callback URL** to `http://localhost:8080/auth/callback/github`.
5. Register the application and note your **Client ID** and **Client Secret**.

## 2. Configuration

The Go server is configured via environment variables and CLI flags.

### Set Provider Credentials

Create or update your `~/.scion/hub.env` file with provider credentials:

```bash
GOOGLE_CLIENT_ID=your-client-id
GOOGLE_CLIENT_SECRET=your-client-secret
GITHUB_CLIENT_ID=your-client-id
GITHUB_CLIENT_SECRET=your-client-secret
SCION_SERVER_SESSION_SECRET=a-long-random-string
```

Or export them directly:

```bash
export SCION_SERVER_AUTH_GOOGLE_CLIENTID="your-client-id"
export SCION_SERVER_AUTH_GOOGLE_CLIENTSECRET="your-client-secret"
export SCION_SERVER_AUTH_GITHUB_CLIENTID="your-client-id"
export SCION_SERVER_AUTH_GITHUB_CLIENTSECRET="your-client-secret"
```

### Optional: Authorized Domains
To restrict login to specific email domains:

```bash
export SCION_SERVER_AUTH_AUTHORIZEDDOMAINS="example.com,mycompany.org"
```

## 3. Running the Server

Start the Go server with the web UI and hub enabled:

```bash
scion server start --enable-web --enable-hub --web-port 8080 --session-secret "your-secret"
```

Or with environment variables from hub.env:

```bash
set -a; source ~/.scion/hub.env; set +a
scion server start --enable-web --enable-hub --web-port 8080
```

## 4. Testing the Flow

1. Open your browser to `http://localhost:8080`.
2. Since you're not authenticated, you should be redirected to the login page (`/login`).
3. Click the button for your chosen provider (e.g., "Sign in with Google").
4. Complete the OAuth flow in the provider's pop-up/redirect.
5. If successful, you will be redirected back to the Scion dashboard.

### Troubleshooting

- **Redirect URI Mismatch**: Ensure the redirect URI in your provider's console matches exactly: `http://localhost:8080/auth/callback/<provider>`.
- **Port Conflict**: If you change the web port, update your redirect URIs accordingly.
- **Session Secret**: For a consistent experience across restarts, always set `SCION_SERVER_SESSION_SECRET` (or `--session-secret`).
- **Authorized Domains**: If you set authorized domains and your login email doesn't match, you'll see an error message.
