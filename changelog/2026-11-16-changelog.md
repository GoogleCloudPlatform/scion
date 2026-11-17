# Release Notes (2026-11-16)

This day brought GCP identity to flat Cloud Run Runtime Brokers, added recurring exec schedules with a CLI and web UI, and merged the Decisions experiments into one flag. It also tightened agent access to project secret metadata.

## ⚠️ BREAKING CHANGES
* **Decisions use a single `web.decisions` experiment** (#4962): `web.chat_choices` and `web.project_decisions` are retired and replaced by `web.decisions`, which gates choice prompts, the chat cards and the project and profile Decisions pages together. A Hub that had the old flags on must enable `web.decisions` to keep Decisions. While it is off, choice-prompt sends are refused with `400 experiment_disabled` and decision routes answer `404`. Answers stay in the decision log after their user is deleted, with the recorded name and email kept.

## 🚀 Features
* **GCP identities on flat Cloud Run Runtime Brokers** (#4940, #4944, #4941, #4946, #4948): A flat Cloud Run instance has a default identity path, and the Hub decides native Cloud Run handling from the target type. Agents can be assigned a service account in `assign` mode when it is on the instance's `assignable_service_accounts` list, and unlisted accounts are refused with 400. Passthrough runs agents as the instance's own service account, and an identity change recreates the agent's Cloud Run instance on its next start or restart. Behind `hub.flat_runtime_brokers`, a project can set a default GCP service account per broker from the CLI or the web UI, which shows the effective identity an agent will get. `scion runtime-broker convert` carries `cloudrun.block_service_account` and `cloudrun.assignable_service_accounts` from legacy profiles into the flat instances.
* **Exec schedules from the CLI and web UI, one-shot and recurring (experimental)** (#4938, #4957, #4961, #4964): With `hub.exec_schedules` (and `hub.conduit`) on, `scion schedule create --type exec` and `create-recurring --type exec` run a command in an agent's container at a set time over its Conduit session, with no message and no model turn. The project's schedule forms offer the `exec` type with an agent picker, per-argument inputs, an optional timeout, and run history. Scheduled event lists are paginated.
* **System One administration (experimental)** (#4934): Behind `hub.system_one`, Hub admins manage System One provider instances with `scion admin system-one instance` and the matching admin API.
* **Deliver-message waits for the agent session (experimental)** (#4935): Behind `hub.conduit_deliver`, a message that arrives before the agent's Conduit session exists is held and delivered once the session connects, instead of failing.
* **ffmpeg and Playwright in agent images** (#4937): Agent images include the system `ffmpeg`, and `sciontool init` registers it in Playwright's browser registry in the persistent agent home, so Playwright uses it without downloading its own.

## 🔒 Security
* **Project secret metadata needs `project:secret:read`** (#4947, #4958): An agent must hold `project:secret:read` to read project secret metadata. Project environment reads leave out environment-secret metadata for agents without it. Every scope in the stock agent roles is now checked to map to an enforced permission.

## 🐛 Fixes
* **`@email` mentions highlight only real users** (#4933): In chat, an `@<email>` is highlighted and resolved to the member's display name only when it matches a known Scion user in the project.
