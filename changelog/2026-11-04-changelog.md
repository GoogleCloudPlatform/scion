# Release Notes (2026-11-04)

This period added `scion project settings` and `scion artifact comments`, began work on Cloud Run as a flat Runtime Broker target, and gave delegating users access to their agents' new artifacts. It also made Cloud Run agents start reliably after a stop, fixed Vertex AI settings for Claude agents, and fixed a starter Hub crash loop on first start.

## 🚀 Features
* **`scion project settings`** (#4569, #4577): The `scion project settings get`, `set` and `unset` commands manage the defaults the Hub applies to agents created in a project: template, harness config and auth, model, thinking level, active profile, limits, resources, agent roles, telemetry, auto-exposed ports and the default GCP identity. `scion hub token --help` now lists the scopes the settings commands need.
* **`scion artifact comments`** (#4563): Lists a review's comments, suggested changes and highlights as structured entries, so agents and scripts don't need to parse CriticMarkup. A bare `<id>` (or `scion://artifact/<id>`) reads the newest review, and `<id>@<seq>` names a specific review version.
* **Delegating users get access to their agents' artifacts** (#4571, #4574): When an agent with a recorded delegating user creates an artifact, the Hub grants that user marks-only access to it, provided the user is active and can read the project. Behind `web.artifact_manage_access`, stewards of agent-owned artifacts can also create share links from the Manage access dialog.
* **Cloud Run as a flat Runtime Broker target (experimental)** (#4580, #4581): `cloudrun` is accepted as a `runtime_target` type for flat Runtime Broker instances, with settings validation, Cloud Run identity and ownership. It is gated behind `hub.flat_cloudrun_target`, and no Cloud Run agent can start from a flat Runtime Broker yet.
* **More settings on the Server Config page** (#4567): The maintenance message can be edited and the agent start timing fields are on the Server Config page. Project settings label the Hub value as a fallback.

## 🔒 Security
* **Secret store encryption-policy fence** (#4573): Under an encryption policy, a write that would leave a secret row with neither a value nor a secret reference is refused, including metadata-only updates of older rows.
* **Cloud Run SSH keys signed for the instance's service account** (#4564, #4579): Each exec looks up the Cloud Run instance and names its service account in the OS Login key signing request, and fails if the service account can't be determined. Signed keys are reused only for the same instance and service account, and launch cleanup drops the cached connection key.

## 🐛 Fixes
* **Cloud Run agents start reliably after a stop** (#4578): A start acts only on its own run's instance. A previous run's stopped or soft-deleted instance no longer fails the start or gets deleted, and an existing instance is reused only when it matches.
* **Vertex AI settings for Claude agents** (#4558, #4566): The Claude provisioner writes real Vertex AI project and region values into the environment overlay instead of `${NAME}` placeholders. On Cloud Run, a Claude agent using Vertex AI now receives the resolved project and region values, applied with the same rules as on Docker.
* **Starter Hub no longer crash-loops on first start** (#4585): A full starter deploy writes a default `image_registry` of `localhost/scion`, which `IMAGE_REGISTRY` or `SCION_IMAGE_REGISTRY` can override.
* **Copilot agents without credentials** (#4584): In no-auth mode, Copilot starts with no session bus, so it can't reach a keyring, and the sign-in is captured from its config file.
* **Chat attachments from agents without artifact access** (#4583): An agent whose token lacks `project:artifact:read` now gets the existing 404 `attachment_artifacts_unavailable` and falls back to the legacy attachment path, instead of a 500.
* **Restart no longer briefly shows an agent as stopped** (#4575): Status reports from the run being replaced during a restart, and self-reports that name no run, no longer change the agent's phase.
* **Agent time zone read errors** (#4570): A failed read of a stored time zone setting no longer falls through to a lower scope or the Hub default. Agent create, start, restart and edit requests return 503 instead.
* **Empty template name means the default template** (#4587): Every template lookup now resolves an empty name as the default template, and none resolves a templates directory as a template.
* **Cloud Logging health is less noisy** (#4586): The `cloud_logging` health check reports degraded only on a sustained write problem, not on one failed write.
