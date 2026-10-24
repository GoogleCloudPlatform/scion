# Release Notes (2026-10-22)

The Hub's WebDAV endpoint and project-level `scion sync` were removed, and Runtime Brokers now trust only their own records and the Hub's start request for an agent's profile, harness config and runtime. Agents can send and receive message attachments as files, the agent Edit page replaced Configure, and admins can batch-invite users with project roles. Credential handling was tightened across the API client, dev sign-in and agent responses.

## ⚠️ BREAKING CHANGES
* **WebDAV and project-level `scion sync` removed** (#3989): Requests to `/dav/` now get a 404. Bare `scion sync`, push, pull and `--force` at project level exit with a usage error pointing to git or the workspace file API. Agent sync is unchanged.
* **Start inputs come only from the Runtime Broker and Hub** (#4029): The Runtime Broker takes an agent's profile, harness config and runtime from its own image-provenance record and the Hub's start and restart requests, never from files in the agent home. An older agent with neither a record nor a Hub-sent profile is refused with a re-provision error.
* **Read-only project configuration in shared workspaces** (#4045, #4037, #4064): In shared-workspace and related layouts, the project's `.scion` configuration is mounted read-only into agents, and the Runtime Broker refuses later changes to the recorded state. The mount layout is derived only from paths the Runtime Broker knows, and a git common directory outside the project is refused. Docker and Podman always mask `.scion/agents` in plain workspaces.
* **Web base URL from operator inputs only** (#4014): The OAuth redirect base, cookie Secure flag and browser config resolve once at startup from `--base-url`, `SCION_SERVER_BASE_URL`, `SCION_SERVER_HUB_ENDPOINT` or `server.hub.public_url` in the global settings file, never from the settings database. An `http` base URL on a non-loopback listener is refused.
* **Dev credentials only to loopback Hubs** (#4035): An explicit `SCION_DEV_TOKEN` or `SCION_DEV_TOKEN_FILE` is not sent to a non-local Hub; the CLI prints one note naming the Hub instead.
* **`scion doctor --format json` v2** (#4056): The JSON report moves to `formatVersion` 2, with `generatedAt`, a Hub block (endpoint, version, instance, credential kind) and a `platformHealth` state with stable reason codes.
* **Thinking-level and federation settings validation** (#4018): A project `defaultThinkingLevel` must be 1-100 (`null` clears); 0 or out-of-range returns 422, and a stored 0 reads as inherit. A `null` federation value is refused.

## 🚀 Features
* **Agent attachments as files** (#3937): Agents send attachments with `scion message --attach` and receive them as files in `~/.scion/inbox` before the message text arrives, through a single-use, per-recipient delivery capability checked by the receiving Runtime Broker.
* **Agent Edit page covers everything** (#4008): The Edit page now has every former Configure field, phase-aware Save & Start and Save & Resume actions, a mutability indicator on every field, and grouping by function or tier. `/agents/{id}/configure` redirects to Edit.
* **Batch invites** (#3986): New `scion hub users invite` invites addresses from arguments, a file or stdin, with an optional note and repeatable `--project PROJECT=ROLE`. The bulk invite API reports one result per address and can apply project roles, which require `user.read`.
* **Provision-time files refresh at start** (#4011): When the system prompt, agent instructions, skills or services change, the Hub tells supporting Runtime Brokers to re-render those files at the next start, once per change.
* **CLI and port proxy usability** (#4025, #4040, #4033, #3975): `scion ssh` forwards SIGHUP and SIGTERM; tunnel, ssh and ssh-config honor `--project` with `--global`; tunnel status lists open local listeners. The first successful `scion hub auth login` makes that Hub current. The bare port proxy URL redirects to its trailing-slash form, and failed page sub-requests get a plain-text explanation. A new `scion-web-serving` platform skill guides agents serving pages through `scion expose`.
* **Settings correctness** (#4032, #3987): Operational settings applies are ordered by a stored revision, and the save report lists `hub_name` only when the effective name changes. A create that names the Runtime Broker's global settings directory is refused before any files are written.
* **Web client** (#4065, #3968, #4012, #4006): Experiments ship in the boot payload, saving a request. Default subscription triggers match the Hub's agent-create set. Behind `hub.artifacts`, the Artifacts page gains New artifact with a project picker, plus Owner deleted badges, expiry dates, custodian Delete and a Retention view.
* **Experimental web options** (#3973, #3995): `web.agent_badges` shows harness vendor and model size badges on agents. `web.sse_hidden_tab_close` closes streams in tabs hidden for 60 seconds; the first reconnect is now jittered for everyone.
* **Async start, experimental** (#4015): With `hub.async_agent_start` and `hub.async_agent_launch` on, start, resume and restart of an existing agent return as soon as the Runtime Broker accepts, once every Hub instance supports it.
* **Groundwork** (#4017, #4019, #4039, #4067, #4009, #3963): Delegated project read and an `agent_delegation_policy` setting. A versioned `scion-analytics` event envelope. Browser event-stream metrics. Persisted boundary update history and catalog envelopes for Runtime Broker lifecycle audit events. A one-time maintenance migration that marks legacy agent rows in group conversations as left.

## 🔒 Security
* **Hub credentials stay on the Hub origin** (#3996): The API client applies Hub and transport credentials only to requests whose scheme, host and port match the Hub, and strips `Authorization`, proxy credential and `X-Scion-*` headers on cross-origin redirects.
* **Agent home confinement** (#4024): Every Runtime Broker start-path write and removal under the agent home goes through one handle that stays inside the home and never follows links.
* **More redaction** (#4016, #4043, #4068, #4034): Agent responses hide MCP server env and headers and service env from callers without attach. Log text, clone URLs, tunnel endpoints and template sources share one URL user-info rule. Skills discovery passes through only supported query parameters and logs URIs without queries.
* **Narrower schedule and group authority** (#4048, #3962): Agent callers list and manage only the schedules they hold authority for and cannot edit schedule definitions. Removing members from, or deleting, a group that is the subject of an access constraint requires an interactive session.

## 🐛 Fixes
* **Shared dirs on NFS Hubs** (#4074): On a Hub with NFS workspace storage, web uploads to shared dirs land in the project's tree on the share, so agents see them and they survive instance recycling.
* **Executable bits preserved** (#4028): Hub-hosted templates and harness configs keep the executable bit, so Agent Plugins `./bin` MCP servers and provision scripts run.
* **Cloud Run phases** (#4013, #4007): Running Cloud Run instances report running and are reused instead of recreated, and stopped instances report stopped. The cloudrun-sandbox runtime no longer mistakes generic errors for a gone sandbox.
* **Reincarnation and schedules** (#4010, #4004): A failed reincarnation restores the replaced service-account assignment, and agent delete cancels scheduled events only while they are still pending.
* **Agent status and skills** (#4001, #4000): `sciontool` hooks keep terminal status and sub-agent waits and count failed or interrupted tools correctly. Agent starts stop logging a skip warning per platform skill.
* **Chat** (#4022): The conversation panel survives a switch to the mobile layout, and message links are built over text nodes so attribute contents are not linkified.
* **Upload cleanup** (#3977): Every upload handler removes parsed multipart temporary files on all return paths.

## 🔧 CI & Infrastructure
* **CI on Cloud Run Jobs** (#3943, #4046, #4061): A new `cr-ci/` area runs GitHub Actions jobs on Cloud Run Jobs through a mediator service, with fixes from the live trial. golangci-lint is built with a patch for the `unused` analyzer's slowdown on large packages.

## 📖 Docs
* **Tunnel runbook** (#4042): Uses the in-agent `scion expose` and `scion unexpose` commands.
