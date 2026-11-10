# Release Notes (2026-11-05)

This period added experimental zip downloads for artifacts and native service account identities for Cloud Run agents, and laid groundwork for agent images pinned to a release. It also made Kubernetes starts fail fast on NFS mount problems, fixed version reporting on preview builds, and started publishing weekly previews to Homebrew.

## 🚀 Features
* **Download an artifact version as a zip (experimental)** (#4610): Behind `hub.artifact_archive`, an artifact's Files tab gets a Download all (.zip) button, backed by `GET /api/v1/artifacts/{id}/versions/{seq}/archive`. The archive holds the version's uploaded files at their manifest paths. Copies of remote images are left out, as on the Files tab.
* **Cloud Run agents run as their assigned identity (experimental)** (#4598): Behind `hub.cloudrun_native_identity`, a Cloud Run dispatch in assign mode creates the instance with the service account the Hub names, after checking it against the Runtime Broker's `assignable_service_accounts` list. The instance's own metadata server then serves that account. An existing instance of the agent's run is recreated rather than restarted, so a changed identity or secret set always gets a fresh instance.
* **Project storage provisioning consolidated (phase 5b)** (#4607): Provisioning bookkeeping now lives only in the project's state directory, and the Hub, Runtime Broker and agent manager create workspace leaves through one provisioner. A one-time promotion at Hub start moves old workspace-root markers and clears stale lock files, never a live lock.
* **Image tag setting (experimental, not yet applied)** (#4603, #4599): Behind `hub.image_tag_setting`, a new `image_tag` setting lets agent images be pinned to a release tag instead of `:latest`. It resolves from `SCION_IMAGE_TAG`, then the Hub settings, then the settings file. It is validated but not yet applied to agent starts. A new `build-release-images.sh` script builds the image set from a release tag's own image specs.

## 🔒 Security
* **Notification header and host checks** (#4612): The email notification channel keeps each From, To and Subject header value on one line, collapsing line breaks. The Teams channel accepts a service URL only when its host is a Bot Framework service host (public cloud, GCC, GCC High or DoD).

## 🐛 Fixes
* **Onboarding identity applies without a restart** (#4590): After the identity step of the onboarding wizard, `/api/v1/auth/me`, the dashboard and `scion hub status` show the new display name and email right away, instead of the startup identity until the next server restart. *(Contributor: GodD001)*
* **Kubernetes starts fail fast on NFS mount errors** (#4602): A start whose NFS volume can't be mounted now fails quickly with the launch code `volume_mount_failed`, instead of waiting for the pod-ready budget to run out and reporting `launch_timeout`.
* **Refused Cloud Run resource specs return 400** (#4604): A Cloud Run resource spec the platform can't run is answered with 400 `validation_error` on create, start and restart, with a message naming the field, the value and the platform rule. An async launch reports it as `container_config`.
* **Agent initialization on Cloud Run and Kubernetes NFS homes** (#4606, #4601): `sciontool init` publishes its credential state outside `/run/scion`, which Cloud Run creates with an owner and mode that init refused. On Kubernetes pods with NFS homes, the provision status is now recorded even before the harness outputs directory exists, so a failure keeps its reason.

## 🔧 CI & Infrastructure
* **Preview builds report their own version** (#4609): Release binaries are stamped with the tag being built. A weekly preview such as `v0.3.0-preview.5` no longer reports the nightly it was cut from, and update checks no longer treat preview installs as nightly-channel.
* **Weekly previews on Homebrew** (#4594): Preview releases now build their images from the release tag, push them to `ghcr.io/homebrew-scion`, and publish a generated formula to the `homebrew-scion` tap.
