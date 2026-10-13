---
title: Working with Several Hubs
description: Give hubs nicknames, pick the current hub and project with scion use, and see which hub and project a command will target.
---

**What you will learn**: how to keep a list of hubs under short nicknames, how to select a current hub and project with `scion use`, and how the CLI decides which hub and project a command targets.

The CLI keeps a **hub list** for each user. Each entry gives a hub a **nickname**, such as `local`, `prod` or `staging`. One hub in the list can be the **current hub**, and each hub remembers its own **current project**. Together, the current hub and its project make up the **current context**. Commands use the current context when nothing more specific chooses a hub or project, so `scion list` or `scion start` work from any directory with no `--project` or `--global`.

The hub list, the current context and the known projects are stored in `~/.scion/context.yaml` (mode `0600`). The file holds no secrets: logins stay in `~/.scion/credentials.json`, keyed by the hub's URL, so renaming a hub never affects its login. Inside agent containers the file is never read (see [Agents are not affected](#agents-are-not-affected)).

:::note[Not the same as target-hub profiles]
`--hub-profile` selects a [target-hub profile](/scion/hosted/user/test-identity-target-hubs/), which is for test identities on another test hub. The hub list described here is separate.
:::

## Add hubs to the hub list

```bash
scion hub add prod https://hub.example.com
scion hub add staging https://staging.example.com --use   # also make it the current hub
```

A nickname uses lowercase letters, digits and `-`, starts with a letter or digit, and has at most 63 characters. Each hub URL can be in the list once. The hub's health is checked, but a hub that does not respond is still added, with a warning, so you can add hubs offline. If you are not logged in to the hub, the command prints the login command:

```text
Added hub 'prod' (https://hub.example.com).
Not logged in to hub 'prod'. Run: scion hub auth login --hub prod
```

Hubs are also added for you:

