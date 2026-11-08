# Release Notes (2026-11-07)

This period started the chat permission model with a new built-in project-viewer role, added an optional separate content host for shared artifacts, and tightened `sciontool init` network capabilities. It also added grant notifications for artifacts and a version line on the server config page, and hardened the release image pipeline.

## ⚠️ BREAKING CHANGES
* **Chat permissions and the built-in project-viewer role** (#4643): Chat resolvers now decide chat send, history, the thread list, promotion, uploads and creating topics and group conversations. Topic responses report each write action in `_capabilities`, with `canPost` kept as an alias. A new built-in `project-viewer` role can read a project and its chat but can't post messages or create agents. A reader without chat post permission gets 403 `chat_post_denied`. An existing custom role named `project-viewer` is renamed to `project-viewer-custom-<id8>`.

## 🚀 Features
* **Separate content host for shared artifacts** (#4648): A new opt-in `server.hub.content_url` gives the Hub a second public origin that serves only artifact share links and artifact views, apart from its own origin and sign-in. On that host every other route answers 404 before authentication, and cookies, `Authorization` and other credentials are stripped. The value must be https with no path, is set by admins only, applies without a restart and is audited. The CLI and the web share dialog use it.
* **Notify the user named in an artifact grant** (#4646): A grant request can set `notify`. After the grant commits, the Hub sends that user one notification naming the granter, the level and the artifact. It's sent only from an interactive session, only when the grant raises the user's level, and within rate limits. A notice that isn't sent never blocks the grant.
* **Version line on the server config page** (#4642): The server config page shows a read-only version and effective channel, with a Manage updates link to Maintenance, in place of its duplicate update controls.
* **Copy link address in chat** (#4654): The message menu's "Copy link" is renamed "Copy link to msg", and a new "Copy link address" copies the URL of an http, https or mailto link in a message body, including on touch screens.
* **Shared-dir PVC defaults in the web UI** (#4639): The admin server config page has inputs for `shared_dir_storage_class` and `shared_dir_size`.

## 🔒 Security
* **`sciontool init` drops the extra network capability** (#4636): When the metadata mode needs network interception, `sciontool init` sets up the network policy itself, then drops the extra network capability from every capability set of every thread, including the bounding set, before hooks, services and the harness start. If the policy can't be set up or the drop fails, init exits without starting the harness. An opt-in `SCION_INIT_FIREWALL` setting runs the image's egress firewall script from init.
* **Rebuild branch validation** (#4642): Both rebuild executors reject a branch name with a leading dash or one that `git check-ref-format --branch` doesn't return unchanged.

## 🐛 Fixes
* **Chat lookup faults answer 500, not 404** (#4659): A store error during a chat topic or project lookup now returns a generic 500 instead of "Thread not found". A missing project or topic still returns 404.
* **Update staging cleaned up after an update** (#4653): A successful binary update removes its download directory before the restart and marks the plugin staging as applied, so neither is left behind.
* **Artifact review text on touch screens** (#4650): Review comment boxes, the review document and the Source editor stay at 16px on touch screens, so iOS no longer zooms on focus and leaves the review toolbar out of view.

## 🔧 CI & Infrastructure
* **Release image channel tags by digest** (#4655): `build-release-images.sh --channel-tag preview|stable` points each image's channel tag at the digest the run pushed and never moves `:latest`. It refuses unpushed builds, mismatched or nightly tags and `--also-tag latest` before cloning or building.
* **Homebrew images must be publicly pullable** (#4657): The Homebrew images job now fails if any pushed image can't be pulled anonymously from ghcr.
* **Release tag forms only** (#4645, #4644): Build version detection accepts only the release tag forms (`vX.Y.Z`, `-preview.N`, `-rc.N` and `nightly-YYYYMMDD`). Any other tag builds as `dev` with a warning. The single-node VM deploy now builds its release images with the release image driver.
