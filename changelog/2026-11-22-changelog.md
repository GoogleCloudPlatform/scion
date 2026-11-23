# Release Notes (2026-11-22)

This day separated starting an agent from resuming one, added experimental agent-scoped secrets and execution containers, and kept hardening credential and proxy authentication. It also limited what hosted Hubs fetch on import.

## ⚠️ BREAKING CHANGES
* **`scion start` no longer resumes an existing agent** (#5194): Starting or creating an agent whose name already belongs to a suspended, stopped or halted agent is refused with 409 `agent_exists_resumable`. Run `scion resume <agent>` instead, delete the agent, or pick another name. Agents still in the created or provisioning phase, such as after `scion create`, or with a launch already in flight, are still started or followed. Status messages for preempted or evicted Kubernetes agents now point to `scion resume`.
* **Hosted Hubs restrict import sources** (#5286): Imports, discovery and harness-config reimports on a hosted Hub accept only `https://github.com/...` folder and archive URLs by default. Each fetch is bounded: at most 64 MiB and 2 minutes to download, 512 MiB unpacked, and 256 MiB or 20,000 files written.
* **No worktree-per-agent on NFS project storage** (#5354): Docker, Podman and Apple container brokers refuse `worktree-per-agent` workspaces on shared (`nfs`) project storage, as Kubernetes already did, because each agent needs a private `.git/config`.
* **Stricter proxy authentication** (#5312): Proxy authentication checks more at startup and tightens host matching and JWT identity headers. `--enable-test-login` is refused in proxy auth mode, even on a test Hub. Extra proxy user headers listed in `identity_headers` are stripped from agent port-proxy requests.

## 🚀 Features
* **Agent-scoped secrets (experimental)** (#5324, #5352): Behind `hub.agent_secrets`, secrets can be scoped to a single agent (`scion hub secret ... --agent <name>`). Humans can write them and read their metadata, but never their values. An agent can write its own agent-scoped secrets.
* **Execution containers (experimental)** (#5348): Behind `hub.exec_containers`, an agent can run the reserved built-in `shell` harness with no LLM. Users sign in to CLI tools inside it by hand, so those credentials stay in the container's home directory and are never exposed to LLM agents. Users can register one in their profile.
* **Recorded decisions keep their choices** (#5311): The decision log keeps each decision's offered choices and recommended choice, and `--made --json` output includes them.
* **Agent provenance in the CLI** (#5327): `scion whoami` and `scion list` show each agent's image, generation, harness and model.
* **Join conversations by name** (#5335): Adding a participant to a conversation (`scion conversation` and the API) accepts a user's email or handle, or an agent's name.
* **Code-span mentions (experimental)** (#5300): Behind `hub.code_span_mentions`, an inline code span holding just a mention, such as `` `@coordinator` ``, notifies like a plain mention. Mentions of `@scope/name` packages create less noise.
* **More GitHub triggers and other experimental work** (#5338, #5350, #5346, #5292, #5329, #5303): GitHub triggers (`hub.github_triggers`) handle push events and can author agent dispatches from the CLI. Artifact review drafts can be edited in the review editor. The admin branding page has a contrast check and live preview. System One supports a Gemini decisions provider.

## 🔒 Security
* **Delegation standing enforced on every credential route** (#5349, #5340, #5353): Agent delegation standing is enforced structurally across credential routes. Credential and delegation revocations at suspend recover from failures instead of being skipped. User-scope secrets shared with progeny are authorized directly through `secret.use`.
* **Notification events scoped per user** (#5305): The notification event stream sends each user only their own events.

## 🐛 Fixes
* **Reincarnate fixes** (#5301, #5330): Reincarnate pins the harness config, compares images rewritten by the broker, and shows the reasons for its plan. The reincarnation preamble defers its last step to the handoff.
* **GCP tokens and telemetry** (#5293, #5302): GCP ID tokens are cached, with backoff on 429. Telemetry export stops after a permanent authentication failure and warns when GCP credentials are missing.
* **CLI fixes** (#5325, #5328, #5326): `scion notifications` and `scion messages` reject stray positional arguments. Deleting a one-shot scheduled event suggests `scion schedule cancel`.
* **Substrate start command** (#5341): The substrate start command is built before the actor is created. *(Contributor: aalzo)*
* **Chat mention menu** (#5074): Right-clicking a highlighted mention in chat opens that agent's menu.
