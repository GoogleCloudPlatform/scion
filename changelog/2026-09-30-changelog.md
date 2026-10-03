# Release Notes (2026-09-30)

Per-broker agent caps became a first-class, enforced setting, and GCP Secret Manager names are now hub-prefixed so several hubs can share a project. A Terraform module set for multi-hub HA landed. Messaging got two silent-loss fixes and an opt-in large-DM offload. The Cmd/Ctrl+K chat palette is complete and on by default.

## ⚠️ BREAKING CHANGES
* **Broker agent caps can apply on upgrade** (#2142, #2115, #2114): On boot, broker-scoped `max_agents_per_broker` bindings are migrated into per-broker settings. The engine never enforced these bindings before, so upgrading can impose or tighten a broker cap. New hubs seed the cap at 100, and broker quota enforcement is on by default. New broker-scoped bindings for this limit return 400; use the per-broker settings API instead.
* **GCP Secret Manager names are hub-prefixed** (#2123): New secrets, including the hub signing keys, are written as `scion-<12-hex hub hash>-…`. Legacy names still resolve through a read fallback that logs a WARN. Operators should run `scion hub secret migrate-names` (`--dry-run`, then `--delete-legacy`) and update IAM conditions to the new prefix. For terraform-ha Cloud Run hubs, see `docs/deploy/migrate-names-cloudrun.md` (#2166).
* **Unsafe raw messages rejected** (#2125): Raw messages combined with plain return 400. Raw messages with broadcast, groups, mentions, attachments, wake, interrupt or conversation addressing return 422 `unsupported_capability`. Raw messages are also refused on schedules, broker-inbound routes, managed backends and from other projects. A plain raw DM to one agent in the same project is unchanged.
* **`scion list` rejects positional arguments** (#2153): Use the new filter flags instead.

## 🚀 Features
* **Per-broker settings and enforced caps** (#2126, #2141, #2145, #2115, #2114): Brokers have a settings document (`GET/PUT /api/v1/runtime-brokers/{id}/settings`, which requires `quota.update`), and `maxAgents` is its first key. The effective cap resolves in this order: broker setting, then entitlement, then hub default. The broker detail page, the brokers list, admin quota usage and `scion hub projects info` show the cap and its source. A live hub switch can turn enforcement off, in which case caps show as "not enforced". Admins can edit the default and description of system limits.
* **Terraform HA module set** (#2149): Shared infrastructure (Cloud SQL PG16 regional, Filestore, GKE Autopilot, Artifact Registry) plus per-hub roots (Cloud Run hub behind IAP, per-hub DB and service accounts, Kubernetes namespace with Workload Identity, NFS PV) for several namespaced hubs in one GCP project. IAM grants are additive only.
* **Native chat palette complete** (#2169): Cmd/Ctrl+K now has a Documents group with an in-page preview. The grouped palette is on by default, and the rollout flag and the old switcher are removed.
* **`scion list` filters** (#2153): `--owner`, `--broker` and `--harness`, plus `--ancestors`, `--descendants` and `--lineage`. Filtering is done by the hub and only narrows results within projects you can access. These flags require a Hub.
* **Restrict agent-written secrets to profile scope** (#2171): The new admin setting `agent_secrets.user_scope_only` makes the hub reject agent secret writes to project scope (403 `secret_scope_restricted`, and `--force` doesn't bypass it).
* **Reincarnate migration guidance** (#2177, #2193): `scion reincarnate --handoff-template` prints the handoff template (it also works offline and inside agents). The new generation's preamble names who requested the migration, and while a migration runs the agent message reads "migrating to generation N".
* **Large-DM offload** (#2132): Large agent DM bodies can be delivered as a short stub with a fetch command, while the full body stays in the stored message. It's off by default (threshold 0) until agent images include the fetch command.
* **Edit stopped agents' config** (#2076): `PATCH /api/v1/agents/{id}` accepts `config` updates (model, image, env) for stopped agents, so they take effect on the next start. *(Contributor: G. Hussain Chinoy)*
* **Hub-wide experiments** (#2121, #2152): An experiments registry, `GET /api/v1/experiments`, and admin `GET/PUT/DELETE /api/v1/admin/experiments` guarded by the new `hub.experiments.update` permission. The admin UI comes later.
* **Chat additions** (#2128, #2117, #2136): "Mark unread" on threads, DMs and members. "Open terminal" and "Open in graph" on agent messages. Choosing Reply focuses the composer.
* **Copilot usage telemetry** (#2113): Copilot calls and tokens are derived from its native metrics, with cache-inclusive input corrected.
* **Model alias fixes across harnesses** (#2163, #2183): The hub keeps `model_aliases` and `model` from harness config on every write path, so tier picks like `L` resolve correctly. Codex resolves its default model through aliases. grok-build, muse-code, hermes and opencode resolve S/M/L/XL tiers through a shared helper.
* **Agent keys API groundwork** (#2118, #2137, #2139, #2140): The contract, broker `/keys` route, dispatch and authorization gate for a dedicated keystroke API that will replace raw messaging. It isn't reachable yet.

## 🔒 Security
* **Versioned user access token permission ceiling** (#2143): Each token's permission set is stored once as a versioned ceiling. A missing or unrecognized ceiling denies access. An idempotent migration converts existing tokens.
* **Hub-default GCP passthrough limited to local runtimes** (#2186): Passthrough is allowed only when the agent's resolved runtime profile is Docker or Podman. Kubernetes or unresolved profiles fall back to `block`.
* **Stricter DM consistency checks** (#2122): A `dm:` thread_id must match the conversation's own key, and an explicit recipient must be the other participant (and of the right kind).
* **Authorization hardening** (#2155, #2127, #2150): Identities are classified by concrete type and mismatches are denied. Broker on-behalf-of admission is explicit. Agent runtime secret reads require an originating user who is an active project member, or who has exact `secret.use` system authority.

## 🐛 Fixes
* **Agent messages lost after a hub restart** (#2119): An agent resumed after the hub restarted never got its message-broker subscriptions. Its messages to users returned "sent" but were never stored. *(Contributor: Stephen Ierodiaconou)*
* **Dropped user-message deliveries surfaced** (#2147): When an event-bus subscriber's buffer is full, the hub now returns a retryable 503 instead of answering 200 for a message that was never stored.
* **Intermittent ECHILD agent-start failures** (#2156): The PID-1 reaper in sciontool init no longer races `exec.Cmd.Wait` for its own child processes.
* **Harness image builds** (#2174, #2172, #2157): The muse-code CLI install and the hermes pip install are fixed. Every harness, including muse-code, is now built by Cloud Build, with a CI check for coverage. A single harness can be rebuilt with `--target`.
* **Codex and grok-build TOML config edits hardened** (#2178): Edits are checked against `tomllib`, a rejected edit leaves the file untouched, and failures raise errors instead of reporting success.
* **Single-node VM deploy** (#2151, #2120, #2124): The hub daemon's working directory is pinned to its home. Custom-mode `default` networks are supported, and the deployer permissions in the docs are corrected. `default_harness_config` is set in settings.yaml.
* **`scion hub secret migrate` and `migrate-names` work outside a project directory** (#2181).
* **Web performance** (#2185, #2179, #2175, #2176): File tabs and below-the-fold listings mount on demand (about 70% fewer DOM elements). The project file browser issues one initial listing per data source. Graph layout is cached across status-only updates, and file-list date formatting is much faster.
* **Operator logging** (#2168, #2167): 400, 409 and 422 errors are logged at INFO with their public reason. Successful provider self-heal restamps log one INFO line.
* **Smaller hub fixes** (#2180, #2164, #2148, #2161, #2138): `/usage/me` no longer lists the broker-scoped cap. Admin limit and settings edits trim whitespace and handle zero values consistently. Every 405 response carries an `Allow` header. A missing sender record no longer causes side effects.

## 📖 Docs
* **HA setup** (#2162): The setup-gcp `settings.yaml` example includes the required `server.hub.hub_id`.
* **IAP data-access audit logging** (#2130): How to enable it for the Cloud Run proxy.
