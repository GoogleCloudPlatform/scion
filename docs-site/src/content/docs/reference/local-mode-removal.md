---
title: Local-only mode removal
description: What changed when scion stopped running agents without a hub, what happens on your first command after upgrading, and how existing agents are imported.
---

Scion used to have a **local-only mode**: with no hub configured (or with `--no-hub`, or after
`scion hub disable`), the CLI started and managed agent containers directly, with no server.
That mode has been removed. Scion now always runs agents through a hub. On your own machine that
hub is the [workstation server](/scion/workstation/workstation-server/), which you start once with
`scion server start`; for a team it is a remote hub.

This page is the reference that the CLI's migration messages link to. It covers what changed,
what an existing user sees on the first command after upgrading, how existing agents are
imported, and how to roll back.

## What did not change

- **Projects keep their layout.** The `.scion` directory, agent homes under
  `.scion/agents/<name>`, and the git worktrees of existing agents at
  `.scion/agents/<name>/workspace` stay where they are. The Runtime Broker inside the
  workstation server reuses existing agent directories.
- **Machine and project setup are the same:** `scion init --machine` once per machine (or the
  [onboarding wizard](/scion/getting-started/onboarding/)), `scion init` once per repository.
- **Containers, templates, harness configs, tmux sessions and credentials** work as before. On a
  workstation server, agents get your harness login files from your home directory just as
  local-only mode gave them; see [Agent Credentials](/scion/local/agent-credentials/#host-credential-files-on-a-workstation).

## What changed

:::caution[New agents in a git repository get a clone, not a worktree]
In a repository with a git remote, a new agent clones the remote inside its container instead of
getting a git worktree of your checkout. A private repository needs a `GITHUB_TOKEN`, the agent
does not see your unpushed changes, and its branch reaches you only after it pushes. This is a
known gap; see [Linked git projects](/scion/local/workspace/#linked-git-projects). Imported
agents keep their worktrees.
:::

| Removed | Replacement / action |
| --- | --- |
| Running agents with no hub | Start the local hub once with `scion server start` (its first run opens the [onboarding wizard](/scion/getting-started/onboarding/)). To start it at login, install the service file that `scion server install` prints. |
| `--no-hub` flag | Removed. It stays for one release as a hidden flag that fails with a pointer to this page. Every command uses the hub. |
| `scion hub enable` | Removed. A configured hub endpoint is used automatically. The hidden command fails with guidance. |
| `scion hub disable` | Removed. To stop the local hub, run `scion server stop`. The hidden command fails with guidance. |
| `hub.enabled`, `hub.local_only` settings | Deprecated and ignored. They are still accepted so existing settings files validate. `scion hub status` notes it when a settings file still sets them. |
| `scion hub link` before using a repository with the local hub | Not needed. A repository links itself to the local workstation hub on its first command, without a prompt. A remote hub still asks before linking. |
| `scion --no-hub delete <name>` to finish a failed local cleanup | `scion delete --cleanup-only <name>` (add `--preserve-branch` to keep the branch). |
| Hint "To use local-only mode, run: scion hub disable" after hub errors | For the local hub: start it with `scion server start` (check with `scion server status`). For a remote hub: check it with `scion hub status`. |
| `templates show/delete/clone` silently falling back to the filesystem when the hub fails | The hub error is shown. `--local` is the explicit filesystem view, now also on `scion templates list --local`. |
| `scion hub unlink` putting a project into local-only mode | `scion hub unlink` now only records that the project is unlinked. Agent commands in that project then refuse with a pointer to `scion hub link`. Agents that already exist keep running on the broker; manage them with `--project <hub-slug>`. |

With no hub configured, any command that needs one fails with:

```
No hub configured. Start the local hub with 'scion server start' (first run opens setup), or set a remote one with 'scion config set hub.endpoint <url>'.
```

When the configured hub is the local workstation server and it is not running:

```
Local scion server is not running. Start it with 'scion server start'.
```

`scion config set hub.endpoint <url>` is the current way to point the CLI at a remote hub. A later
release replaces it with a list of named hubs; see [`scion use`](/scion/reference/cli/#scion-use)
for the current-project selection that already exists.

## Your first command after upgrading

Every repository migrates on its own, on its first hub command. There is no bulk migration
command.

1. **No hub running yet.** In a repository that still has agents from local-only mode, the first
   command prints:

   ```
   Local-only mode has been removed: scion now runs agents through a hub. Run 'scion server start' (the first run opens setup), then rerun this command; this project links automatically and existing agents are imported. See https://googlecloudplatform.github.io/scion/reference/local-mode-removal/.
   ```

2. **Start the local hub:** `scion server start`. If the onboarding wizard opens and your machine
   is already set up, you can step through it or skip it.
3. **Rerun the command.** The repository links to the local hub without a prompt:

   ```
   Linking project 'my-repo' to the local hub at http://127.0.0.1:8080
   ```

   The agents scion finds in `.scion/agents` that the hub does not know yet are imported, once:

   ```
   Local-only mode has been removed: scion now runs agents through a hub.
   Importing 3 existing agent(s) (name, template and harness config; history is not imported):
   + fix-bug (running, adopted)
   + docs (imported as created, not started)
   + refactor (imported as created, not started; its exited container was removed)
   Running agents were adopted in place by the broker. Other agents were imported as created, not started; start them with 'scion start <name>' (a suspended agent's harness session is not resumed).
   See https://googlecloudplatform.github.io/scion/reference/local-mode-removal/ for details.
   ```

If you point the CLI at a remote hub instead, the repository is not linked silently: the existing
link prompt asks first, and the import shows its plan for confirmation. If you decline, run
`scion hub link` later to import the agents.

If no Runtime Broker is online for the project (for example the server started without one,
see [missing image registry](#missing-image-registry)), the import is deferred and runs on the
next command once a broker is online.

### What the import does with each agent

The import carries over each agent's **name, template and harness config**. Status and
conversation history are not imported. The agent home, worktree and branch stay in place.

| Agent state before the upgrade | After the import |
| --- | --- |
| **Running** | Adopted in place: the same container keeps running, now managed through the hub. `look`, `message`, `stop` and `delete` work as usual. |
| **Created, never started** | Shown as **created** on the hub. No container is started. |
| **Suspended or stopped** | Shown as **created** on the hub, not started. No container is started. Its exited container is removed first; only that container's writable layer is lost. Everything outside the mounted home and workspace directories that the harness wrote inside the container is gone, and a suspended agent's harness session is not resumed. |

Start a non-running agent with `scion start <name>`. If the import could not remove an exited
container, it warns and prints the fix: run `scion stop <name>` before `scion start <name>`.

The import is a one-release migration tool. It is removed in a later release. After that, an
upgraded repository still links automatically, but old local agents are not registered. Their
directories stay on disk, and `scion start <name>` with the same name reuses the directory.

### Projects that ran `scion hub disable`

A project whose own settings still turn the hub off (`hub.enabled: false` or
`hub.local_only: true`, for example left by `scion hub disable`) is treated like a project that
was never linked: on its first command its local agents are imported once, and scion prints, once:

```
Local-only mode has been removed; hub.enabled=false / hub.local_only are ignored.
```

Scion then rewrites those keys to `hub.enabled: true` and `hub.local_only: false`, the values an
older scion release also reads as "use the hub". `scion hub link` does the same. Only the
project's own settings are read; a value in your global settings does not mark other projects.

### Users with a remote hub endpoint

If your settings already had a remote `hub.endpoint` but you kept the hub turned off, that hub is
now used. `scion hub status` shows the endpoint and where it came from. Nothing is linked to a
remote hub without a prompt.

## Missing image registry

The workstation server needs an `image_registry` for its Runtime Broker. Without one, the server
still starts the hub and the web UI, so the onboarding wizard can set it, but the broker does not
start, and `scion server start` prints:

```
Runtime broker not started: image_registry is not configured.
  Set it in the setup wizard, or run one of:
    scion config set --global image_registry <your-registry>
    export SCION_IMAGE_REGISTRY=<your-registry>
  then run 'scion server restart' to start the broker.
```

Run `scion server restart` after setting the registry; the broker does not start in place.

## macOS with Apple container

Agents reach the hub from inside their containers. Docker and Podman are handled automatically.
On macOS, runtime auto-detection picks Apple `container` first when it is installed (then Podman,
then Docker). With Apple container, container-to-host DNS needs a `sudo` DNS rule that must be re-created after each reboot (or installed as a LaunchDaemon); the onboarding
wizard's runtime step shows the command. You can choose Podman or Docker instead. See
[Apple Container DNS](/scion/local/apple-container/).

## Rolling back

Install the previous scion release. Settings stay compatible in both directions:

- `hub.linked` and `hub.endpoint` mean the same to older releases.
- A project that was migrated from `scion hub disable` now has `hub.enabled: true`, so an older
  release uses the hub for it too. Run the older release's `scion hub disable` there to return to
  local-only mode.
- `scion hub unlink` from this release writes `hub.linked: false` without `hub.enabled: false`.
  An older release then uses the hub if an endpoint and credentials exist.
- Agents imported onto the hub keep their directories, so the older release still finds them on
  disk.

## See also

- [Tutorial](/scion/getting-started/tutorial/): the first-run flow from a fresh install.
- [Choosing a Mode](/scion/choosing-a-mode/): Workstation and the hosted tiers.
- [Workstation Server Mode](/scion/workstation/workstation-server/): running and stopping the local hub.
- [CLI reference](/scion/reference/cli/): `scion delete --cleanup-only`, `scion hub unlink`, `scion templates list --local`.
