# Release Notes (2026-11-06)

This period added Hub supervision of message-bus plugins with recovery of lost sessions, and applied the experimental image tag setting to agent images. It also added review summary notifications for artifact owners and extended delegated credentials to more list routes. Several Cloud Run, Kubernetes NFS and onboarding fixes landed too.

## 🚀 Features
* **Message-bus plugin supervision** (#4480): The Hub now supervises message-bus plugins such as Telegram and Discord. A lost plugin session is detected, the plugin is respawned or re-dialed with backoff, and its subscriptions are replayed. Health and `scion doctor` report the plugin as degraded while it recovers. Publishes to observer plugins are bounded, so a hung plugin can't stall a request.
* **Image tag applied to agent images (experimental)** (#4619): With `hub.image_tag_setting` on, the Hub resolves `image_tag` and sends it with every create, start, resume and restart. `version` means the Hub's own release. Only short-form `scion-*` images that are untagged or on `latest` take the tag. Saving `image_tag` with the experiment off returns 412.
* **Review summary notifications for artifact owners** (#4621): When someone other than the owner submits a review of an artifact version, the owner gets an `artifact_review_summary` notification with the start of the reviewer's note.
* **Delegated project agent, skill and template lists (experimental)** (#4614): An agent delegated credential can list a project's agents, project-scoped skills and templates, with a per-item read check on each.

## 🔒 Security
* **Cloud log level filter accepts only Cloud Logging names** (#4615): The cloud log query endpoints accept a log level only when it is a Cloud Logging severity name, matched case-insensitively. Any other value is refused before a filter is built.
* **Suspend revoke audit only after success** (#4596): A failed credential revoke during suspend is no longer recorded in the audit log as committed.

## 🐛 Fixes
* **Kubernetes provisioning on squashed NFS exports** (#4635): On an NFS export that maps root to the workspace owner, provisioning now accepts a refused chown when the entry already has the desired owner, instead of failing with EPERM.
* **Harness and telemetry environment on Cloud Run** (#4629): The Cloud Run runtime applies the harness environment, and the telemetry environment when telemetry is on, in the same order as Docker. Names the runtime reserves are not taken from the harness environment.
* **`sciontool doctor` under a non-root init** (#4630): When `sciontool init` runs as a non-root user, as on Kubernetes, `sciontool doctor` reports the credential state check as not applicable instead of warning.
* **Onboarding harness step always loads its list** (#4625): Every way into the onboarding wizard's harness step loads the harness configs, including a reload after the server restart the registry step asks for.
* **Runtime type switch clears Kubernetes-only keys** (#4617): In the admin server config editor, switching a runtime from Kubernetes to Docker or Podman clears the Kubernetes-only keys and Cloud Run blocks, so the save no longer sends them back.

## 🔧 CI & Infrastructure
* **Release notes compare against the previous release in the channel** (#4631): Preview, rc and stable release bodies list the PRs since the previous release in the same channel, not since the newest nightly, and start with a link to the docs release notes page.
* **Release-matched image builds** (#4622, #4632): `build-images.sh` stops when a needed base image is missing, warns when HEAD doesn't match the claimed release, and takes its version from the `v*` tag first.
* **Homebrew on Linux** (#4628): The generated Homebrew formula explains, on Linux only, when Homebrew installs its own glibc and gcc, and links the release tarball. The installation docs now match the formula.
