---
experiment: hub.agent_secrets
---

# Asking users for secrets: secret requests

A **secret request** (`secret-request`) is a message asking a human to supply
one to ten **agent-scoped** secrets for you without pasting values into chat.
In web chat it renders as a masked form; on other surfaces (Slack, Telegram,
Discord, Google Chat) it renders as a Markdown fallback with a link to the
Hub's standalone form (`/secret-requests/<messageId>`).

## Rules for handling secrets

- **Never ask for values in chat.** Always send a secret request instead.
- **Read inline when needed.** Fetch a fulfilled value directly where you need
  it: `$(scion secret get --value KEY)`.
- **Never expose values.** Never echo a secret value, write it to files
  (workspace, `/tmp`, or shared directories), commit it, or include it in
  messages, artifacts, or logs.
- **If a user pastes a secret into chat anyway:** do not echo, repeat, or store
  the pasted value. Tell the user to rotate the exposed credential immediately
  and send a `--secret-request` for the replacement instead.

## Sending a secret request

Send to a user or a conversation (`user:<email>`, `@<email>`, `conv:<id>`, or
`#<thread>`). Agents, `group[...]`, and broadcasts are refused. You cannot
combine secret requests with `--attach`, `--artifact`, or `--wake`.

```bash
# Single key
scion message "conv:<conversation-id>" --secret-request GITHUB_TOKEN \
  --secret-description "GitHub PAT with repo scope for org foo" - <<'EOF_MESSAGE'
I need a token to push the release branch.
EOF_MESSAGE

# Multiple keys or typed secrets (JSON array, 1-10 entries, max 32 KiB)
scion message "conv:<conversation-id>" --secret-request-file req.json
```

Example `req.json`:

```json
[
  {
    "key": "DEPLOY_KEY",
    "description": "SSH deploy key for staging",
    "type": "file",
    "target": "~/.ssh/id_ed25519"
  },
  {
    "key": "NPM_TOKEN",
    "description": "Read-only npm token for private packages"
  }
]
```

- `key`: required, `[A-Za-z_][A-Za-z0-9_]{0,127}` (compared case-insensitively;
  `SCION_*` and `GCE_METADATA_*` are reserved for `environment` secrets).
- `description`: optional, up to 1000 characters. Descriptions are metadata
  visible to anyone with `agent.read` — never put secret values in them.
- `type`: optional, `environment` (default), `file`, or `variable`.
- `target`: required when `type` is `file`; must be under `~/` and must not
  contain `..`.
- `--secret-description` applies only with a single `--secret-request`; use
  `--secret-request-file` (or `-` for stdin) for multiple keys with
  descriptions.

After sending, signal `sciontool status blocked "Waiting for secret request"`
(or continue unblocked work) until the user responds.

## Waiting for and using a fulfilled secret

When an eligible responder (a signed-in recipient with `agent.attach`) submits
the form, the Hub stores the value at agent scope and sends you a
`secret-response` message (waking you automatically if needed). Read it inline
when you need it:

```bash
curl -H "Authorization: Bearer $(scion secret get --value GITHUB_TOKEN)" https://api.github.com/user
```

List your available secrets (showing `SCOPE`) with `scion secret list`, or pin
a lookup with `scion secret get --value --scope agent KEY`.

If the responder dismisses the request, you receive a `secret-response` message
with their optional reason. Unanswered requests expire after **7 days** (no
message is sent on expiry).

## Re-asking (`--update`) and withdrawing

- **Pending key:** You can have at most one `pending` request per key. Re-sending
  a pending key without `--update` is refused (`secret_request_key_pending`).
  Pass `--update` to mark the pending version `replaced` and issue `v(n+1)`.
  When revising a multi-key card, resend all still-pending keys of that card
  together.
- **Rotation (terminal key):** Once a key's latest version is `fulfilled`,
  `withdrawn`, `dismissed`, or `expired`, a plain send (without `--update`)
  creates the next version.
- **Withdraw when no longer needed:**
  ```bash
  scion secret request withdraw GITHUB_TOKEN --reason "no longer needed"
  scion secret request withdraw GITHUB_TOKEN --set --reason "switched approach"
  ```
  `--set` withdraws every other pending key on the same card. Repeating
  `withdraw` on an already-withdrawn key is an idempotent no-op.
- **Inspect your requests:**
  ```bash
  scion secret request list
  scion secret request list --all --json
  ```
  Filter flags: `--pending` (default), `--fulfilled`, `--withdrawn`,
  `--dismissed`, `--expired`, `--all`.
