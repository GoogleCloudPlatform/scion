# Release Notes (2026-10-26)

This period brought a native Windows client CLI, Hub self-update for single-node VMs, and `scion hub delegation` commands. Dev authentication was tightened to single-use sign-in links and loopback-only Hubs, and secret rotation reached Hub-stored backups.

## ⚠️ BREAKING CHANGES
* **Dev sign-in uses a single-use launch link** (#4094): with dev auth on, the web server creates a session only from a short-lived, single-use link (`POST /auth/dev-launch`, then `GET /auth/dev-signin`). `scion server start` opens the UI through such a link, and the new `scion server open [--print]` produces one on demand. Without dev auth both routes return 404.
* **Dev auth requires a loopback Hub host** (#4112): with dev auth enabled and the web server disabled, `scion server start` refuses a non-loopback Hub host at startup.
* **`sciontool secret get` and `list` moved to `scion`** (#3985): inside agents, use `scion secret get` and `scion secret list`, which are now allowed in agent mode. `scion secret get --value` prints the value; metadata stays the default, and `--format json` stays metadata-only.
* **Hub refuses to start when the encryption policy cannot be met** (#4145): Hub-stored backups (signing keys, OIDC keyset) now use the shared versioned value cipher. A startup audit runs before any write and reports unreadable backups in readiness without overwriting them. The Hub refuses to start if the local key ring cannot satisfy the database encryption policy.
* **Single-node VM install layout** (#4179): Hubs with no repository path that relied on blanket sudo report self-update as unsupported until `migrate-layout` is run (see Hub self-update below).

## 🚀 Features
* **Native Windows client CLI** (#4098, #4205, #4209): `scion` builds and runs on Windows (amd64 and arm64) as a Hub client for ssh, ssh config, tunnel and VS Code use. Other commands fail with a clear "not supported on Windows" error. Private files (Hub credentials, profiles, settings, ssh and VS Code files) get owner-only permissions and are verified on open, and opens retry if the file is replaced concurrently. Releases now include Windows zips listed in `SHA256SUMS`, and a new docs page covers install and verification.
* **Hub self-update for single-node VMs** (#4179): `deploy.sh` installs versioned releases under `/opt/scion` with current and previous symlinks. `update-binary` takes a target version and uses pinned GitHub asset URLs, per-asset SHA256 checks and installer strategies with a preflight. Pending updates are reconciled at boot, update status is queryable, and the default `update_policy` is `notify`.
* **`scion hub delegation` commands** (#4196): user-mode `create`, `list`, `get`, `revoke` and `revoke-all` (with confirmation), backed by a new `hubclient` agent delegation SDK and an operator reference page.
* **Skill content hash v2 (experimental)** (#4195): behind `hub.skill_content_hash_v2`, skills gain a `contentHashV2` covering path, normalized mode and file hash. Pinned hashes are accepted in either form, and `contentHash` stays v1.

## 🔒 Security
* **Database settings redacted from integration config** (#4207): integration config responses omit every Hub-managed setting, including the database URL and driver.

## 🐛 Fixes
* **Inbox message claims** (#4210): the Runtime Broker inbox write script now claims messages with an exclusive file create instead of `mkdir`, so two writers can no longer both claim one message.
* **Quick switcher on phones** (#4206): opening the chat quick switcher focuses its search box, so the iOS keyboard appears.
* **Unknown user sub-paths** (#4201): unknown sub-paths under `/api/v1/users/{id}` return 404 instead of being treated as the bare user.

## 🔧 CI & Infrastructure
* **Release pipeline guarded on forks** (#4147): `build-release.yml` runs only on GoogleCloudPlatform/scion, so mirrored tags no longer publish releases on forks.
* **cr-ci runner** (#4194, #4101): the runner image gets the tools routed jobs expect, git built from pinned source, and trust-boundary docs. Forks can opt in to routing CI jobs to it via `SCION_LARGE_RUNNER`; it stays inert upstream.
