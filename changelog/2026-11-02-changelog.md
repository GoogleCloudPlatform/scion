# Release Notes (2026-11-02)

This day enforced the agent hold on every message path, made TLS the Helm default for the relay hop between Hub nodes, and brought operator placement rules to legacy Kubernetes Runtime Brokers. It also extended experimental chat retention to deleted conversations, added hub-boundary access tokens in the web UI, and cleaned up agent directories on NFS project storage.

## ⚠️ BREAKING CHANGES
* **Helm: TLS on the relay hop between Hub nodes** (#4304, #4489): `conduit.internalTls` defaults to `auto`, which turns TLS on above one replica. Multi-replica installs then need `conduit.internalTls.existingSecret` (a `kubernetes.io/tls` secret; cert-manager works) or `conduit.internalTls.enabled: false`. Render guards refuse a non-loopback relay listen address without TLS. The relay wiring needs a Hub image at `nightly-20261029` or later, or `v0.3.0-preview.5` or later.
* **Operator placement rules on legacy Kubernetes Runtime Brokers** (#4487): Legacy Kubernetes starts and restarts now follow the operator placement policy. Agents may add selector keys but not change operator keys, and tolerations and runtime class must match the policy. Agents outside the policy are refused at their next start or restart, before anything is provisioned or stopped; running pods are unchanged. Use `scion doctor` or the placement report to find them. A restart without the project, from an older Hub, is refused before the stop.
* **Held agents refuse every message send** (#4507): The agent hold check now covers all sends to an agent: user sends with or without wake, agent sends with wake, chat integration inbound and mentions, topic messages and broadcasts, group sends and project broadcasts. A held target is refused before any wake, store write or dispatch.

## 🚀 Features
* **Chat retention purges deleted conversations (experimental)** (#4476): With `hub.chat_retention` and retention enabled, messages of soft-deleted conversations and topics are purged after `deleted_conversation_grace_days` (default 30), even when no retention window is set. Attachment bytes no longer linked to by any message are deleted after each purge.
* **Configurable failed-message retention** (#4474): A new `messaging.failed_message_retention_days` setting (default 7, minimum 1) in the admin messaging settings controls the failed-message purge, which now runs in short, paced chunks.
* **Hub-boundary access tokens in the web UI** (#4497, #4496): The access tokens page can create a user access token with a Hub boundary, covering any project the user can access, and set a purpose and up to 8 labels. The scopes API reports which selectors the user may pick on a Hub boundary, and refusals at token mint or delegation issuance return a typed 403 `scope_violation`.
* **Agent lifecycle analytics events (experimental)** (#4432): Behind `hub.lifecycle_events`, the Hub emits versioned `agent.lifecycle` events for create, start, stop, launch, reincarnation and reported config, delivered through a transactional outbox. This adds an automatic, additive schema migration (two tables, two columns).
* **Sender time zone on messages** (#4460): A message to an agent from a person whose profile has a time zone carries it as `sender_timezone` metadata, so the agent can read times in the sender's zone. Only the Hub sets it.
* **Server Configuration saves only changed sections** (#4486): On database-backed and workstation Hubs, a save sends only the changed sections, each guarded by its revision. A section another admin saved since the page loaded is refused with 409 and a reload prompt.
* **Settings environment-override badges** (#4502): The Federation page, Harness Configs section and project default scratchpad switch show when an environment variable overrides a value, and the Agent Defaults sub-tabs work again.
* **Hub chart termination grace period** (#4482): The combined Hub and Runtime Broker pod gets `hub.terminationGracePeriodSeconds` (default 45), so conduit draining and in-flight launch reporting finish before shutdown.
* **CLI usability** (#4499, #4488, #4493): `scion conversation list` and `scion conversation get` print full IDs that can be passed back as `conv:<id>`. Login to a Hub behind Identity-Aware Proxy fails with a clear message, and the docs cover workstation IAP access. Refusals under a project-scoped token name the exact missing scopes.

## 🔒 Security
* **Federation tokens honor `allowed_domains`** (#4484): The trusted issuer's `allowed_domains` now applies on the federation-token authentication path, as it already did on the other bearer paths.
* **Attachment deletes stay inside the store** (#4491): Attachment deletes validate the ID as reads do and remove the entry through a handle on the store root, so a delete can't reach outside the store.
* **Client path parameters are escaped** (#4472): Every `hubclient` method that puts a caller-supplied ID into a URL path now escapes it.

## 🐛 Fixes
* **Agent directories cleaned up on NFS project storage** (#4495, #4503): Agent delete removes per-agent directories from the share, clearing old provisioning-lock files first. Audits check the share is mounted and record real outcomes. A failed Cloud Run start removes the directories it created, including ones a failed mkdir left behind part-way.
* **Cloud Run agent logs no longer hang** (#4500): Log reads cover the last 24 hours with a 20s deadline, return the newest entries read, and name a missing logging permission.
* **Key backups stay put on restart** (#4494): A Hub restart no longer re-saves signing-key and OIDC key backups that are already current, so their versions stop changing on every restart.
* **Runtime switch to Cloud Run clears shared-dir defaults** (#4501): Switching a runtime entry to Cloud Run on the admin server config page now also clears the Kubernetes-only shared directory storage class and size.
