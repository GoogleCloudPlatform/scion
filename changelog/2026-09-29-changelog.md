# Release Notes (2026-09-29)

Messaging reliability led the day: large messages to agents were silently dropped, @mentions from agents went undelivered, and agent reply rows could get stuck pending and later be purged. Sign-in, broker registration and scheduled dispatch permissions were tightened. The native chat gained a Cmd/Ctrl+K quick command palette.

## ⚠️ BREAKING CHANGES
* **Sign-in requires a provider-verified email** (#2071): Google, GitHub and OIDC sign-in only accept an email the provider marks as verified. GitHub sign-in always checks the account's email list. Users whose only email is unverified can no longer sign in.
* **Broker registration requires `broker.create`** (#2063): Registering a broker or re-minting its join token (`POST /api/v1/brokers`, and the embedded-broker path of `POST /api/v1/projects/register`) needs `broker.create`, which is granted through the built-in hub-member role. Viewer-role users can no longer register brokers, and scoped user access tokens don't satisfy the owner or super-admin shortcuts. The hub-member role is reconciled to revision 3 on start.
* **Scheduled `dispatch_agent` work requires an unscoped credential** (#2066): Creating, updating, re-targeting or resuming scheduled `dispatch_agent` events and schedules with a scoped user access token is now denied. This matches the existing rule for scheduled messages.

## 🚀 Features
* **Native chat quick command palette** (#2069, #2104, #2084): Cmd/Ctrl+K opens a fuzzy-matched palette behind a rollout flag. It has Agents (opens the DM, including with peers you haven't messaged before), Threads, People and Recent Files groups, and a shared keyboard model where Tab cycles groups. Recent files open in a reusable file viewer.
* **Messages deferred during reincarnate** (#2079): Messages to a reincarnating agent are saved instead of dropped. DMs, group messages and @mentions return 202 `deferred`, and scheduled messages fail loudly. The new generation is told to catch up with `scion conversation catch-up`. DM conversations now register both participants, so they appear in `scion conversation list`.
* **@mentions from agents are delivered** (#2083): When an agent's message @mentions another agent, the mentioned agent now receives it. Mentions are deduplicated against explicit recipients, limited to the same project and rate-limited per delivery without failing the primary message.
* **Hub settings control agent image and `imagePullPolicy`** (#2074): A settings-level `image_pull_policy` field (with a profile override) joins `image`. Both are re-evaluated on every start in this order: request override, then template, then Hub settings, then the harness-config default. `scion reincarnate` plan previews follow the same order.
* **Broker capacity on project providers** (#2097): `GET /api/v1/projects/{id}/providers` reports `agentLimit` and `agentCount`, and `scion hub projects info` shows `(agents: count/limit)`.
* **Harness usage telemetry for Codex, OpenCode and Antigravity** (#2065, #2082, #2087): Codex usage comes from its native events, and Codex's native log ingestion is fixed. OpenCode publishes one usage event per completed LLM step. Antigravity publishes call counts (it reports no tokens).
* **Credential attribution in logs and audit** (#2090, #2091, #2092): User access tokens can carry bounded, descriptive purpose and label metadata. Credential attribution flows through request logs, authorization decisions and audit records. Scheduled and async work records who initiated it.
* **Hybrid single-node VM deployment** (#2100): `scripts/single-node-vm/deploy.sh` can optionally attach an existing GKE cluster as a second runtime with a shared NFS export. This is off by default.
* **Chat quality-of-life** (#2096, #2080): `owner/repo#N` references auto-link to GitHub, and thread-group collapse state persists per user.
* **Project roles can assign GCP service accounts** (#2062): Project owner, admin and member roles gain `gcp_service_account.assign` for verified project-scoped service accounts. When `gcp_iam_check_mode=enforce`, the caller's IAM actAs grant is also checked.

## 🔒 Security
* **Opaque pagination cursors** (#2106): List cursors for templates, harness configs and groups are sealed with AES-256-GCM and bound to the endpoint, filter and caller. Invalid cursors return a uniform 400 `invalid_cursor`.
* **Stricter agent token checks** (#2078): Agent token authentication and refresh require a successful credential-status check. Credential-store errors return 503 instead of authenticating.
* **Secret reads fail closed** (#2085, #2072): Runtime secret reads go through a single check sequence (project permission, active member originator, progeny sharing) with one audit event per request. Values are fetched only for the recorded metadata version, and retrieval errors are reported per item instead of returning an empty value.
* **Direct-conversation recipient validation** (#2099): The recipient check also runs when a raw `conversation_id` is supplied.

## 🐛 Fixes
* **Large agent messages no longer dropped** (#2107): Messages are streamed into uniquely named tmux buffers. Before this, messages over about 16 KB (including several smaller ones batched together) were silently lost.
* **Agent reply rows stuck pending** (#2081): Agent replies to users and threads were saved as pending. That blocked promoting a DM ("Agent is still responding"), and the rows were later marked failed and purged after 7 days. Replies are now saved as dispatched, the sweep only covers agent recipients, and existing rows are repaired at boot.
* **Own message flashes conversation unread** (#2112, #2111): Sending a message no longer marks your own thread or DM as unread. The open conversation is no longer swapped out when your user ID resolves in the background.
* **Kubernetes root agent home directory** (#2108): Root agents on Kubernetes now get `/root` instead of `/home/root`.
* **Runtime unavailability reported as 503** (#2098): When the broker can't list agents, it returns a retryable 503 instead of a 404, a raw 500 or a false-success 202.
* **Co-located broker registration failures surfaced** (#2070): A co-located broker that fails to register marks `/healthz` degraded, and `scion server status` gives the reason.
* **GKE auth fallback scope** (#2058): The Application Default Credentials fallback requests `userinfo.email`, so email-based RBAC bindings match.
* **Agents can read their own record** (#2095): `GET /api/v1/agents/{id}` allows an agent to read itself. *(Credit: miller79)*
* **Second-broker cleanup** (#2059): `runtime-broker --foreground` works with systemd `Type=simple` units. Broker status can list providers right after `--auto-provide`. Releases publish a `SHA256SUMS` file, which `deploy.sh` and the hub updater verify.
* **Clearer SA-assign denials** (#2056): A 403 caused by the delegation ceiling now explains why.

## 📖 Docs
* **Admin user provisioning design** (#2064): Design for `POST /api/v1/users` as invitation-equivalent pre-registration.
* **Hook usage dialect fields** (#2061): Documents the `cache_write_tokens` and `reasoning_tokens` fields and the rule that output tokens include reasoning.
