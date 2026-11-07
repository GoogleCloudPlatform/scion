---
title: "v0.3.0-preview.7"
---

Released on 2026-11-07 (`v0.3.0-preview.6..v0.3.0-preview.7`, changes since `v0.3.0-preview.6`).

`v0.3.0-preview.7` adds experimental Cloud Run native service-account assignment (`hub.cloudrun_native_identity`), applies the `image_tag` setting to short-form `scion-*` agent images (`hub.image_tag_setting`), extends agent delegated credentials to project agent, skill and template lists, and registers the `hub.conduit_deliver` experiment for message delivery over Conduit. Notification channels enforce single-line email headers and Teams Bot Framework hosts, cloud log queries validate Cloud Logging severity names, and artifact owners receive `artifact_review_summary` notifications when a review is submitted. Cloud Run applies harness and telemetry environment variables in Docker's precedence order, `sciontool doctor` treats non-root `sciontool init` runs as not applicable, onboarding and Admin → Server Config fixes clean up setup flows, and weekly preview releases publish to the `homebrew-scion` tap with release-matched image build guards.

---

## Highlights

### 1. Cloud Run Native Identity, `image_tag` Application and Delegated Project Lists

Behind `hub.cloudrun_native_identity`, Cloud Run dispatches in `assign` mode verify the Hub-assigned service account against `runtimes.<entry>.cloudrun.assignable_service_accounts`, create the instance with that account so its metadata server serves it in `passthrough` mode, and recreate existing run instances on start so identity or secret changes always get a fresh instance. With `hub.image_tag_setting` on, the Hub resolves `image_tag` (`version` resolves to the Hub's release) and applies it on create, start, resume and restart to short-form `scion-*` images that are untagged or `:latest` (`412` when saving `image_tag` while the experiment is off). Agent delegated credentials can list a project's agents, project-scoped skills and templates with per-item read checks, and `hub.conduit_deliver` is registered to gate message delivery over Conduit (`deliver-message`).

### 2. Notification Channel Hardening, Cloud Log Filter Validation and Artifact Review Notifications

Email notification headers (`From`, `To`, `Subject`) replace CR and LF runs with a single space so each header stays on one line, and the Teams channel accepts a `service_url` only when its host is a Teams Bot Framework service host across public cloud, GCC, GCC High and DoD. Cloud log query endpoints accept a log level only when it matches a Cloud Logging severity name case-insensitively, and agent suspend records a credential revoke audit entry only after revocation succeeds. When someone other than the artifact owner submits a review of an artifact version, the owner receives an `artifact_review_summary` notification with the start of the reviewer's note.

### 3. Cloud Run Environment Precedence, Non-Root `sciontool doctor`, Onboarding Fixes and Homebrew Previews

The `cloudrun` runtime applies the harness environment—and the telemetry environment when telemetry is on—in the same precedence order as Docker while refusing runtime-reserved variable names from the harness environment, and `sciontool doctor` reports the init credential state check as not applicable (`INFO`) when `sciontool init` runs as a non-root user such as UID 1000 on Kubernetes. Completing the onboarding wizard's identity step applies the new development identity immediately without a restart, the onboarding harness step loads its harness-config list on every entry (including after the registry step's server restart), and switching a runtime from Kubernetes to Docker or Podman in Admin → Server Config clears Kubernetes-only keys and Cloud Run blocks. Weekly preview releases publish release-tagged images and formulas to `homebrew-scion` with Linux `glibc`/`gcc` formula notes, and `build-images.sh` enforces release-matched image guards.

---

## 🔐 Security & Access Control
- **Notification header and Teams host checks (#4612):** The email notification channel replaces runs of CR and LF in `From`, `To` and `Subject` header values with a single space so each header stays on one line, and the Teams notification channel accepts a `service_url` only when its host matches a Teams Bot Framework service host (public cloud, GCC, GCC High or DoD).
- **Cloud log level filter validation (#4615):** Cloud log query endpoints accept a log level only when it matches a Cloud Logging severity name case-insensitively, refusing any other value before building a filter.
- **Suspend credential revoke audit timing (#4596):** Suspending an agent records the credential revoke audit entry only after the revocation succeeds.

## 🤖 Agent Lifecycle & Workspaces
- **Harness and telemetry environment on Cloud Run (#4629):** The `cloudrun` runtime applies the harness environment, and the telemetry environment when telemetry is enabled, in the same precedence order as Docker while excluding runtime-reserved variable names from the harness environment.
- **`sciontool doctor` under a non-root init (#4630):** When `sciontool init` runs as a non-root user (such as UID 1000 on Kubernetes), `sciontool doctor` reports the init credential state check as not applicable (`INFO`) instead of logging a warning.

## 💬 Chat & Messaging
- **Artifact review summary notifications (#4621):** When someone other than the artifact owner submits a review of an artifact version, the owner receives an `artifact_review_summary` notification with an excerpt from the start of the reviewer's note in the web notification tray and `scion notifications`.

## ☁️ Runtime Brokers, Settings & Operations
- **Live onboarding identity for development auth (#4590):** Saving the identity step of the onboarding wizard updates the active development identity immediately so `/api/v1/auth/me`, the web dashboard and `scion hub status` reflect the new display name and email without a server restart.
- **Homebrew preview tap and release-matched image build guards (#4594, #4628, #4622, #4632):** Weekly preview releases build release-tagged images, push them to `ghcr.io/homebrew-scion` and publish a generated formula (`update-homebrew-formula.sh`) to the `homebrew-scion` tap with Linux-only notes and installation docs explaining when Homebrew installs `glibc` and `gcc`; `build-images.sh` stops when a required base image is definitely missing, warns when `HEAD` is on a release tag but `--tag` is not given, and resolves `VERSION` from a `v*` tag first.

## 🧪 Experiments & Groundwork
- **Cloud Run native service account identity (#4598):** Behind `hub.cloudrun_native_identity`, a Cloud Run dispatch in `assign` mode checks the Hub-assigned service account against the Runtime Broker's `runtimes.<entry>.cloudrun.assignable_service_accounts` list, creates the instance with that service account so its metadata server serves it in `passthrough` mode, and deletes and recreates an existing instance of the agent's run on start so a changed identity or secret set always gets a fresh instance.
- **`image_tag` applied to agent images (#4619):** With `hub.image_tag_setting` enabled, the Hub resolves `image_tag` (`version` means the Hub's own release) and sends it with every agent create, start, resume and restart; only short-form `scion-*` images that are untagged or `:latest` take the tag, and saving `image_tag` with the experiment off returns `412`.
- **Delegated project agent, skill and template lists (#4614):** An agent delegated credential can list a project's agents, project-scoped skills and templates (`GET /api/v1/projects/{projectId}/agents`, and project-scoped entries from `GET /api/v1/skills` and `GET /api/v1/templates`), with a per-item read check on each entry.
- **Message delivery over Conduit experiment (#4618):** Registers the server-layer alpha experiment `hub.conduit_deliver` (off by default, requiring `hub.conduit`) to gate delivering message text and interrupts to an agent over its Conduit session (`deliver-message` control route) instead of through runtime exec.

## 🎨 Web UI
- **Onboarding harness step list loading (#4625):** Every entry to the onboarding wizard's harness step loads the Hub's harness configs, including a reload after the server restart requested by the registry step, and displays the HTTP status and a Retry button when loading fails.
- **Clearing Kubernetes-only runtime keys on type switch (#4617):** In the Admin → Server Config editor, switching a runtime from Kubernetes to Docker or Podman clears and hides Kubernetes-only keys and Cloud Run blocks so saving does not send stale fields back.
