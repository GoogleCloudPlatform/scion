---
title: GitHub App Setup
description: Register a GitHub App, connect it to your Scion Hub, and give agents short-lived GitHub installation tokens.
---

**What you will learn**: How to register a GitHub App for your Hub, configure its credentials, install it on a GitHub organization or account, associate projects with the installation, and verify, troubleshoot and rotate the integration.

:::note[Applies to any hosted tier]
The GitHub App integration is a Hub feature. It works the same way on a single-node or HA Hub, and on a workstation server that GitHub can reach. For the security model behind it, see [GitHub App Integration](/scion/hosted/single-node/auth/#github-app-integration) in Auth & Tenancy.
:::

## Overview

With a GitHub App configured, the Hub mints **GitHub App installation tokens** for each project's repository instead of relying on long-lived personal access tokens:

- **Scoped, short-lived tokens**: each token is limited to the project's repository and to the project's token permissions, and expires after about an hour (GitHub's limit).
- **Automatic refresh**: on agent create, start and restart the Hub injects a fresh token as `GITHUB_TOKEN`. Inside the container `sciontool` refreshes it about 10 minutes before it expires, and its git credential helper (`sciontool credential-helper`) supplies a current token to `git` on demand.
- **Installation tracking**: the Hub records each installation of the App and keeps its status (`active`, `suspended`, `deleted`) current from webhooks, discovery and a periodic health check.
- **GitHub triggers (optional)**: with webhooks and the `hub.triggers` and `hub.github_triggers` [experiments](/scion/reference/experiments/#server-experiments-with-notes) enabled, GitHub events can start agent work. This guide covers the App settings triggers need; the trigger features themselves are described in [Auth & Tenancy](/scion/hosted/single-node/auth/#permissions-and-events-for-github-triggers).

If a project has no usable installation, agents fall back to a `GITHUB_TOKEN` secret, as described in [Projects (Hub-managed vs Linked)](/scion/workstation/git-projects/#option-b-personal-access-tokens).

### Prerequisites

- A running Hub (see [Hub Setup](/scion/hosted/single-node/hub-server/)) and a Hub admin account. Every `/api/v1/github-app` route, and binding an installation to a project, requires the `hub.github_app.read` or `hub.github_app.update` permission, which Hub admins hold.
- Permission to create a GitHub App for, and install it on, the GitHub organization or personal account that owns your repositories.
- A public HTTPS URL for the Hub (`server.hub.public_url`) if you want webhooks. GitHub must be able to reach `https://<hub-public-url>/api/v1/webhooks/github`. The setup redirect after installation only needs to be reachable from the installing user's browser.

## Step 1: Register the GitHub App on GitHub

1. On GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App**. To create the App for an organization, start from the organization's settings instead.
2. Fill in the general settings:

   | GitHub field | Value |
   | :--- | :--- |
   | **GitHub App name** | Any unique name, for example `acme-scion`. Its slug forms the installation URL. |
   | **Homepage URL** | Your Hub's public URL. GitHub requires a value; the Hub does not use it. |
   | **Callback URL** / **Request user authorization (OAuth) during installation** | Leave empty and unchecked. The Hub does not use the App's user authorization flow. |
   | **Setup URL** (under *Post installation*) | `https://<hub-public-url>/github-app/setup`. Check **Redirect on update** so the Hub also hears about repository selection changes made from GitHub. |
   | **Webhook → Active** | Checked if the Hub is reachable from GitHub; otherwise uncheck it (see [Running without webhooks](#running-without-webhooks)). |
   | **Webhook URL** | `https://<hub-public-url>/api/v1/webhooks/github` |
   | **Webhook secret** | A long random value, for example from `openssl rand -hex 32`. Keep it for Step 2. |

3. Under **Permissions**, set the repository permissions. The Hub requests these by default for every installation token (`contents`, `pull_requests` and `issues` at write, `metadata` at read), so grant at least:

   | Repository permission | Access |
   | :--- | :--- |
   | **Contents** | Read and write |
   | **Pull requests** | Read and write |
   | **Issues** | Read and write |
   | **Metadata** | Read-only (selected automatically) |

   Add **Checks** or **Actions** only if you also give a project those token permissions (see [Project token permissions](#project-token-permissions)).

   :::caution[A token request may not exceed the App's permissions]
   If the App lacks a permission that the Hub requests, GitHub refuses the token and the project shows `permission_denied`. To run an App without **Issues** write, narrow each project's token permissions to what the App grants.
   :::

4. Under **Subscribe to events**, select the events you need. The Hub handles the installation lifecycle events `installation` and `installation_repositories` whenever webhooks are active; GitHub sends them to every App with an active webhook. For GitHub triggers, also subscribe to **Issues**, **Issue comment**, **Pull request**, **Pull request review** and **Pull request review comment**, plus **Push** if you want push triggers. The Hub reports trigger readiness only when the first five are subscribed.
5. Under **Where can this GitHub App be installed?**, choose **Only on this account** to keep it to your organization or account, or **Any account** to let others install it.
6. Click **Create GitHub App** and note the **App ID** shown on the App's settings page.
7. Under **Private keys**, click **Generate a private key**. GitHub downloads a `.pem` file. The Hub accepts PKCS#1 (`BEGIN RSA PRIVATE KEY`) and PKCS#8 (`BEGIN PRIVATE KEY`) RSA keys.

:::danger[Protect the private key]
The private key lets anyone act as your App on every installation. Store it as a secret, never commit it, and delete local copies once the Hub has it.
:::

## Step 2: Configure the GitHub App on the Hub

Configure the App either in the web UI, which is the usual path, or in deployment configuration. The non-secret fields form the `github_app` section of the [Layer-1 operational settings](/scion/reference/admin-settings/): `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `private_key_path`, `app_slug` and `command_mention_aliases`.

### Option A: Web UI (recommended)

1. Sign in as a Hub admin and open **Server Config** in the admin navigation (`/admin/server-config`), then select the **GitHub App** tab.
2. Fill in the **GitHub App Configuration** section:
   - **App ID**: the numeric App ID from Step 1.
   - **API Base URL**: leave empty for `github.com`. For GitHub Enterprise Server, enter the API URL, for example `https://github.example.com/api/v3`; for GitHub Enterprise Cloud with data residency, `https://api.<subdomain>.ghe.com`. The Hub derives the git host that project remotes must use from this URL.
   - **Private Key (PEM)**: paste the full contents of the `.pem` file.
   - **Webhook Secret**: the webhook secret from Step 1.
   - **Webhooks**: turn on if you configured an active webhook.
   - **Public Installation URL**: `https://github.com/apps/<app-slug>/installations/new`. Project settings show this link to admins whose project has no installation yet.
3. Click **Save GitHub App Configuration**. The status next to the button changes to **Configured** once the App ID and a private key are present.

The save is applied to the running Hub without a restart, except for the first `app_id` (see [Restart once after the first configuration](#restart-once-after-the-first-configuration)). The Hub stores the private key and webhook secret as Hub-scoped secrets named `GITHUB_APP_PRIVATE_KEY` and `GITHUB_APP_WEBHOOK_SECRET`, and the other fields in the `github_app` section of the Hub database. The UI never shows the stored secrets again: leave a secret field empty to keep its current value.

The same save is available as `PUT /api/v1/github-app` with any of `app_id`, `private_key`, `webhook_secret`, `api_base_url`, `webhooks_enabled` and `installation_url`. Because it manages credentials, this route requires an interactive session and refuses user access tokens with `403`.

### Option B: Deployment configuration

Set the App in `settings.yaml` under `server.github_app`, or through environment variables:

```yaml
server:
  github_app:
    app_id: 123456
    private_key_path: /etc/scion/github-app.pem
    webhooks_enabled: true
    installation_url: https://github.com/apps/acme-scion/installations/new
```

| Setting | Environment variable |
| :--- | :--- |
| `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `private_key_path` (seed values for the Layer-1 section) | `SCION_SEED_SERVER_GITHUBAPP_APPID`, `SCION_SEED_SERVER_GITHUBAPP_APIBASEURL`, `SCION_SEED_SERVER_GITHUBAPP_WEBHOOKSENABLED`, `SCION_SEED_SERVER_GITHUBAPP_INSTALLATIONURL`, `SCION_SEED_SERVER_GITHUBAPP_PRIVATEKEYPATH` |
| `private_key` (PEM contents) | `SCION_SERVER_GITHUBAPP_PRIVATEKEY` |
| `webhook_secret` | `SCION_SERVER_GITHUBAPP_WEBHOOKSECRET` |

The non-secret values seed the `github_app` section until an admin saves it in the UI; after that the database value wins (see [Admin Settings Model](/scion/reference/admin-settings/)). `private_key` and `webhook_secret` are read only at startup and are never written to the database.

### Where the secrets live

- **Private key**: the Hub uses the first one it finds: a key held in memory (from `private_key` in `settings.yaml` or the environment, or from a UI save since the last restart), then the file at `private_key_path`, then the stored `GITHUB_APP_PRIVATE_KEY` secret.
- **Webhook secret**: a value from `settings.yaml` or the environment (or a UI save since the last restart) wins over the stored `GITHUB_APP_WEBHOOK_SECRET` secret.
- **Storage**: with an external secret backend (such as GCP Secret Manager, `server.secrets.backend`), the two secrets are stored there. Otherwise the default `local` backend stores them in the Hub database, encrypted with the Hub's value cipher. The cipher is keyed from the keyring's `encryption` ring when [`server.keyring_file`](/scion/reference/server-config/#environment-variables) is set, and otherwise from the legacy key derived from the session secret. The secrets are stored as plaintext only when the Hub has neither a keyring file nor a session secret.

:::caution[Choose one source for each secret]
Because a startup value wins, a key set by `private_key_path` or `SCION_SERVER_GITHUBAPP_PRIVATEKEY` replaces a key uploaded in the UI at the next restart. Configure each secret in one place only, and rotate it in that same place.
:::

### Restart once after the first configuration

The agent dispatcher attaches the App's token minter at startup, and only when an App ID is set then. After setting `app_id` for the first time, restart the Hub so agents receive App tokens when they start. A save that changes `app_id` is reported as pending a restart for this reason. Discovery, webhooks and the project status check work without the restart.

The periodic installation health check needs no restart. It runs every 24 hours when `webhooks_enabled` is on and every 6 hours when it is off. Saving `app_id` or `webhooks_enabled`, in the UI or the API, reschedules it at once, and clearing `app_id` stops it.

## Step 3: Install the App and sync installations

1. Open the App's public page (`https://github.com/apps/<app-slug>`, the same as the **Public Installation URL**) and click **Install**, or use **Install App** in the App's settings.
2. Choose the organization or account and select **All repositories** or the repositories your agents will work on.
3. GitHub redirects the browser to the Setup URL. The Hub looks the installation up on GitHub, records it with its account and repository list, associates matching projects (see Step 4), and redirects to the **GitHub App Setup** page (`/github-app/installed`), which lists your GitHub projects and their status.

The Hub also learns about installations in these ways:

- **Webhooks**: `installation` events (`created`, `deleted`, `suspend`, `unsuspend`) and `installation_repositories` events (`added`, `removed`) update the stored installation.
- **Discovery**: **Discover from GitHub** in the **Installations** section of the **GitHub App** tab, or `POST /api/v1/github-app/installations/discover`, lists every installation of the App from GitHub, records or updates each one, and associates matching projects. Use it after installing without a Setup URL or webhook, or to catch up after missed deliveries.
- **Manual record**: `POST /api/v1/github-app/installations` with `installation_id` and `account_login` records an installation without contacting GitHub. It does not associate any projects: bind them as an admin or run discovery afterwards.

The **Installations** list shows each installation's account, type, repository count, ID and status. `GET /api/v1/github-app/installations` returns the same records, with optional `account_login` and `status` filters.

## Step 4: Associate projects and verify agent access

A project uses an installation only when the **association is authorized**. The Hub checks this when it records an association and again at every token mint. All of these must hold:

1. The project's git remote is on the App's GitHub host: `github.com`, or the host of `api_base_url`.
2. The installation is `active` and its account is the owner of the remote (for `https://github.com/acme/backend.git`, the installation on `acme`).
3. One of these proofs exists:
   - **Admin binding**: a Hub admin bound the installation to the project's current remote, and that admin is still an active Hub admin. Write permissions are allowed.
   - **Creator proof**: the project creator's own user-scope `GITHUB_TOKEN` secret can read the repository on GitHub. With `push` access write permissions are allowed; with only `pull` access every token is reduced to read-only.

If any step cannot be completed (a lookup error, a GitHub error, a timeout or a rate limit), the check fails closed and no token is minted. The result of the creator-token check is cached: access for 10 minutes, a definitive denial for 1 minute. Inconclusive failures are retried at the next mint.

### Automatic association

The Hub associates a project with an installation that lists the project's repository whenever the association is authorized: when the project is created, when an installation is recorded or gains repositories, and on discovery. It never replaces an existing association. For the creator proof to work, the user who creates the project stores a GitHub token with access to the repository first:

```bash
scion hub secret set GITHUB_TOKEN <github-token>
```

Secrets are user-scoped by default. See [Secrets](/scion/hosted/user/secrets/) for how user-scoped secrets reach agents.

### Admin binding

To associate a project without relying on the creator's token, bind the installation with a Hub admin's credentials:

```bash
curl -X PUT "https://<hub-public-url>/api/v1/projects/<project-id>/github-installation" \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"installation_id": 12345678}'
```

The Hub answers `422` if the remote is not on the configured GitHub host, the installation is not active, or the installation's account does not own the repository. The binding is stored on the project in the `scion.dev/github-installation-binding` annotation and is tied to the remote: if the project's remote changes, bind it again. `DELETE` on the same route removes the association and the binding; the **Remove** button in the project's **GitHub App Integration** settings does the same.

There is no web UI or CLI command for binding yet.

### Project token permissions

Each project can narrow what its tokens request with `GET`, `PUT` and `DELETE` on `/api/v1/projects/<project-id>/github-permissions`. The body takes `contents`, `pull_requests`, `issues`, `metadata`, `checks` and `actions`, each `"read"`, `"write"` or empty. A `PUT` must set at least one of them: a body with every field empty, or with any other field, is rejected with `400`. To go back to the defaults, send `DELETE`. Without stored permissions the Hub requests the defaults from Step 1, and `GET` returns them; a stored set with every field empty counts as no stored permissions. A read-only proof turns any `write` into `read`.

Commit authorship for agent commits is configured per project at `/api/v1/projects/<project-id>/git-identity` (mode `bot`, `custom` with `name` and `email`, or `co-authored`; default `bot`).

### Verify

1. Open the project's settings. For a project with a `github.com` remote, Hub admins see a **GitHub App Integration** section with the status, installation ID, last token mint and permission badges. Click **Check Status** (or **Recheck Status**), which runs `POST /api/v1/projects/<project-id>/github-status`: the Hub runs the association check, mints a token and records the result. A working project shows **Active** (`ok`). On GitHub Enterprise, or without the UI, call the same endpoint directly; `GET` on it returns the stored status.
2. Start an agent in the project and check its git access, for example with `git ls-remote origin` in the agent's terminal. `sciontool doctor` inside the container also checks the GitHub App token.

When an agent starts, the Hub injects `GITHUB_TOKEN`, `SCION_GITHUB_APP_ENABLED=true`, `SCION_GITHUB_TOKEN_EXPIRY` and `SCION_GITHUB_TOKEN_PATH` (`/tmp/.github-token`). If the agent already gets a `GITHUB_TOKEN` from secrets, that token stays in the environment for the `gh` CLI, and `git` still uses App tokens through the credential helper. If minting fails, the agent starts with whatever `GITHUB_TOKEN` secret is available.

## Step 5: Verification, troubleshooting and rotation

### Check the Hub's configuration

`GET /api/v1/github-app` returns `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `configured` (an App ID and a private key are present), `has_private_key`, `has_webhook_secret` and the last seen GitHub `rate_limit`. With `hub.triggers` and `hub.github_triggers` enabled it also returns `triggers`, whose `ready` is `true` only when webhooks are enabled, a webhook secret is set, the App's metadata could be read, and no required event or permission is missing (`missing_events`, `missing_permissions`). Secrets are never returned.

**Sync Permissions** on the **GitHub App** tab (`POST /api/v1/github-app/sync-permissions`) re-reads the App's permissions from GitHub and marks any project whose token permissions exceed them as `degraded` (`permission_denied`). The health check runs the same comparison.

To review every association on the Hub, use `GET /api/v1/github-app/installations/association-report` (add `?refresh=true` to rebuild it; rebuilds are limited to one per minute). Each entry shows whether the association is `authorized`, its `basis` (`admin_binding`, `creator_push` or `creator_pull`) and, when refused, an `error_code` and `reason`.

### Project status codes

A project's GitHub App status is `unchecked`, `ok`, `degraded` or `error`, with one of these error codes:

| Code | Meaning | Fix |
| :--- | :--- | :--- |
| `private_key_invalid` | GitHub rejected the App's JWT. | Upload a current private key for this App ID. |
| `app_not_found` | GitHub does not know the App ID. | Correct the **App ID** (and **API Base URL** for GitHub Enterprise). |
| `installation_revoked` | The installation no longer exists. | Reinstall the App on the account, then discover installations. |
| `installation_suspended` | The installation is suspended. | Have the account owner unsuspend it on GitHub. |
| `repo_not_accessible` | The installation does not cover the repository. | Add the repository to the installation on GitHub. |
| `permission_denied` | The token request asked for more than the App or installation grants. | Add the permission to the App and have the installation owner accept it, or narrow the project's token permissions. |
| `association_unauthorized` | The association failed the host, account or proof check. | Bind the installation as an admin, or have the creator store a `GITHUB_TOKEN` with access to the repository. |
| `association_check_failed` | The association check could not complete. | Retry; check the Hub logs for GitHub errors or rate limits. |
| `token_mint_failed` | Other token mint failures, such as a missing App ID or private key. | Check the configuration and the Hub logs. |

### Webhook failures

Check **Advanced → Recent Deliveries** in the App's settings on GitHub for the Hub's response:

- **`503`** (`GitHub webhook is not configured`): the Hub has no webhook secret. It rejects every delivery rather than accept unsigned events. Set the webhook secret.
- **`401`** (`invalid webhook signature`): the secret on GitHub and on the Hub differ, or a proxy altered the body or dropped the `X-Hub-Signature-256` header.
- **No delivery reached the Hub**: check that `https://<hub-public-url>/api/v1/webhooks/github` is reachable from GitHub through your firewall and reverse proxy.

A `ping` delivery answers `{"status":"pong"}`. Content events answer `{"status":"ignored"}` unless both trigger experiments are on.

### Running without webhooks

If GitHub cannot reach the Hub, leave the App's webhook inactive. The Hub then learns about installations from the Setup URL redirect, from discovery, and from the health check every 6 hours, and token mints report revoked or suspended installations when agents start. GitHub triggers need webhooks.

`webhooks_enabled` does not switch webhook processing on or off: a signed delivery is processed whenever it arrives. The setting chooses the health check interval and is one of the conditions for trigger readiness.

### Rotate the private key

GitHub allows several active private keys per App, so you can rotate without downtime:

1. In the App's settings on GitHub, generate a new private key.
2. Give the Hub the new key where the current one is configured: paste it into **Private Key (PEM)** and save (applied immediately), or replace the file at `private_key_path` or the `SCION_SERVER_GITHUBAPP_PRIVATEKEY` value and restart the Hub.
3. Run **Check Status** on a project, or **Sync Permissions**, to confirm that the Hub authenticates with the new key.
4. Delete the old key on GitHub.

Tokens already given to agents stay valid until they expire. To roll back, restore the old key on the Hub before deleting it on GitHub.

### Rotate the webhook secret

GitHub accepts only one webhook secret per App, so deliveries fail with `401` between changing it on GitHub and on the Hub. Keep that window short:

1. Generate a new secret.
2. Save it on GitHub (**Webhook secret** in the App's general settings).
3. Immediately save the same value as **Webhook Secret** in the **GitHub App** tab (applied without a restart), or update `SCION_SERVER_GITHUBAPP_WEBHOOKSECRET` or `webhook_secret` in `settings.yaml` and restart the Hub.
4. In **Recent Deliveries** on GitHub, redeliver any delivery that failed during the change, or run **Discover from GitHub** to resynchronize installations.

## See also

- [Auth & Tenancy: GitHub App Integration](/scion/hosted/single-node/auth/#github-app-integration): association rules, token-source projects, trigger permissions and repository routing.
- [Projects (Hub-managed vs Linked)](/scion/workstation/git-projects/): creating git projects and the personal access token alternative.
- [Agent Credentials](/scion/local/agent-credentials/): how agents receive GitHub credentials.
- [Admin Settings Model](/scion/reference/admin-settings/): Layer-1 sections, seeds and managed sections.
