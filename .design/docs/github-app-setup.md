# GitHub App Setup Guide

This guide walks through configuring GitHub App authentication for your Scion Hub.

## Prerequisites

- A running Scion Hub instance
- Admin access to the Hub
- Admin access to a GitHub organization or user account

## Step 1: Register a GitHub App

1. Go to **GitHub Settings > Developer Settings > GitHub Apps > New GitHub App**.
2. Fill in:
   - **App name**: e.g., `Scion Hub - <your org>`
   - **Homepage URL**: Your Hub's public URL
   - **Setup URL**: `https://<hub-public-url>/github-app/setup`
     - Check "Redirect on update"
   - **Webhook URL**: `https://<hub-public-url>/api/v1/webhooks/github`
   - **Webhook secret**: Generate a strong secret (e.g., `openssl rand -hex 32`)
3. Set **Permissions**:
   - Repository permissions:
     - Contents: **Read and write**
     - Metadata: **Read-only**
     - Pull requests: **Read and write**
     - Issues: **Read and write** (required when GitHub triggers are used; optional for Git authentication alone)
     - Checks: **Read and write** (optional, for future CI status)
     - Actions: **Read-only** (optional, for workflow status)
4. Subscribe to events:
   - Installation
   - Installation repositories
   - When GitHub triggers (`hub.github_triggers`) are used, also subscribe to:
     - Pull requests (`pull_request`)
     - Issues (`issues`)
     - Issue comments (`issue_comment`)
     - Pull request reviews (`pull_request_review`)
     - Pull request review comments (`pull_request_review_comment`)
     - Push (`push`, optional — required only if push triggers are used)
5. Set "Where can this GitHub App be installed?" to **Any account** (or restrict to your org).
6. Click **Create GitHub App**.

## Step 2: Generate a Private Key

1. On the app's settings page, scroll to **Private keys**.
2. Click **Generate a private key**. A `.pem` file will be downloaded.
3. Store this file securely on the Hub server (e.g., `/etc/scion/github-app-key.pem`).
4. Set file permissions: `chmod 600 /etc/scion/github-app-key.pem`.

## Step 3: Configure the Hub

Add the GitHub App configuration to your Hub's `settings.yaml`:

```yaml
github_app:
  app_id: 123456          # From the GitHub App's General page
  private_key_path: /etc/scion/github-app-key.pem
  webhook_secret: "your-webhook-secret"
  webhooks_enabled: true   # Set to false if Hub is not publicly reachable
  # api_base_url: https://github.mycompany.com/api/v3  # For GitHub Enterprise Server
```

Alternatively, configure via the Hub admin API:

```bash
curl -X PUT https://<hub-url>/api/v1/github-app \
  -H "Authorization: Bearer <admin-token>" \
  -d '{
    "app_id": 123456,
    "private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
    "webhook_secret": "your-webhook-secret",
    "webhooks_enabled": true
  }'
```

Restart the Hub server to pick up the configuration.

## Step 4: Install the App on Your Organization

1. Navigate to your GitHub App's public installation page:
   `https://github.com/apps/<app-slug>/installations/new`
2. Select the organization or user account.
3. Choose repository access:
   - **All repositories** — app can access all repos in the org
   - **Only select repositories** — choose specific repos
4. Click **Install**.
5. GitHub will redirect to your Hub's setup URL. The Hub will:
   - Record the installation
   - Auto-match groves by git remote URL
   - Display a confirmation with matched groves

## Step 5: Verify

Check the installation was recorded:

```bash
curl https://<hub-url>/api/v1/github-app/installations \
  -H "Authorization: Bearer <admin-token>"
```

Check a grove's GitHub App status:

```bash
curl https://<hub-url>/api/v1/groves/<grove-id>/github-status \
  -H "Authorization: Bearer <admin-token>"
```

The status should show `"state": "unchecked"` until the first agent runs, after which it will show `"state": "ok"`.

