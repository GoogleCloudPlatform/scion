# Release Notes (2026-11-10)

This period made agent-written user secrets wait for owner approval, stopped the Hub from cloning over SSH, and folded artifact review into the grant ladder. It also bound Hub JWTs and Conduit sessions to the user's session generation, added experimental multiple-choice decisions and project self-leave, and kept chat search and update snapshots from stalling the Hub.

## ⚠️ BREAKING CHANGES
* **Agent-written user secrets wait for owner approval** (#4747, #4751, #4754): A user-scoped secret that an agent saves is held as pending and isn't delivered to any agent until the owning user approves it. Owners list, approve or reject pending secrets with `scion hub secret pending`, or from Profile > Secrets in the web UI, which shows each secret's state and the agent and project that saved it. Pending secrets are limited to 10 per agent and 50 per owner and expire after 7 days. Owners are notified, and every action is audited.
* **Hub-side git refuses SSH repository URLs** (#4739): Clones and fetches run by the Hub on its own host refuse an SSH or scp-style repository URL with 422 before any git work, with or without a project git credential. Use an HTTPS repository URL. A project git secret authenticates HTTPS only.
* **Review joins the artifact grant ladder** (#4745): Artifact grant levels are now read, review, write and admin, each including the ones before it, and the separate review-only grant path is removed. Existing review grants now also give read access; list them with `scion artifact grants <ref>` and remove any you don't want. The downgrade floor is `nightly-20261112`.

## 🚀 Features
* **Multiple-choice decisions for agents (experimental)** (#4726, #4743): Behind `web.chat_choices`, agents can send one or more multiple-choice decisions with `scion message --choices` or `--choices-file`, and update them with `--update`. Each answer becomes a Hub-minted `choice-response` message to the agent and a row in a permanent decision log, and a prompt with several decisions is answered as one set. Sending choices puts the agent in waiting-for-input. Chat surfaces without native cards get a Markdown fallback with an answer link.
* **Leave a project from the CLI and web UI (experimental)** (#4732, #4731): With `hub.project_self_leave` on, `scion project leave <project>` shows what leaving changes, settles what happens to the caller's agents and reports every outcome. The web UI offers the same.
* **Legacy Runtime Broker conversion on co-located hosts (experimental)** (#4738): `scion runtime-broker convert --apply` and `--rollback` now work on a co-located host, where the server runs the Hub and an embedded Runtime Broker, in stages around a server restart.
* **Welcome project (experimental)** (#4720): Behind `hub.welcome_project`, a workstation setup wizard step installs the agent-team templates into a welcome project, downloaded and verified by commit sha. Given credentials, it starts a coordinator that greets its owner once.

## 🔒 Security
* **Session generation in Hub JWTs and Conduit** (#4741, #4752): Hub user JWTs carry the user's session generation, which is checked on validation. Conduit sessions and streams authenticated with a Hub user token keep the generation they were admitted with and compare it on every re-check, so revoking a user's sessions also ends these.
* **Runtime Broker errors return generic text** (#4742): API clients see a Runtime Broker failure as its status and error code. The Runtime Broker's own text is logged once on the Hub, capped and sanitized, and only allow-listed details are relayed.
* **Workspace file paths walked without following links** (#4740): Workspace file writes, downloads, walks and deletes open each parent directory in turn without following symbolic links, check `.git` on the opened directory, and give each refusal a named reason.
* **Per-peer keys for relay peers (opt-in)** (#4412): Relay peers can be enrolled with their own Ed25519 keys in `server.hub.conduit.peers`, with a private key in `server.hub.conduit.peer_key_file`. A signature verifies only with a key enrolled for the peer it names.
* **Project-default GCP passthrough uses the shared checks** (#4721): A project-default GCP passthrough identity now goes through the same authorization and local-runtime checks as explicit and Hub-default identities.

## 🐛 Fixes
* **Podman runs require a full container ID** (#4672): A `podman run` whose output isn't a full 64-character container ID now fails the run and removes the container it may have created, so it isn't left running unaddressed. *(Contributor: Stephen Ierodiaconou)*
* **Chat search can't stall the Hub** (#4735): Web chat search runs on a separate read-only database handle under a time limit (503 `SEARCH_TIMEOUT`) and Hub-wide and per-user caps (429 `SEARCH_BUSY`).
* **Pre-update snapshot no longer blocks requests** (#4748): The database snapshot before an update uses its own read-only connection, so the Hub keeps serving requests while it's written.
* **A2A bridge replies** (#4746, #4753): The A2A bridge reads an agent's reply from the caller's own conversation, so calls no longer time out in plugin and standalone mode. With `reply_poll.enabled`, a federated user's reply is read from that user's own conversation.
* **Held agents get no Hub notices** (#4749): While an agent is on hold, the Hub delivers no notices to it, and notices skipped during the hold aren't delivered later.
* **Agent runtime fixes** (#4737, #4695, #4675): muse-code agents fire their hooks again, because the handlers no longer carry a key muse rejects. Kubernetes keeps the runtime's start command, as Docker does. Flat Runtime Broker agents whose container is gone after a start are reconciled.
* **Combined server logs each request once** (#4750): The combined Hub and web server no longer logs every request twice.
