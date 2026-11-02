# Release Notes (2026-10-30)

This period hardened host-side git and repository credentials, added confirmed downgrades with database snapshots and a boot-failure rollback for single-node Hubs, and introduced an opt-in pi harness. Count limits can now be set to Unlimited, and shared-storage deletion moved to a single Hub-side deleter.

## ⚠️ BREAKING CHANGES
* **`max_turns: 0` now means Unlimited** (#4356): count limits (`max_turns`, `max_model_calls`) have three states: unset inherits, a positive value caps, and `0` means no limit. An existing `0` value changes meaning from "inherit" to "Unlimited". On a Runtime Broker without the `unlimitedCountLimits` capability, the Hub refuses the change with 422.
* **Runtime Broker NFS mounts require `nosuid,nodev`** (#4377): every NFS mount the Runtime Broker makes ends with `nosuid,nodev`. Agent create, start and restart on local-container runtimes are refused with `503 nfs_unavailable` when the project share lacks either flag.
* **No host git credentials without a project secret** (#4376, #4352): Hub git operations, the Runtime Broker backstop clone and agent-start clones no longer fall back to the host's git credential helpers or askpass. The project's git token is offered only to the project's recorded remote. SSH repository URLs require a project secret.
* **Shared-storage deletion through the Hub** (#4354): the Hub is the only remover of a deleted project's shared tree, and every Hub replica must mount the shared project storage. If the Hub cannot see the share, delete returns 200 with `storageKept` instead of 204 and reports `project_storage_tree` as degraded. Runtime Brokers keep their project directory while agents remain (409).
* **Root-level web paths require a session** (#4374): only files shipped in `web/public/` skip the session check. Every other root-level path sends browsers to sign-in and returns 401 to other clients, including `/robots.txt`.

## 🚀 Features
* **Confirmed downgrades and rollback for single-node Hubs** (#4371): older releases can be installed after a typed confirmation (409 `downgrade_requires_confirmation`; refused below the downgrade floor or on non-SQLite stores). Each update first takes a database snapshot and a settings copy, keeping five. A new boot-failure guard in `deploy.sh` swaps a crash-looping, unverified release back to the previous one. Restore docs are included.
* **pi harness (opt-in)** (#4353, #4367): a new container-script harness bundle for pi, pinned to 1.3.0. Model IDs keep their colons (Ollama tags, Bedrock versions), and only pi thinking suffixes are stripped.
* **Kubernetes runtime and project settings editors** (#4339, #4373): the admin Runtimes editor edits Kubernetes placement and identity fields (node selector, tolerations, service account mappings, runtime and priority class). Project Settings gains an editor for the per-profile default GCP service account map.
* **Deleted users' schedules pause** (#4364): when a user is deleted, schedules whose recorded authority is that user are paused with `creator_deleted` in every project, without blocking the delete.
* **User role changes and deletion with access tokens** (#4365): user access tokens can change roles with `user:promote` (member and viewer only) and delete users with `user:delete`, under Hub-level checks.
* **Kubernetes Role guidance** (#4363): `scion doctor` permission remediations recommend a namespaced Role holding only what the Runtime Broker uses, and the Kubernetes docs gain a Runtime Broker permissions section.

## 🔒 Security
* **Host git hardening** (#4378, #4381, #4379, #4358):
  * Host git refuses argument lists that are empty or start with an option.
  * Discovery is pinned to the resolved directory, including the after-run check.
  * Host git always runs with `LC_ALL=C`.
  * The Runtime Broker's workspace probe runs through the same guarded entry point.
* **Conduit relay tokens on the signing ring** (#4355): conduit relay admission tokens and relay-peer signatures use the keyring's versioned signing ring with key IDs. Unversioned values are accepted only from legacy generations.

## 🐛 Fixes
* **Env-gather provenance** (#4380): `hubHas[].scope` labels now follow the shared env scope precedence, and Runtime Broker-only keys are labeled correctly.

## 🔧 CI & Infrastructure
* **Release crons after DST** (#4375): the nightly moves to 03:00 UTC and the weekly preview to Friday 15:00 UTC, keeping 7 PM and 7 AM Pacific after daylight saving ends.