- **`scion server start`** adds the local workstation hub as `local`. It becomes the current hub when no hub is current yet and your global settings do not name another hub in `hub.endpoint`.
- **`scion hub auth login`** adds a hub URL given with `--hub-url` or `--hub`, with a nickname derived from the host (for example `hub` for `https://hub.example.com`). It does not make the hub current.
- **`scion use --hub <url>`** and **`scion use <project> --hub <url>`** add a URL that is not in the list yet, and print `Registered hub '<name>' (<url>); rename with 'scion hub rename'.`
- **`scion hub link`** adds the hub it linked on, if needed (see [Linked directories](#linked-directories)).
- **An existing `hub.endpoint` in your global settings** is added to the list once, the first time the CLI reads the hub list. It is named `local` for a local workstation hub, otherwise after its host. This import does not make it the current hub, and no settings file is changed.

After a hub is in the list, `--hub <nickname>` works anywhere `--hub <url>` does, and `scion hub auth login --hub-url <nickname>` works too. Shell completion offers the nicknames after `--hub`.

## See the hub list

```text
$ scion hub list
CURRENT  NAME     ENDPOINT                     AUTH
*        local    http://127.0.0.1:8080        dev
         prod     https://hub.example.com      oauth (alice@example.com)
         staging  https://staging.example.com  none
```

`AUTH` shows how the CLI would authenticate to each hub, worked out from local state only: `oauth (<user>)` for a stored login, `dev` for the workstation dev token (it counts only for a hub on a loopback address), `env token` for a token from the environment (`SCION_HUB_TOKEN`, `SCION_AUTH_TOKEN` or the agent token file), and `none`.

`--check` adds two columns. `REACHABLE` (`yes` or `no`) says whether each hub responds. `VERIFIED` says whether the hub accepted the CLI's credential: `yes`, `no`, or `-` when it was not checked. Each hub is sent only its own credential (its login, or the dev token for a loopback hub). Tokens from the environment are never sent.

## Rename and remove hubs

```bash
scion hub rename staging stage
scion hub remove stage
scion hub remove stage --logout   # also remove the stored login
```

`rename` updates the current hub and the projects known on that hub. `remove` also forgets the projects known on the hub. If the removed hub was current, no hub is current afterwards, and the command says how to pick another. The stored login is kept unless you pass `--logout`, so you can add the hub again without logging in.

## Select the current hub and project: `scion use`

`scion use` is an alias of `scion project use`; both take the same flags.

### Switch hubs

```text
$ scion use --hub prod
Now using project 'web' on hub 'prod' (https://hub.example.com).
```

`--hub` takes a nickname or a URL. Switching makes the hub current and restores the project you last selected on it. If there is none, the command says so:

```text
Now using hub 'staging' (https://staging.example.com).
No project selected on hub 'staging'; run 'scion use <project>' or 'scion project list --hub staging'.
```

Switching makes no network calls. If you are not logged in to the hub, it prints `Not logged in to hub '<name>'. Run: scion hub auth login --hub <name>`.

### Select a project

```text
$ scion use web
Now using project 'web' on hub 'prod' (https://hub.example.com).
```

The project can be a slug, a name, a hub project ID or a git URL, as for `--project`. Selecting a project also makes its hub the current hub. The CLI looks for it in this order:

1. **The known-project list.** If exactly one known project matches, the CLI checks it on its hub with one call and selects it. If the hub cannot be checked because it does not respond or you are not logged in, the project is still selected, with a warning or the login command, so you can switch while offline. If the hub says the project no longer exists, the CLI goes on to step 2.
2. **The hubs.** The CLI looks the project up on every hub in the list that it can authenticate to, in parallel, with a 5-second limit per hub. With `--hub`, only that hub is searched. When `SCION_HUB_ENDPOINT` or `SCION_HUB_URL` is set, only that hub is searched. `--refresh` skips step 1 and always asks the hubs.

Each hub's built-in global project is never offered as a match.

**When several hubs have the project**, the CLI asks you to choose:

```text
Several projects match 'web':
  1) web on prod (https://hub.example.com)
  2) web on staging (https://staging.example.com)
Enter choice 1-2 (or 'c' to cancel):
```

Without a terminal, with `--non-interactive`, or in agent mode, the command fails instead and lists the `--hub` value of each candidate:

```text
project 'web' exists on several hubs; choose one with --hub:
  scion use web --hub prod      (https://hub.example.com, id 1dfdd6c7-f077-4acd-bde2-978d12f34f9a)
  scion use web --hub staging   (https://staging.example.com, id 77ab0c3e-5d2a-4a8e-9b61-0c1f2e3d4a5b)
```

`--yes` does not pick a candidate for you. With `--format json` the error is `{"error": "ambiguous", "candidates": [{"hub", "endpoint", "id", "slug"}, ...]}`.

**When no hub has the project**, the error names the hubs that were searched:

```text
project 'web' not found on hubs: local, prod
```

If some hubs could not be searched, the error says that the project may be on one of them, and gives the login command for each hub you are not logged in to:

```text
project 'foo' not found on the hubs searched (local, prod); it may be on the hub that could not be searched (staging: not logged in)

Log in with: scion hub auth login --hub staging
```

When the only hub that could not be searched is one you are not logged in to, an interactive run offers to log you in (`Log in to staging now?`) and then searches that hub again. `--yes` does not start a login.

When the project is found but some hubs were skipped, the selection still succeeds and a `Warning: skipped hubs: ...` line names them.

### Forget the current project

```text
$ scion use --clear
Cleared the current project 'web' on hub 'prod'.
```

The hub stays current.

### Show what commands will target

Bare `scion use` shows the hub and project that commands run from the current directory will target, and where each one comes from. It reads local state only and makes no network calls.

```text
$ scion use
Hub:      prod  https://hub.example.com               (current context)
Project:  web   1dfdd6c7-f077-4acd-bde2-978d12f34f9a  (current context)
Auth:     logged in as alice@example.com (OAuth)
```

Inside a project directory, the directory takes precedence over the current context, and the saved context is shown on its own line:

```text
$ cd ~/src/bar && scion use
Hub:      local  http://127.0.0.1:8080                 (directory link)
Project:  bar    9a1e0b52-6c3d-4f7e-8a91-2b3c4d5e6f70  (directory: ~/src/bar)
Auth:     Dev auth (<dev token source>), not verified; run 'scion hub status' to check
Saved context (applies outside scion project directories): prod / web
```

The source labels are `--hub flag`, `SCION_HUB_ENDPOINT` (or `SCION_HUB_URL`), `directory link`, `project settings: <path>`, `global settings` and `current context` for the hub, and `--project flag`, `directory: <path>`, `current context` and `global project` for the project; `none` means nothing chose one. A hub that is not in the hub list shows `-` as its name. `scion hub status` uses the same labels for its endpoint.

With no hub at all, `scion use` prints `No hub selected. Add one with 'scion hub add <name> <url>' or start a local hub with 'scion server start'.` and exits 0.

With `--format json`, the output has `effective` (`hub` with `name`, `endpoint` and `source`; `project` with `id`, `slug`, `name`, `path` and `source`; and `auth` with `method` and `user`), `context` (the saved hub and project), and `overridden`, which is `true` when the effective target differs from the saved context. The JSON `source` values are `flag`, `env`, `directory`, `project_settings`, `context`, `global_settings`, `global` and `none`. A script can check its target this way before it acts.

## List projects across hubs

```text
$ scion project list
CURRENT  HUB      PROJECT  NAME          LOCAL PATH  LAST USED
*        prod     web      Web frontend  -           2m ago
         prod     api      API           -           3d ago
         local    bar      bar           ~/src/bar   1h ago
         -        scratch  scratch       ~/scratch   -
```

`scion project list` (or `scion projects list`) shows one row per project: the projects known on your hubs, merged with the project directories on this machine. A directory linked to a hub project shows its local path in that project's row. A directory that is not linked to a hub shows hub `-`. `CURRENT` marks the saved current project. Without `--refresh`, the command makes no network calls.

- `--refresh` first fetches the project list of each hub you are logged in to (or only the `--hub` hub). A hub that is not logged in or does not answer keeps its cached rows, and one `Warning: not refreshed: ...` line names it.
- `--hub <hub>` shows only the projects on that hub (a nickname or a URL).
- `--local` lists the project directories on this machine instead, with their type, agent count, status and workspace. It cannot be combined with `--hub` or `--refresh`.

Scripts that parse `scion project list` should use `scion project list --local`, which keeps the output format, JSON included, that the command had before it listed projects across hubs.

The known-project list is a cache: the hub is always authoritative. Projects get into it when you select them with `scion use`, when you link a directory with `scion hub link`, and from `--refresh`.

## Which hub and project a command targets

The hub and the project are resolved separately. The first source that gives a value wins.

**Hub:**

1. `--hub <nickname or URL>`. A URL that is not in the hub list is used for this command only and is not added.
2. `SCION_HUB_ENDPOINT`, then `SCION_HUB_URL`.
3. Inside a project directory: the directory's link entry in the hub list (see [Linked directories](#linked-directories)).
4. Inside a project directory: `hub.endpoint` in its settings (its own, else inherited from the global settings).
5. The current hub.
6. Outside a project directory, when no hub is current: `hub.endpoint` in the global settings.

**Project:**

1. `--project` or `--global`.
2. The project directory found from the current directory.
3. The project remembered for the hub chosen above. So `--hub staging` outside a project directory uses the project you last selected on `staging`.
4. The global project (`~/.scion`), for commands that allow it.

Inside an agent container, the agent's own project (`SCION_PROJECT_ID`) also applies; see the [project resolution order](/scion/reference/cli/#global-flags).

**Inside a project directory, the directory wins.** `scion use` changes what commands do outside project directories; scripts and commands run inside a repository keep targeting that repository's project. If you select a project inside a project directory, the selection is saved, and a note says it does not apply there:

```text
Note: in this directory, project 'bar' (~/src/bar, hub 'local') takes precedence; 'web' applies outside scion project directories.
```

**The context project is used only by commands that need a project.** Commands that act on a login, the hub list, the server or the Runtime Broker never get one, and neither do `scion config` commands. `scion tunnel`, `scion ssh` and `scion ssh-config` do use it. To make a script independent of the current context, pass `--project` or `--global`.

:::note[`--hub` in a linked directory]
For commands that act on a project, `--hub` given in a directory linked to a different hub prints a warning, because the directory's project probably does not exist on the other hub:

```text
Warning: this directory is linked to hub 'local' (http://127.0.0.1:8080), but --hub selects 'prod' (https://hub.example.com); the directory's project may not exist there.
```

The warning applies to project commands only. Commands that need no project, such as `scion hub status`, and commands given `--project` or `--global` do not print it.
:::

**Environment variables.** `SCION_HUB_ENDPOINT` and `SCION_HUB_URL` override the current hub in that shell and are never saved. Bare `scion use` and `scion use --hub` print `Note: SCION_HUB_ENDPOINT is set and overrides the current hub in this shell.` when the variable names another hub. `SCION_HUB_TOKEN` is not tied to a hub. When it is set and the hub came from the current context, `scion use` warns: `Warning: SCION_HUB_TOKEN is set and will be sent to hub '<name>'`.

**`scion server` and `scion runtime-broker`** do not use the current context at all; see [The server's own hub endpoint](#the-servers-own-hub-endpoint).

## Linked directories

`scion hub link` links the current directory's project on the hub that applies there (often the current hub) and records a **link entry** in the hub list: the hub, the hub project and the directory. From then on the directory resolves to that hub, whichever hub is current. The project also appears in `scion project list` with its local path. If the hub was not in the hub list, it is added. `hub link` does not write `hub.endpoint` into the project's settings.

`scion hub unlink` removes the directory's link entry. It does not change the current context.

When `scion hub auth login` gets its hub from the current context or from a directory's link entry (no `--hub-url`, `--hub` or environment variable), it stores the login and leaves your settings unchanged:

```text
Logged in to https://hub.example.com (current hub, selected with 'scion use'); settings unchanged.
Logged in to http://127.0.0.1:8080 (the hub this directory is linked to); settings unchanged.
```

## Agents are not affected

Agents never use the hub list or the current context. The CLI ignores `~/.scion/context.yaml` in agent mode, inside agent containers and in hub-managed agents, and the file is never copied into a container. Inside a container, the hub comes from `SCION_HUB_ENDPOINT` and the project from the agent's own settings. `scion use` and the `scion hub add`, `list`, `rename` and `remove` commands refuse to run inside an agent.

## The server's own hub endpoint

The hub list is CLI configuration only. A Scion server or Runtime Broker takes its hub endpoints from server-scoped settings:

| Value | Setting | Environment variable |
| :--- | :--- | :--- |
| The hub's public URL, which agents and links use | `server.hub.public_url` | `SCION_SERVER_HUB_ENDPOINT` |
| The hub a Runtime Broker reports to | `server.broker.hub_endpoint` | `SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT` |

A workstation combo server needs neither: it uses its own loopback address. `scion runtime-broker register` and `join` save `server.broker.hub_endpoint` when it is not set, except on a host that runs a local workstation hub.

A server still falls back to the CLI's `hub.endpoint` (or `SCION_HUB_ENDPOINT`) when these are not set. That fallback is deprecated and will be removed in a later release. When the server's endpoint actually comes from it, the server logs this warning once at startup. On a host configured as a server, `scion doctor` reports the same text when it can tell the server is affected:

```text
deprecated: the server's hub endpoint came from hub.endpoint/SCION_HUB_ENDPOINT; set server.hub.public_url (SCION_SERVER_HUB_ENDPOINT) or server.broker.hub_endpoint (SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT).
```

Because of this split, `scion use --hub prod` never changes the endpoint that a local workstation server gives its agents. Under `scion server` and `scion runtime-broker`, `--hub` takes a URL, not a nickname. See [Hub Endpoint Resolution](/scion/reference/server-config/#hub-endpoint-resolution) for the full order.

## See also

- [Connecting to a Hub](/scion/hosted/user/hosted-user/)
- [CLI reference: `scion project use`](/scion/reference/cli/#scion-project) and [`scion hub`](/scion/reference/cli/#scion-hub)
