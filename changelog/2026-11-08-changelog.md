# Release Notes (2026-11-08)

This period made release builds default their agent images to their own version tag, moved the Cloud Run native identity decision into the Hub, and tightened how upstream text and caller input reach logs. It also aligned the Kubernetes environment order with Docker and fixed chat copy actions on plain-http hosts.

## ⚠️ BREAKING CHANGES
* **Release builds default agent images to their own version** (#4660): When `image_tag` is unset, a release build (`vX.Y.Z`, `-preview.N` or `-rc.N`) now uses its own version as the tag of the bundled `scion-*` agent images instead of `:latest`, so a Homebrew install pulls the images published for its release. Development, nightly and other builds keep `:latest`. Any set `image_tag` wins, and `image_tag: latest` opts out. The default is gated by `hub.image_tag_version_default`, which is on by default.

## 🚀 Features
* **Cloud Run native identity decided by the Hub (experimental)** (#4663): With `hub.cloudrun_native_identity` on, the Hub decides for each Cloud Run dispatch whether the instance runs as a native identity, records the decision on the agent and sends it to the Runtime Broker. The Hub refuses a dispatch it can't decide and withholds GCP access tokens while a native identity is set. The Runtime Broker now runs block mode as the configured block account, and refuses passthrough and unsupported combinations.
* **Legacy Runtime Broker conversion plan in the web UI (experimental)** (#4656): With `hub.flat_runtime_brokers` on, a legacy Runtime Broker's Overview tab shows a read-only plan for moving to flat Runtime Brokers: instances, the profile map, findings and capacity, plus the bound agents and projects.

## 🔒 Security
* **Upstream response text sanitized in errors and logs** (#4679): Text from OAuth and OIDC endpoints, the Teams token endpoint and Google credential validation reaches errors and log lines only through a 512-byte capped wrapper that replaces control characters, line separators and invalid UTF-8.
* **Caller-supplied chat keys bounded in logs** (#4674): When a chat store lookup fails, the topic key is logged at most 128 bytes long, with its full length recorded separately, so a caller can't control the size of a log entry.
* **Content host can't share the Hub's site** (#4661): A `server.hub.content_url` on the Hub's registrable domain is refused on save and ignored at runtime unless `server.hub.content_url_allow_same_site` is set. The server config response and page report `content_url_status`.

## 🐛 Fixes
* **Kubernetes follows the Docker environment order** (#4676): On Kubernetes, an agent container's environment sources are applied in the same order as on Docker: resolved auth, the harness and telemetry environment, config environment, then runtime keys. The later source wins for a name set more than once.
* **Chat copy actions work on plain-http hosts** (#4664): Copy text, Copy link to msg and Copy link address now show a toast and no longer throw where the Clipboard API is unavailable.
* **Canceled requests stop per-item checks** (#4668): When a client gives up mid-request, the remaining per-item permission checks stop and deny instead of each logging a context-canceled warning.
* **Conversation consistency check bounded** (#4666): The diagnostic check runs off the message path with a 200 ms deadline and fails open, and two new indexes speed up its lookups.
