# Release Notes (2026-10-24)

This period moved agent-bound delegation forward on both the Hub and agent sides, tightened Hub-managed agent deletes and Runtime Broker records, and added a weekly preview release train. Chat gained private starred messages (experimental) and a fix for the unread divider.

## ⚠️ BREAKING CHANGES
* **Metadata `block` mode refused under host networking on cloud hosts** (#4124): with Docker or Podman host networking, metadata `block` mode now follows the Runtime Broker setting `SCION_HOST_NETWORK_BLOCK_POLICY`. When unset or `auto`, the start is refused on a detected cloud host (DMI vendor, cloud-init, or GCE guest agent files). `strict` always refuses, `allow` starts and logs a warning, and an unrecognized value counts as `strict`. Operators who relied on host networking with `block` mode on cloud VMs must set `allow` or change network mode.

## 🚀 Features
* **Agent-bound delegation** (#4167, #4166, #4169): `sciontool` can exchange delegated credentials. It keeps them in memory only and serves them through a peer-checked `~/.scion/delegation.sock` proxy that re-exchanges before expiry. On the Hub, deleting, restoring, suspending or reincarnating an agent, its issuer, or its project now revokes its grants and credentials, and messages record delegated attribution. The Hub delivery credential now goes through a recorded-chain admission check, replacing the old hard deny. Groundwork: no production caller uses the delivery identity yet.
* **Weekly preview releases** (#4123, #4126): a Friday workflow fast-forwards the newest `release/vX.Y` to the latest green nightly and tags the next `vX.Y.0-preview.N` with a non-force push. It skips weeks with no new nightly. The bump and promote scripts now order tags by patch version, then by preview number.
* **Chat starred messages (experimental)** (#4149): behind `web.chat_starred_messages`, users can star project thread messages. Stars are private, and each conversation header lists them with a jump to each message.
* **gh:// skills keep file modes and subdirectories** (#4150): skills are listed from the git tree, so executable bits survive (modes are normalized to 0755 or 0644) and files in subdirectories are included. Symlinks are skipped. Submodules, truncated trees, unsafe paths and skills over 1000 files fail clearly.
* **`scion start` and `scion resume` name the environment values they send** (#4148): when values go to the Hub without the interactive prompt, the CLI prints the sorted key names (never values) on stderr.
* **`scion templates sync` and `push` accept a directory path** (#4146): the Hub name defaults to the directory name, and `--name` overrides it.
* **Admin Server Config shows values in effect** (#4165): for keys overridden by `SCION_SERVER_*` environment variables, the page now shows the environment value the Hub is actually using.

## 🔒 Security
* **Uploads send only verified bytes** (#4143): template, harness-config, skills and workspace uploads read each file through the directory it was collected from. They send only bytes that match the size and hash recorded at collection, and a changed file fails the upload before it is finalized.
* **Template cache clones use the scoped credential helper** (#4144): remote GitHub template imports authenticate the same way provisioning clones do, with plain https URLs and host-side git.

## 🐛 Fixes
* **Hub-managed agent delete cleanup** (#4020, #4058, #4158, #4159):
  * Deleting a hosted agent now removes its sharer registry entry and, for the last sharer, the shared worktree, in order.
  * A delete where the Runtime Broker holds a different run than the Hub recorded now deletes that run and then the agent. This always happens under force; without force, only when the run is one of the agent's own previous runs.
  * A forced move's rollback now checks markers by the same agent ID and Hub project keys the delete uses.
* **Runtime Broker records** (#4163): a heartbeat no longer overwrites a concurrent re-registration's profile set. Deleting a Runtime Broker releases its reservations immediately. New projects link every auto-provide Runtime Broker, not just the newest 50.
* **Single-node VM fresh deploys** (#4172): fresh `deploy.sh` installs now work again. The bootstrap Hub start binds to loopback until the https base URL is written.
* **Chat participants and DM promotion** (#4161):
  * Soft-deleted agents can no longer be added to groups.
  * Promoting a DM whose agent is not live is refused, and promotion stores the agent slug as the thread default.
  * The unread count reads only spaces the user belongs to.
* **Chat unread divider** (#4135): opening a conversation with unread messages lands at the New messages divider instead of the bottom.
* **Cloud Run sandbox diagnostics** (#4059): sandboxes restored at Runtime Broker start get exit watchers. Failed execs log a warning, and run failures name the requested image.
* **Configuration papercuts** (#4141):
  * A settings YAML decoder panic is now a parse error.
  * Template-field warnings come only from the template chain.
  * Telemetry cert and key merge as a pair.
* **Smaller fixes**:
  * Remote fetches that run out of time are reported as `timeout` (#4164).
  * The chat app returns an error, not a panic, for Secret Manager responses with no payload (#4162).