## Upgrading an Existing App for Triggers

If your GitHub App was registered for Git authentication only, upgrade its permissions and event subscriptions before using GitHub triggers (`hub.github_triggers`):

1. **Ensure webhooks are enabled**:
   - On GitHub (**GitHub App settings > General**), confirm **Webhook** is active, **Webhook URL** points to `https://<hub-public-url>/api/v1/webhooks/github`, and **Webhook secret** matches the secret configured on the Hub.
   - In Hub configuration, ensure `webhooks_enabled: true` and the webhook secret is stored.
2. **Update permissions and subscribed events on GitHub**:
   - Go to **Permissions & events** in your GitHub App settings.
   - Under **Repository permissions**, set **Issues** to **Read and write** (keep **Contents: Read and write**, **Pull requests: Read and write**, and **Metadata: Read-only**). `Issues: Read and write` is required both so GitHub delivers `issue_comment` events (including pull request conversation comments) and so trigger actions and agents can post comments.
   - Under **Subscribe to events**, check **Pull requests**, **Issues**, **Issue comments**, **Pull request reviews**, and **Pull request review comments** (plus **Push** if push triggers are used), while keeping **Installation** and **Installation repositories** checked.
   - Save changes.
3. **Accept the updated permissions on each installation**:
   - Subscribing to events for already-granted permissions takes effect immediately, but adding or raising a permission causes GitHub to ask each installation's organization or account owner to accept the update. Until accepted, installation token mints requesting `issues: write` fail and GitHub does not deliver the newly added events for that installation.
   - Have each installation owner approve the prompt in **GitHub Settings > Applications** (or Organization **Settings > Third-party Access > GitHub Apps**). When accepted, GitHub delivers `installation` (`action: "new_permissions_accepted"`); while `hub.github_triggers` is enabled, the Hub refreshes its cached App metadata and resets affected projects' GitHub App status from `degraded` (`permission_denied`) back to `unchecked` so the next token mint re-validates cleanly.
4. **Verify trigger readiness on the Hub**:
   - Refresh cached App metadata immediately if needed:
     ```bash
     curl -X POST https://<hub-url>/api/v1/github-app/sync-permissions \
       -H "Authorization: Bearer <admin-token>"
     ```
   - With the `hub.github_triggers` experiment enabled, check readiness on `GET /api/v1/github-app`:
     ```bash
     curl https://<hub-url>/api/v1/github-app \
       -H "Authorization: Bearer <admin-token>"
     ```
     Confirm `"triggers"` reports `"ready": true` with empty `missing_events` and `missing_permissions` (a forthcoming `scion trigger github status` CLI command will also display this status).

## Webhook Troubleshooting

If webhooks are not being received:

1. **Check Hub reachability**: Ensure the webhook URL is accessible from GitHub's servers.
2. **Check firewall rules**: Allow inbound HTTPS from GitHub's webhook IP ranges.
3. **Check reverse proxy**: Ensure your reverse proxy forwards the `X-Hub-Signature-256` and `X-GitHub-Event` headers.
4. **Check webhook secret**: Ensure the secret matches between GitHub and Hub config.
5. **Check GitHub delivery log**: Go to your GitHub App settings > Advanced > Recent Deliveries to see delivery status and response codes.
6. **Fallback to polling**: If webhooks can't be enabled, set `webhooks_enabled: false` in Hub config. The Hub will use auto-discovery and periodic health checks instead.

## Without Webhooks

If your Hub is not publicly reachable (e.g., behind a corporate firewall):

1. Set `webhooks_enabled: false` in Hub config.
2. The Hub will:
   - Run periodic health checks every 6 hours to detect installation changes.
   - Use auto-discovery when groves are created.
   - Detect issues at token minting time (when agents start).
3. You can trigger manual discovery:
   ```bash
   curl -X POST https://<hub-url>/api/v1/github-app/installations/discover \
     -H "Authorization: Bearer <admin-token>"
   ```
