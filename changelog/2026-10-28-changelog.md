# Release Notes (2026-10-28)

This period hardened the conduit relay and Runtime Broker request handling, made hub-issued tokens mandatory for hub-dispatched starts, and made shared NFS project storage work on Kubernetes. Starred messages are now on by default, and forced user deletion arrives behind an experiment.

## ⚠️ BREAKING CHANGES
* **Conduit internal relay hop hardened** (#4282): deployments with a plaintext, non-loopback internal relay hop must follow the upgrade order in the "Upgrading" section of the separate-relay guide.
* **Hub-dispatched starts require a hub-issued token** (#4248, #4294): a hub-dispatched start takes the agent's Hub token only from the token the Hub issued. The Runtime Broker removes `SCION_AUTH_TOKEN` from the start environment. A start without a hub-issued token fails with `agent.ErrNoHubToken` and no container starts. A restart without one ends with the agent stopped.
* **Flat Runtime Broker pod placement checks** (#4295): a flat instance's runtime target gains `node_selector`, `tolerations` and `runtime_class_name`. A start is refused before any side effect if an agent sets a toleration or runtime class the target lacks, or the target's selector key or runtime class has changed. Run `scion doctor` (flat-placement-preflight) before upgrading and fix flagged agents.
* **Signed request bodies capped at 2 MiB** (#4274): the Hub's Runtime Broker verifier and the Runtime Broker HMAC middlewares bound signed request bodies before hashing them. Over-size requests get 413 `payload_too_large`.

## 🚀 Features
* **Starred messages on by default** (#4300, #4268): `web.chat_starred_messages` is now enabled by default, and admins can still turn it off. A new `chat-star-reconcile` maintenance operation cleans up stars; for multi-instance Hubs, the upgrade notes cover turning the experiment off, upgrading and reconciling before turning it on.
* **Forced user deletion (experimental)** (#4284): behind `hub.user_delete_force_cascade`, `DELETE /api/v1/users/{id}?force=true` revokes the user's roles, tokens and delegations, stops their agents, then removes the user. `GET /api/v1/users/{id}/deletion` reports per-agent progress, and the web adds a Force delete dialog with a status panel.
* **Shared NFS project storage on Kubernetes** (#4299, #4292, #4265):
  * Agent creation now works for every project shape with a Kubernetes Runtime Broker, and agent-scoped workspaces live on the share.
  * Runtime Brokers that only verify the NFS mount treat a mount-source mismatch as advisory.
  * The Brokers area shows each Runtime Broker's reported project storage layout and any mismatch with the Hub.
* **Store database diagnostics** (#4291): the Hub logs rate-limited warnings naming the caller when a store transaction holds a connection over 2s or a request waits over 1s, and records hold, wait and commit histograms. On by default.
* **`scion message --attach` warns on scratchpad fallback** (#4293): when attachments fall back to scratchpad staging, the CLI says why and warns that the files may not reach recipients on another Runtime Broker.

## 🔒 Security
* **Sandboxed content policies** (#4263): agent-served pages may use forms, dialogs and related sandbox features. Workspace files, chat attachments, GCS links and agent pages get their own Permissions-Policy.
* **Backstop clone sends no token** (#4290): the Runtime Broker's shared workspace backstop clone no longer passes a git token, so private repositories fail with the existing "needs credentials" error.
* **Artifact download redirects** (#4245): artifact file downloads enforce the HTTPS redirect policy and refuse redirect hops. *(Contributor: Ravshanbek Nazarov)*

## 🐛 Fixes
* **Owner secrets for adopted agents** (#4272): agents adopted from before recorded delegation, at the baseline or readonly role, again receive their owner's user secrets at start and restart.
* **Conduit in external relay mode** (#4278, #4285): the Hub tells agents to dial conduit through its ingress when a relay is eligible. A Runtime Broker's conduit dial through the ingress is no longer refused for a nonce recorded twice.
* **`sciontool` metadata server** (#4277, #4296): a refused GCP credential is served from cache with capped backoff instead of re-asking the Hub on every request. Concurrent identity-token fetches for different audiences no longer interfere with each other.
* **Container `SCION_HARNESS` follows the launched harness** (#4281): usage parsing and telemetry labels stay correct after a harness-config switch.
* **Sign-in deep links keep their query** (#4287): a page deep link returns to the same path and query after sign-in.
* **Single-node VM teardown** (#4289): `deploy.sh --delete` removes the Hub service account's project IAM bindings before deleting it, leaving no tombstones.
* **Smaller fixes**:
  * Content-addressed v1 skill lookups return the oldest matching version (#4208).
  * Authorization refusal follow-ups (#4288).
  * Client options guard nil receivers, headers and HTTP clients (#4253). *(Contributor: Ravshanbek Nazarov)*
