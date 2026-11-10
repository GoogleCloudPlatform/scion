# Release Notes (2026-11-09)

This period narrowed which Hub-scoped secrets reach agents, moved Runtime Broker Hub connections out of settings files, and made `sciontool init` drop `CAP_NET_ADMIN` once network setup is done. It also added chat plugin attachments as Hub uploads, publish warnings for open artifact reviews, conversion apply and rollback for legacy Runtime Brokers, and a long list of security and lifecycle fixes.

## ⚠️ BREAKING CHANGES
* **Hub-scoped secrets reach agents only in `always` or `as_needed` mode** (#4713): Start and restart deliver a Hub-scoped environment secret directly only in `always` mode. An `as_needed` secret is filled only by the harness credential pass, and only for a key the harness asks for. Agent read paths list only `always` Hub-scoped environment secrets; admin reads keep their full view. The telemetry credential file is still delivered.
* **Runtime Broker Hub connections leave settings files** (#4718): Runtime Broker registration no longer writes `hub_connections` into settings. Each connection lives in the Runtime Broker credential store, and `hub_connections` keys are refused as not writable.
* **Secret Manager migration runs as maintenance and verifies values** (#4729): The Hub's migration to GCP Secret Manager decodes each secret before writing it, reads it back to verify it, and keeps local copies until every secret is verified. Values an earlier run copied in encoded form are repaired, and a migration completed under an earlier release reopens once. `scion hub secret migrate` now refuses to run and points to the maintenance migration.
* **Telegram `downloads_path` retired** (#4725): Inbound Telegram attachments now go through the Hub as uploads (see Features), and the plugin's `downloads_path` setting is no longer used.

## 🚀 Features
* **Chat plugin attachments as Hub uploads** (#4725): Chat plugins can check with the Hub whether a sender may attach files, upload them as the sender's unsent uploads, and name them in inbound messages, so the Hub delivers them to each recipient's inbox. Telegram uses this. Uploads are limited to about 2 MiB per message for now.
* **Publish warnings for open artifact reviews** (#4710): Review mode tells the Hub a review is open. A publish that would overtake another person's open review of the current version answers 409 `review_open`, naming the reviewers. The publisher can publish anyway with force, which is recorded in the decision log.
* **Legacy Runtime Broker conversion apply, status and rollback (experimental)** (#4693, #4694): `scion runtime-broker convert` gains `--apply`, `--status` and `--rollback` for one instance on a remote-registration host. Apply is journaled and resumable, and rollback undoes it. Flat Runtime Broker instances with a `cloudrun` runtime target can now activate, using only their own credentials file.
* **Leave a project (experimental)** (#4712): Behind `hub.project_self_leave`, a signed-in user can remove their own direct membership of a project.
* **Event stream timeouts** (#4649): New settings `server.hub.sse.max_connection_age` (10m), `server.hub.sse.heartbeat_interval` (30s) and `server.hub.platform_request_timeout` bound event streams and Conduit sessions with jitter, and Cloud Run warns when the platform timeout is unset. Heartbeats that change nothing visible no longer emit status events, and auto-expose stops polling ports.
* **`hub env` and `hub secret` infer the project** (#4714): A bare `--project` on `scion hub env` and `scion hub secret` infers the project as `tunnel`, `ssh` and `ssh-config` do: the directory's Hub project, else the `scion use` project.
* **herdr plugin for scion agents** (#4719, #4730): An optional plugin under `extras/herdr-scion` shows the current project's running agents as herdr panes, with attach and start actions and a paged dashboard.

## 🔒 Security
* **`sciontool init` drops `CAP_NET_ADMIN`** (#4727, #4728): Init drops `CAP_NET_ADMIN`, including from the bounding set, after network setup and before the workspace clone, hooks, services and the harness, whenever a metadata server or the opt-in init firewall was set up. A failed drop refuses the start when init installed network controls. `sciontool metadata status` reports the iptables check as not verifiable once the capability is gone.
* **Workspace files API refuses `.git` paths** (#4690): The Hub's workspace files API refuses paths with a `.git` component and caps request body size.
* **Credential and secret lifecycle** (#4697, #4698, #4703): Auto-suspend of a stalled agent revokes its credentials, as manual suspend does. Progeny-shared owner secrets are decided once per request on the owner-material chain, and a legacy chain with no recorded ancestry is withheld. A co-located flat Runtime Broker activation gets a fresh Runtime Broker secret on every start.
* **Fixed client text for upstream failures** (#4699): GitHub App token errors and template and harness-config validation failures return fixed client text, and upstream text reaches only the server log, sanitized.
* **Skill versions and workspace checks** (#4696, #4688, #4709, #4692): Skill version reads by number return only published and deprecated versions unless the caller can update the skill. Workspace marker entries count only as real directories, judged without following links. Start-time repository probes run through the host git door. Rebuild requests validate the branch name before a run is created.

## 🐛 Fixes
* **Podman container IDs no longer include warnings** (#4671): Runtime commands capture stdout and stderr separately, so a rootless Podman warning on stderr no longer ends up in the container ID. Errors carry the tail of stderr. *(Contributor: Stephen Ierodiaconou)*
* **Agent lifecycle state** (#4715, #4717, #4702): A requested stop now ends the container at Exited (0) instead of 255. An agent at rest no longer shows a running container status. An agent create request that the Runtime Broker refused before making a container is finalized without waiting on the Runtime Broker, so deleting it no longer hangs.
* **Runtime Broker record faults** (#4691, #4704): A config edit that needs an unreadable Runtime Broker record answers a retryable 503 and writes nothing. Plugin Hub credentials fail when the plugin's Runtime Broker row can't be confirmed.
* **Onboarding and runtime validation** (#4716, #4706): Onboarding reports `harnessesSeeded` from the harness configs the setup wizard lists. Operator-only substrate validation now also covers the default profile.
* **Web fixes** (#4689, #4686, #4682): Every copy action uses one guarded clipboard helper that never throws and reports failure. The agent detail page shows Unlimited for stored zero limits. Chat starts a new group header when the recipient changes.
* **GitHub remote case recovery** (#4687): A `.git` suffix in any letter case is trimmed when recovering a remote's owner and repository.

## 🔧 CI & Infrastructure
* **Image builds record pushed digests** (#4665): Each Cloud Build step that pushes an image records its digest as step output, for the release image channel tagging to read.
