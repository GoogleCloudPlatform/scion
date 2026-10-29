# Release Notes (2026-10-27)

This period tightened Hub authorization along recorded delegation chains (secrets, service-account tokens, hook tokens), moved agent and user JWTs onto the keyring signing ring, and continued the project storage rework. Copilot agents start again on current CLI releases, chat starred messages expanded (experimental), and Runtime Broker version mismatches are now visible.

## ⚠️ BREAKING CHANGES
* **Service-account token mint follows the recorded parent ceiling** (#4231): the agent GCP identity and access token endpoints and the GCS link proxy now check `gcp_service_account.use` against the service account's recorded parent authority before minting. Agents created before assignment rows were recorded lost their GCP tokens on upgrade with `ceiling_provenance_missing`. A follow-up boot migration (marker `sa_assignment_legacy_backfill`) backfills the missing rows from each agent's recorded delegation edge. Agents whose edge cannot back a row are skipped with a warning and stay denied.
* **Removed storage overrides refused at agent start** (#4242): profiles that still set `shared_dir_storage_backend(s)`, `home_storage_backend` or `home_storage_leaf` are now refused with 422 at agent start. Previously they were silently ignored and left Kubernetes claims Pending. Move these settings into `server.project_storage`.
* **Runtime Broker refuses replayed and nonce-less signed requests** (#4227): each signed Hub request is served once, and a repeat within the clock-skew window gets 401 "request already served". Signed requests must carry a nonce; every in-tree signer already sends one.
* **Keyring-signed JWTs** (#4243): on Hubs with a server keyring file, agent and user JWTs (access, refresh, CLI and test-login) are signed by the active generation and carry its key ID. A token is accepted only by the generation that signed it, unknown key IDs are refused with no fallback, and the Hub refuses to start if the keyring is unusable. Hubs without a keyring file are unchanged.

## 🚀 Features
* **Chat starred messages expand (experimental)** (#4226, #4239, #4241): behind `web.chat_starred_messages`, stars now work in direct messages. A `?messages=starred` view shows only your starred messages, with a jump to full context, and the chat rail gains a Starred filter. Stars follow a conversation when it becomes a topic and are cleaned up when messages or users are deleted.
* **Runtime Broker version mismatch flags** (#4186): Runtime Broker pages and the health dashboard show each Runtime Broker's version next to the Hub's and flag mismatches. The list and get APIs add `versionStatus`.
* **Project storage phase 3b** (#4219): on shared backends, the Runtime Broker reads a Hub-managed project's configuration from the project root. Agent homes on Kubernetes and Cloud Run share one path shape, and stopped Cloud Run agents' home and secret paths migrate automatically. Workstation paths are unchanged.
* **Review editor improvements (experimental)** (#4254, #4236): the Markdown review editor opens large documents about ten times faster, with Source view as the fallback for very large files. It now takes suggestions and comments in list items and table cells while keeping the original Markdown formatting.
* **Return to the requested page after sign-in** (#4228): with a web OAuth provider configured, signing in lands on the page originally requested, limited to same-site paths.

## 🔒 Security
* **User-scope secrets follow the recorded delegation chain** (#4229, #4234): agent reads and lists of user-scope secrets, and start and restart secret delivery, apply the user-scope parent ceiling along the agent's recorded delegation chain.
* **Hook token mint re-checks the writer** (#4230): before a lifecycle hook mints an access token, the Hub confirms that the user who wrote the hook still exists, is active and may still use the service account.
* **Agents start without a Hub token when none was issued** (#4097): the Runtime Broker passes an agent only its Hub-issued token and removes any stale token file. The start is refused if the file cannot be removed.
* **Private git config copies** (#4238): the copy policy now also strips client TLS key and certificate paths and other credential-bearing keys.
* **Download hashes verified before writing** (#4133): transfer downloads verify hashes before any file is written. *(Contributor: Ravshanbek Nazarov)*
* **Kubernetes file placement** (#4221): each created path component is re-verified after `mkdir` and refused if it is a symlink or not a directory.

## 🐛 Fixes
* **Copilot agents start on current Copilot CLI** (#4217, #4218): the `--no-remote` flag, removed in Copilot CLI 1.0.103, is dropped; remote sessions stay off through settings instead. Copilot readiness uses a settle delay, so agents started without a task no longer wait 90s for their first message.
* **Launch failures after running** (#4223): a failed launch report that arrives after the agent reported running now ends the launch as failed and cleans up, instead of being answered as completed.
* **Kubernetes start status** (#4222, #4220): copied-home runtimes stay in `starting` until Run returns. Preempted or evicted pods give a start error that names the Kubernetes reason.
* **Attachment-only chat messages** (#4225): sending attachments with no text is stored instead of failing.
* **Linked project template imports** (#4151): template imports resolve from the checkout root. *(Contributor: Matt Van Horn)*
* **Client response handling** (#4049, #4125, #4138, #4168, #4170): the API client rejects non-2xx responses, transfers honor canceled contexts before file I/O, manifest options apply on every build, and unrecognized upload and download responses are rejected. *(Contributor: Ravshanbek Nazarov)*

## 🔧 CI & Infrastructure
* **GitHub Actions off Node 20** (#4214): every Node 20 action moves to a Node 24 release, pinned by full SHA.
* **cr-ci runner guards** (#4204, #4235, #4252): runners accept only `push` and same-repository `pull_request` runs, and a pinned job-started hook allowlists repositories. The runner image builds git without Rust. Mediator validation and deploy order are fixed.
