# Release Notes (2026-11-13)

This day filled out flat Runtime Broker support, with web and CLI placement, legacy Cloud Run profile conversion and Cloud Run shared directories, and carried the experimental multiple-choice decisions through the CLI and web. It also split global skill writes into their own permissions and tightened several Cloud Run, scheduler and delete paths.

## ⚠️ BREAKING CHANGES
* **Global skill writes need new permissions** (#4796): Updating or deleting a skill in the global catalog (publishing or deprecating versions, uploading files, writing files) now requires `skill.update_global` or `skill.delete_global`, no longer the plain `skill.update` and `skill.delete`. Project and user skills are unchanged. The `global-catalog-author`, `hub-admin` and `super-admin` roles carry the new permissions, and their seeded revisions are bumped. A custom role that relied on `skill.update` or `skill.delete` for global skills must be granted the `_global` permissions. A global skill's creator who no longer holds the author role loses write access to it.

## 🚀 Features
* **Flat Runtime Broker placement in the web UI and CLI** (#4800, #4801, #4824): The agent-create page can place an agent on a flat broker instance. `scion hub brokers` shows each broker's target, and the admin server-config page edits `server.broker.instances`. Cloud Run flat targets are presented consistently, and a co-located flat instance that the server refuses to run is marked offline. A saved Kubernetes target identity is guarded against drifting.
* **Convert legacy Cloud Run profiles to flat instances** (#4798, #4806): `scion runtime-broker convert` plans now translate legacy Cloud Run profiles into flat instances, and plan JSON reports a `hostMode`. A co-located server refuses to activate a flat instance (`instance_in_use`) when another live server process holds that instance in a shared Hub database, before any broker secret is rotated.
* **Project shared directories on Cloud Run Instances** (#4823): The `cloudrun` runtime mounts a project's shared-directory NFS leaves into agents.
* **Multiple-choice decisions from the CLI, with confirm and change in chat (experimental)** (#4802, #4799, #4807): `scion decisions list`, `show` and `record` let an agent list its decisions, read one and record an answer it interpreted from someone's reply. Choice-response messages now name who answered and what the outcome was. Single and multi-decision web cards ask to confirm an answer, and let you change it.
* **`reincarnate --harness` on Kubernetes (experimental)** (#4793): The Agent home reset used by `reincarnate --harness` now also works on Kubernetes runtimes.
* **Wake for chat scheduled sends, and clearer schedule detail** (#4827, #4815, #4819, #4817): Chat scheduled sends can opt in to waking a suspended agent. CLI and web schedule detail show the wake and interrupt settings. A scheduled message to a suspended agent now says the agent is suspended and suggests `--wake`, and scheduler wake handles shutdown edge cases.
* **Reply all in web chat** (#4816): Chat has a Reply all action that starts a reply with `@`-mentions pre-filled for the other participants mentioned in the message, leaving out you and the sender.
* **Managed-agent creation is selected with a flag, not a profile** (#4818): The Hub now reads managed-agent creation from `managedAgent` on the create request. The legacy `managed-agents` profile value is still accepted at the create entry and converted, and the request no longer reaches broker or profile resolution as a profile.
* **Antigravity image installs the latest CLI** (#4820): The Antigravity harness image installs `agy` with the upstream installer at build time instead of a pinned `AGY_VERSION`. The installed version is printed in the build log, and published images stay pinned by registry digest.

## 🐛 Fixes
* **Cloud Run file secrets** (#4822): The read-only `~/.scion/secrets` mount is removed from Cloud Run agents, and a file-secret target that can't be written is now reported.
* **Cloud Run refuses worktree-per-agent workspaces** (#4830): Creating such an agent on Cloud Run now fails with 412 before anything is created, instead of failing partway.
* **Delete and rollback** (#4808): Delete conflicts caused by placement now give a hint, a rollback restores the agent's run intent, and the broker's `storageKept` is reported.
* **Chat unread dots for wrapped mentions** (#4821): Unread mention markers are recorded for mentions that wrap across lines and for mentions sent through a broker.
* **`--no-block` restart** (#4795): A SIGTERM during a `--no-block` restart counts as accepted, and the detached-HEAD hint is corrected.
