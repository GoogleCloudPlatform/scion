---
title: About Workspaces
---

Every Scion agent has a dedicated **Workspace**, mounted at `/workspace` inside the agent's container. In worktree-per-agent mode the agent instead works in its worktree under `/repo-root` (for a repository linked to the local workstation Hub, `/repo-root/.scion/agents/<name>/workspace`; see [Linked git projects](#linked-git-projects)); scripts should use `$PWD` or `git rev-parse --show-toplevel`. This is where the agent reads code, makes changes, and runs commands.

Scion provides flexible options for how this workspace is backed on your host machine, ranging from isolated git worktrees to direct directory mounts.

## Workspace Resolution

When you start an agent, Scion determines its workspace based on the following precedence:

1.  **Explicit Workspace** (`--workspace` flag):
    If you provide a path via `--workspace`, Scion mounts that directory directly. This works in both Git and non-Git environments.

2.  **Git Worktree** (Git repositories):
    A repository you use with Scion is linked to a Hub on its first command. On the local workstation Hub, each new agent gets a [git worktree](https://git-scm.com/docs/git-worktree) of your checkout and its own branch (section 2). On a remote Hub, and for projects linked before local worktrees existed, each agent clones the repository's remote instead. See [Linked git projects](#linked-git-projects).

3.  **Project Root / CWD** (Non-Git environments):
    If you are not in a Git repository, Scion mounts the project root (or current directory for global agents) directly.

---

## 1. Explicit Workspaces (`--workspace`)

You can tell Scion exactly which directory to use as the workspace. This is useful for:
- Working on a specific subfolder.
- Using a shared directory across multiple agents.
- Working on a registered git worktree outside the repository root, or a directory outside a non-git project.

### Absolute vs. Relative Paths

The `--workspace` (or `-w`) flag accepts both **absolute host paths** and **project-relative paths**, distinguished automatically by `filepath.IsAbs()`:

#### Absolute Paths

If you specify an absolute host path, Scion mounts that exact directory directly.

```bash
# Mount a specific absolute host path
scion start my-agent "run analysis" --workspace /home/user/my-service
```

- **Behavior**: The specified directory is mounted directly to `/workspace`.
- **Isolation**: **None**. Changes made by the agent are immediately visible on the host and to any other agents sharing this directory.
- **Git**: No new worktree or branch is created automatically. In a git project, a workspace in a subdirectory of the repository or outside the repository root must already be a registered git worktree of that repository; a plain subdirectory or an unregistered path outside the repository root is refused at start (see [Git that Scion runs on the host](#git-that-scion-runs-on-the-host)).

#### Relative Paths (Contained Subdirectory Mounts)

If you specify a relative path, it is interpreted as a subdirectory scoped to your project's **logical root** (or current working directory). This allows you to confine an agent to a specific subdirectory, such as a package in a monorepo:

```bash
# Scope the agent to a project subdirectory
scion start my-agent "fix web bugs" --workspace packages/web
```

- **Behavior**: Scion resolves the subdirectory path against the project root, performs strict containment checks (preventing directory traversal and escaping via symlinks), and mounts only that subtree to `/workspace`.
- **Isolation**: The agent has **full containment** — it cannot see or access any files or folders outside of that specific subdirectory.
- **Support**: Supported for directory (non-git) projects — both linked and hub-managed. In a git repository, a subdirectory workspace must be a registered git worktree of that repository; plain subdirectories are refused at start.
- **Git-Clone Projects Limitation**: Relative `--workspace` paths are **not supported** for per-agent git (clone-based) projects and will be rejected with an error. For those projects, use an absolute path or omit the `--workspace` flag.

---

## 2. Git Worktrees

In a repository linked to the local workstation Hub, each new agent gets a git worktree of your checkout at `.scion/agents/<name>/workspace` (see [Linked git projects](#linked-git-projects) for the requirements and the checks). Agents imported from a release that ran them without a Hub keep their worktrees at the same place. Each agent has its own isolated working directory and branch, so agents can work on different branches at the same time without touching your own working directory. The rules below describe how those worktrees are set up.

### Prerequisites
- Git **2.48.0** or newer is required (for relative path support).

### Branch Resolution
Scion determines which branch to check out in the worktree:

1.  **Explicit Branch** (`--branch`, `-b`):
    ```bash
    scion start my-agent -b feature/login "add logging"
    ```
    - If the branch exists and has a worktree, Scion **reuses the existing worktree** (see below).
    - If the branch exists but has no worktree, Scion creates a new worktree for it.
    - If the branch doesn't exist, Scion creates it (based on current HEAD) and a worktree.

2.  **Agent Name Matching**:
    If you don't specify a branch, Scion checks if a branch named after the agent exists (e.g., `my-agent`).
    - **Match Found**: It behaves exactly as if you passed `-b my-agent`.
    - **No Match**: Scion creates a new branch named `my-agent` and a corresponding worktree.

### Reusing Existing Worktrees
If you request a branch that is already checked out in another worktree (e.g., by another agent or manually created), Scion detects this.
- Instead of failing or creating a conflict, Scion **mounts the existing worktree path**.
- A warning is logged: `Relying on existing worktree for branch '...'`. With the workstation server it appears in `~/.scion/server.log`, not in the CLI.
- If that worktree is your own checkout (the agent is named after the branch you have checked out), the agent edits your working tree directly.
- This allows multiple agents to collaborate on the same branch/worktree if desired.
- An existing worktree is reused only while its directory is still in place. If git still lists the branch in a worktree that no longer exists, the start fails with an error that names that path; run `git worktree remove "<path>"` in the repository and start again. Scion removes only the agent's own worktree entry, when provisioning and when cleaning up, and does not prune other worktrees (except when a Runtime Broker cannot remove a partly created worktree; it then prunes stale entries so the start can be retried).

---

## 3. Non-Git Environments

In non-git projects (where no `.git` directory is found):
- Scion defaults to mounting the **project root** (the directory containing `.scion`).
- For global agents, it defaults to the **current working directory**.
- All agents share the same files. There is no isolation or branching.

---

## 4. Hub-Managed Workspaces

When a Scion Hub is enabled, workspace strategy changes depending on the project type. The Hub supports three types of remote workspaces:

### Hub-Managed Projects (no git repository)
Hub-Managed projects allow you to create project workspaces directly through the Hub API and Web Dashboard **without an external Git repository**.
- The Hub automatically initializes a seeded `.scion` structure (`projects/<uuid>/.scion` on shared workspace storage).
- Workspace files live in `projects/<uuid>/workspace` on shared workspace storage (`nfs`, `cloudrun-volume`, `gke-shared-volume`) or `~/.scion/projects/<slug>` on local storage, and are reached by Runtime Brokers through the same shared workspace storage backend or a co-located Runtime Broker.
- You can directly download individual workspace files or generate ZIP archives of entire projects using the Hub API or Web Dashboard, making it easy to export your data.

### Git Projects (clone-based, Hub-managed)
Projects created from a remote git repository URL use **clone-based provisioning**: the agent's workspace is initialized from the repository at startup.

```bash
# Create a git project from a URL (Hub-managed)
scion hub project create https://github.com/org/repo.git
```

#### How Git Projects Work

1. The Hub stores the git remote URL and default branch as project metadata.
2. When an agent starts, the Runtime Broker injects `SCION_GIT_CLONE_URL`, `SCION_GIT_BRANCH`, and `SCION_GIT_DEPTH` as environment variables.
3. The `sciontool init` process inside the container uses a `git init` + `git fetch` strategy to provision the workspace into `/workspace`. This approach handles workspaces that may already contain `.scion` metadata or `.scion-volumes` directories, and properly clears stale artifacts before initialization.
4. A feature branch `scion/<agent-name>` is created and checked out automatically.

#### Project ID Format

Project IDs are always randomly generated UUIDs (v4). A git remote is associated metadata, not identity, so the same repository can back multiple projects, and re-creating a project from the same URL produces a new ID.

#### Agent Branch Strategy

Each agent gets its own branch named `scion/<agent-name>`. This prevents conflicts when multiple agents work on the same repository concurrently.

#### Shallow Clones & Full Clones

By default, git projects use a shallow clone with `depth=1` for fast startup. 

- **Full Clones on Startup:** If an agent needs full git history for operations like `git log` or `git blame` from the very start, you can configure `depth: 0` (or set `SCION_GIT_DEPTH=0`). The `GitCloneConfig` API treats a depth of `0` as an explicit instruction to perform a complete, non-shallow clone.
- **Unshallowing Later:** Alternatively, an agent with a shallow clone (`depth=1`) can unshallow itself later inside its session by running:
  ```bash
  git fetch --unshallow
  ```

#### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_GIT_CLONE_URL` | URL of the repository to clone. Set by the broker from the project's clone settings only; a value in agent, settings or harness-config env is ignored | *(set by the broker)* |
| `SCION_GIT_BRANCH` | Branch to clone | `main` |
| `SCION_GIT_DEPTH` | Clone depth (set to `0` for full clone, omission defaults to shallow depth `1`) | `1` |
| `SCION_GIT_TOKEN_REMOTE` | The project's git remote, as the Hub recorded it. Set by the broker only | *(none)* |

Authentication is handled via the `GITHUB_TOKEN` environment variable, injected from the project's secrets or your local environment through the env-gather flow. The clone uses the token only when the clone URL is HTTPS and names the project's git remote (the same host and owner/repository as `SCION_GIT_TOKEN_REMOTE`). A clone URL that names another host or repository, for example through a `scion.dev/clone-url` label, is cloned without the token, and no credential helper is configured for it. The same rule applies to the Kubernetes workspace-provisioning init container.

The clone URL must use `https`, `ssh` (including the `git@host:path` form) or `git`. A local path, a `file://` or `http://` URL, a `<transport>::<address>` form, or a URL starting with `-` is refused: the agent is not started. A create request answers HTTP 422, and a start fails with the same fixed message; neither repeats the URL.

### Linked git projects

This section is the reference for how agents in a git repository on your machine get their
workspace; other pages link here.

A repository on your machine becomes a **linked project** when it links to a Hub: on its first
command against the local workstation Hub (no prompt), or with `scion hub link`. Run `scion init`
in the repository first. In a git repository without its own `.scion`, where the project would
otherwise fall back to the global project (no `--global` or `--project` given), `scion hub link`
and the automatic link refuse with:

```
this git repository is not a scion project yet; run 'scion init' here first (or pass --global to link the global project)
```

A `.scion`-less repository inside another scion project is not refused: it links that parent
project, so run `scion init` in the repository itself.

`scion init` adds an untracked `.scion/` to your checkout and, unless it is already ignored, appends `.scion/agents/` to your `.gitignore`.

#### On the local workstation Hub: a worktree of your checkout

A repository that links to the local workstation Hub is registered with the `worktree-per-agent`
mode. Each new agent then gets a git worktree of **your checkout** at
`.scion/agents/<name>/workspace`, on its own branch (see [Branch resolution](#branch-resolution)).
The CLI says so when the agent starts:

```
Using hub, git worktree per agent for repo <remote>
  (for a checkout linked on this machine, each agent gets a worktree of it under .scion/agents/<name>/workspace; otherwise the runtime broker makes per-agent worktrees of its own clone)
```

- **No clone and no token.** Nothing is fetched from the remote.
- **What the agent sees.** The worktree starts from your checkout's current commit, so commits you
  have not pushed are visible to the agent; uncommitted changes are not.
- **Branches land in your repository.** The agent's branch shows up in `git branch`; merge it
  directly.
- **The working directory is the worktree, not `/workspace`.** Your repository is mounted in the
  container at `/repo-root`, and the agent starts in `/repo-root/.scion/agents/<name>/workspace`.
  `/workspace` is not mounted for these agents. Scripts and instructions should use `$PWD` or
  `git rev-parse --show-toplevel` instead of a hard-coded `/workspace`.
- **Deleting an agent deletes its branch**, including commits you have not merged, unless you pass
  `--preserve-branch` to `scion delete`.
- **Git 2.48 or newer.** The first agent worktree sets `repositoryformatversion=1` and
  `extensions.relativeWorktrees` in your repository's git config. From then on, git older than 2.48
  cannot read that checkout (it reports an unknown repository extension).
- **No reincarnate** for these agents yet; to rebuild an agent's home, delete it and start it again.
- This applies to the Runtime Broker inside the workstation server. Kubernetes runtimes are not
  routed this way.

:::caution[An agent named after your checked-out branch works in your checkout]
If an agent's name matches the branch you have checked out, the agent attaches to your own
checkout, as it did before the Hub was required, and edits your working tree directly, uncommitted changes included.
The warning ("Relying on existing worktree") appears only in `~/.scion/server.log`, not in the
CLI. Give agents names that are not your current branch.
:::

Before each start, the broker checks the checkout. If a check fails, the start stops; it never
falls back to a clone. The CLI shows:

```
Error: failed to start agent via Hub: runtime broker failure: <reason> (broker code: local_worktree_refused, broker status: 409, request: <id>)
```

where `<reason>` starts with `cannot create a local worktree for this agent:` and names the cause
and the fix:

| Cause | Reason (after the prefix) |
| --- | --- |
| git too old | `git >= 2.48.0 required for worktree-per-agent mode (--relative-paths), found <version>; upgrade git on this machine` |
| not a git checkout | `the project's local path <path> is not in a git checkout; restore the checkout there, or run 'scion hub link' from the project's git checkout` |
| repository root unreadable | `git could not determine the repository root of the project's checkout at <path>; check that git can read that repository (for example, its ownership and safe.directory setting)` |
| no origin | `the project's checkout at <repo> has no origin remote, but the hub project's repository is <url>; add it with 'git remote add origin <url>'` |
| different origin | `the project's checkout at <repo> has origin <url>, but the hub project's repository is <url>; point origin at the hub project's repository, or link this checkout as its own project` |

Agents that already have a workspace keep it: agents imported from a release that ran them without
a Hub keep their worktrees at `.scion/agents/<name>/workspace`, and agents that already have a
clone keep their clone.

#### Projects linked earlier: still cloning

The workspace mode is set when a hub project is created and cannot be changed. A repository that
was linked to the local workstation Hub by hand before local worktrees existed keeps cloning (see
the next section) until you re-link it to a new hub project. Running `scion hub unlink` and then
`scion hub link` is not enough: it re-attaches the old project. To re-link:

1. Finish the project's agents, or delete them with `scion delete <name>`.
2. Delete the hub project: `scion hub projects delete <name>`. This removes the project's agent
   records on the Hub for every broker.
3. In the repository, run `scion hub link` (or any agent command, which links automatically). The
   new project is registered with the `worktree-per-agent` mode; check with
   `scion hub projects info <name> --format json` (the text output does not show labels). When a
   git checkout that already has a local project directory (`~/.scion/project-configs/<slug>__<short-id>`)
   is linked to a new Hub project, agent commands such as `scion list`, `start`, `stop`, `resume` and
   `delete` resolve the Hub project from the link (`hub.project_id` in the project's `settings.yaml`)
   rather than the earlier local project ID in `.scion/project-id`.

#### On a remote Hub: a clone per agent

A repository linked to a remote Hub, or one of the projects above that still clones, gets
clone-based provisioning: each new agent clones the repository's remote over HTTPS inside its
container, on branch `scion/<agent-name>`. The Hub enforces one workspace strategy for the
project, whichever broker an agent lands on.

**What this means in practice:**

- **SSH credentials are not used** for workspace provisioning. Agents clone via HTTPS using `GITHUB_TOKEN`.
- **A `GITHUB_TOKEN` is required** for private repositories. Set it as a project or user secret on the Hub, or ensure it is present in your local environment (the env-gather flow will collect it; a non-interactive `scion start` or `scion resume` sends it only with `--send-env`, and fails without it naming the missing keys and the `--send-env` flag):
  ```bash
  scion hub secret set --project=my-project GITHUB_TOKEN ghp_xxxxxxxxxxxx
  ```
- **The CLI will tell you** when this mode is in effect:
  ```
  Using hub, cloning repo https://github.com/org/repo.git
  ```
- **The agent works from the remote's state**, not from your checkout, and its branch reaches you only after it pushes. Merge agent work via git push and pull request.

To give agents on a remote Hub git worktrees instead of separate clones, create the project with the
`worktree-per-agent` [workspace sharing mode](/scion/local/workspaces-and-sharing/): the broker then
makes a worktree per agent of its own shared clone. The mode is set when the project is created on
the Hub and cannot be changed later.

#### Git that Scion runs on the host

The Runtime Broker and the Hub run git on the host during provisioning clones and in repositories that
agent containers can also write: your linked checkout (when it creates or removes an agent's
worktree), the shared clone of a `worktree-per-agent` project, and the clone of a shared-workspace
project (including shared-workspace pulls). Before each of these commands in an existing repository,
Scion reads the repository's config (`.git/config`, and a worktree's `config.worktree`) as data and
**refuses the command, naming the key**, if the config contains an `include`/`includeIf` directive
or any key outside an allow-list. This is deny-by-default: a key that is not known to be safe stops
the agent start or workspace pull rather than being ignored.

- **A repository Scion manages** (the shared checkout of a `worktree-per-agent` project, the clone
  of a shared-workspace project) allows only the keys git and Scion write for it: the core
  repository format settings, `extensions.*`, a remote's `url`/`fetch`, `branch.<name>` tracking,
  identity, `gc.auto`, `branch.autoSetupMerge`.
- **Your own linked checkout** allows those plus a wider set of keys that cannot run a command or
  redirect network traffic: `core.autocrlf`/`eol` and similar, `pull.*`/`push.*` defaults, a
  remote's `pushurl`/`tagopt`/`prune`, `branch.<name>` metadata, `color.*`, `advice.*`, `rerere.*`,
  `status.*`, `maintenance.*`, sparse-checkout toggles, `init.defaultBranch`, inert git-lfs data
  keys, `gpg.format` and `user.signingKey` (signing data only), and `submodule.<name>.update` **only**
  as `none`/`checkout`/`rebase`/`merge`, `submodule.<name>.url` **only** for an `https`/`ssh` remote.
  A few keys that would run a command are allowed here only because a fixed command-line setting
  overrides them on every call (`core.hooksPath`, `core.fsmonitor`, `core.pager`,
  `submodule.recurse`), so such a repository value never runs a command.
- **Refused on both** (so an agent cannot make the broker's or your own git run a command): any
  `credential.*`, `url.*.insteadOf`, `http.*`, `gpg.*` program/signing-file keys, any `*.program`,
  `alias.*`, `core.sshCommand`/`editor`, a remote's `uploadpack`/`receivepack`/`proxy`, diff/merge
  command or textconv drivers, non-standard `filter.*` drivers, and any key not on the list. If a
  command is refused, the error names the key; remove it from the repository's `.git/config` (or
  `config.worktree`).
- **Partial clones and sparse-index checkouts are refused:** `extensions.partialClone` and
  `index.sparse` are not on the allow-list, so host-side git in such a repository is refused and an
  agent cannot start there. This is a deliberate, fail-closed limitation; it may be lifted later.
- As defence in depth these commands also run with hooks, the file-system monitor and the pager
  off, signing off (`commit.gpgsign`/`tag.gpgsign`/`merge.verifySignatures` false), the `ext::`
  transport disabled and the `file://` transport allowed only for a top-level operation the user
  drives directly (`protocol.file.allow=user`; git's own indirect uses such as submodule fetches are
  blocked), submodule recursion off, and the `GIT_*` environment scrubbed (except the host operator's own
  config-file, transport and `GIT_ALLOW_PROTOCOL` settings). **Note:** a `GIT_ALLOW_PROTOCOL` set by
  the host operator overrides the `protocol.*.allow` pins.
- **git-lfs:** host-side checkouts take the LFS endpoint from the configured remote, never from a
  tracked `.lfsconfig` (which an agent could point at an arbitrary server).
  - A repository Scion manages (the shared checkout and the shared-workspace clone) and every
    provisioning clone skip the LFS smudge on the host (`GIT_LFS_SKIP_SMUDGE=1`); agents fetch LFS
    objects in-container.
  - Your linked checkout pins the endpoint derived from the `origin` remote URL, with its auth mode
    pinned to `basic` (`lfs.<endpoint>.access`), when the origin is `http(s)` **and** the committed
    `.lfsconfig` neither names its own endpoint (`lfs.url`/`pushurl`, `remote.*.lfsurl`) nor sets any
    `lfs.<url>.access`. If it does either (its objects may legitimately live on a separate server, or
    it selects an auth mode such as `negotiate`), or the origin is `ssh`/scp or absent, the smudge is
    skipped and a warning names the pointer files and says to run `git lfs pull` in the checkout.
    Scion never contacts the `.lfsconfig` endpoint.
  - The git-lfs **post-checkout hook** does not run; it only marks LFS-lockable files read-only.
  - git-lfs's own config writes are tolerated: `lfs.repositoryformatversion`, and `lfs.<url>.access`
    (only `basic`/`none`) and `lfs.<url>.locksverify` with an http(s) URL subsection.
- **Credentials** are never embedded in a clone URL and never left in a workspace's `.git/config`.
  A git token is supplied only through a credential helper scoped to the clone URL's host (so it is
  offered to that host and no other, in particular not to an LFS endpoint), and only for an `http(s)`
  URL; for any other URL the token is ignored. The scoped helper also disables any other configured
  credential helper for the command, so nothing persists the token. On a shared storage layout
  (`server.project_storage`), the Hub provisions `shared-plain` git workspaces; if a Runtime Broker
  reaches its backstop clone path because the Hub's provisioning sentinel is missing, it passes no git
  token and sets no token remote, so a private repository fails with the "needs credentials" error
  until provisioned through the Hub.
- **Updating a shared workspace** (pull) fetches from the project's clone URL known to the Hub, not
  from the remote configured in the clone, then fast-forwards the current branch.

Your own git commands in your checkout are not affected. Scion also mounts the repository's git
metadata into agents so that an agent cannot change what git does for you or for other agents. In the
worktree layouts, the whole `.git` root is mounted **read-only**, so an agent cannot create a
root-level file git honours (such as a `commondir` that would redirect where git reads config, hooks
and refs from). Only the paths an in-container git must write are re-opened read-write: `objects/`,
`refs/`, `logs/`, the git-lfs object store (`.git/lfs`), and the agent's own worktree admin directory.
The rerere cache (`.git/rr-cache`) is re-opened read-write only if it already exists — Scion never
creates it, because creating it would turn on rerere in your own checkout. On docker and podman,
`.git/config` is served from each agent's own **private, read-only copy** in place of the shared one;
the private copy has credential-bearing settings stripped (`credential.*`, `http.extraheader`,
`http.cookiefile`, client TLS key and certificate paths `http.sslKey` and `http.sslCert` with or
without a URL subsection while keeping `http.sslCAInfo` and `http.sslCAPath`, and plaintext password
keys `sendemail.smtppass`, `imap.pass`, `git-p4.password`, and `gitcvs.dbpass`; embedded userinfo is
also scrubbed from URL-valued keys `remote.*.url`/`pushurl`/`lfsurl`/`lfspushurl`,
`lfs.url`/`pushurl`, `url.*.insteadof`/`pushinsteadof`, `http.proxy`, `remote.*.proxy`,
`submodule.*.url`, `bundle.*.uri`, `fetch.bundleuri`, `imap.host`,
`svn-remote.*.url`/`pushurl`/`commiturl`/`rewriteroot`, and `svn.commiturl`, as well as from
`branch.*.remote`, `branch.*.pushremote`, and `remote.pushdefault` when their value is a URL or
remote-helper form rather than a remote name) and is pre-seeded (the agent's branch
upstream plus `push.autoSetupRemote`) so that a bare `git push` exits 0 — without the pre-seed a bare
push fails with "no upstream branch". `git push -u origin <branch>` exits 0 and reaches the remote
either way; git cannot persist the upstream (the private `.git/config` is read-only) and prints an
`error:`, but the push still succeeds.
That copy also disables automatic housekeeping (`gc.auto=0`, `maintenance.auto=false`) so routine
commands do not try to write lock files in the read-only root. Personal git preferences inside the
agent go in the agent's own global git config, which is writable, not in the repository config. On the
Apple runtime agents read the repository's git config directly (the runtime cannot bind a single
private file), so the private-copy settings above (credential stripping, the push pre-seed, the
housekeeping defaults) do not apply there; keep credentials out of repository git config (use a
credential helper in your global git config). An Apple agent whose repository `.git/config` holds
any of the credential-bearing settings above is refused at start, naming the sections that must be cleared; use a
credential helper in your global git config, or run the project on a runtime that provides the private
config copy.

A shared-workspace git project (agents sharing one checkout) is refused outright on the Apple runtime,
which cannot bind the per-agent private `.git/config`; use a worktree-per-agent project or a non-Apple
runtime.

Commit, branch creation, fetch, pull, push and rebase work normally. Because the `.git` root is
read-only, a few operations that write a lock or state file directly in it do not work inside an agent:
deleting a branch or a tag and `git fetch --prune` (all need `packed-refs.lock`), `git gc` /
`git pack-refs`, and fetching into a shallow clone. These fail cleanly, without corrupting the
repository. `git stash` still round-trips (the stash ref lives in the writable `refs/` overlay), though
it may print a cosmetic `packed-refs.lock` warning.

This applies to the worktree-per-agent and outside-root layouts, where each agent has its own
worktree. In the shared-workspace layout agents share one working tree and the `.git` root stays
writable; there the host-side git controls additionally refuse to run if a root-level `commondir` or
`gitdir` is present in the repository, so a redirect file cannot steer git that Scion runs on the
host.

A workspace in a subdirectory of a repository, or outside the repository root, must be a registered git worktree of that repository (for an outside-root worktree, only `/repo-root/.git` and `/workspace` are mounted into the container, not the full repository root). Plain subdirectory workspaces and unregistered workspaces outside the repository root are refused at start, because the per-agent git metadata cannot be mounted for them.

On Docker and Podman, a project `.scion` that is mounted read-only — one in a registered outside-root
checkout, one in a subdirectory of the workspace, or a nested `.scion` directory inside the agent's
own workspace (config-only agents) — must already contain an `agents` directory.
The runtime masks `.scion/agents` in the container but does not create it under a read-only mount, so
when it is missing the start is refused; create it once with `mkdir <path>/.scion/agents` (`scion
init` lists `.scion/agents/` in `.gitignore`) and start the agent again.

On Kubernetes, git projects that share one repository across agents (worktree-per-agent or
shared-workspace on NFS) are not supported: use a clone-per-agent workspace, where each agent gets
its own clone of the repository, or run the project on a local or Docker broker.

---

## 5. Project Shared Directories

Project Shared Directories provide a persistent, mutable storage layer that can be shared between multiple agents within a single project. This is ideal for sharing build artifacts, shared caches, or state files without relying on version control or the Hub database.

### Default Scratchpad Auto-Provisioning

To ensure that newly created projects have an immediate space for agent file transfers, message attachments, and logs, the Hub can automatically provision a default shared directory named `scratchpad` upon project creation.

- **Auto-Provisioning Toggle**: This behavior is controlled by the `project_defaults.default_scratchpad` setting in the Hub's operational settings (default is **`ON` / `true`**).
- **Inbound Message Attachments**: In isolated workspace modes, Scion automatically routes agent message attachments through the default `scratchpad` shared volume to prevent silent delivery failures.
- **Attachment copies expire**: Copies of attachments under the scratchpad's `.attachments/` directory are removed after `messaging.attachment_inbox_retention_days` (default 7 days). This covers chat attachments, `scion message --attach` files, chat plugin downloads and terminal drag-and-drop uploads. Downloading an attachment from chat history is not affected.
- **Direct-message attachments on the workstation**: The workstation server stores chat attachments locally, without artifact inbox delivery. Every chat attachment, including one in a direct message, is therefore copied into the project's scratchpad, where the project's other agents can read it.

### Managing Shared Directories

You can manage shared directories using the `scion shared-dir` CLI commands:

```bash
# Create a new shared directory
scion shared-dir create <name>

# List shared directories in the current project
scion shared-dir list

# View details about a specific shared directory
scion shared-dir info <name>

# Remove a shared directory (permanently deletes contents)
scion shared-dir remove <name>
```

### Removing a Shared Directory

Removing a shared directory deletes its data:

- **On a shared backend** (`server.project_storage.backend` `nfs`, `cloudrun-volume` or `gke-shared-volume`): removing the declaration through the Hub (Web UI or API) deletes the directory and everything in it from the shared export or volume, which every agent of the project mounts.
- **On the local backend**: unchanged. The directory on the Hub host or the co-located broker is deleted, as before.
- **The `scratchpad` directory** cannot be removed while the Hub's project default `default_scratchpad` is on. The request is refused with `409`.

### When the Backing Directory Is Created

Creating a shared directory and creating its backing directory on disk are separate steps:

- **Local CLI (`scion shared-dir create`)** creates the backing directory immediately.
- **Hub (Web UI or API)** only records the declaration. The backing directory is created on first use:
  - when an agent in the project starts on a Runtime Broker. Each broker creates the directory locally, or on the shared export or volume when `server.project_storage.backend` is `nfs`, `cloudrun-volume` or `gke-shared-volume`.
  - when a file is first written or uploaded to it through the Hub.

Until then, listing the shared directory's files through the Hub returns an empty list, and reading a file returns not found. Listing or reading never creates the directory.

A write through the Hub only creates the directory in storage the Hub itself can reach: the NFS export, a hub-managed project's storage on the Hub host, or the project's directory on a Runtime Broker running in the same server as the Hub. It never creates the directory on a remote broker; that copy appears only when an agent of the project starts on that broker. For a git project with no co-located broker and no NFS storage, a write through the Hub is rejected and creates nothing.

### Mounting Shared Directories

When an agent is created in a project that has shared directories, they are automatically mounted into the agent's container.

Each shared directory's mount location depends on its `in_workspace` setting:
- **`in_workspace: false` (default):** `/scion-volumes/<name>` — mounted at the standard volume path, outside the workspace.
- **`in_workspace: true`:** `/workspace/.scion-volumes/<name>` — mounted inside the workspace tree.

A shared directory is mounted at one of these locations, not both.

By default, shared directories live on the host that runs the agent. In hosted deployments, an operator can set [`server.project_storage.backend`](/scion/reference/server-config/#project-storage-serverproject_storage) to `nfs`. Every Runtime Broker then resolves a project's shared directories (and workspaces) to the same NFS export, so agents on different brokers share the same files. A directory that should stay on each node, such as a build cache, is a `volumes:` entry, not a shared directory.

### Common Patterns

Shared directories are most useful when several agents in a project need to exchange data outside of version control. The patterns below show typical setups.

#### Shared Build Caches

Agents working on the same project can reuse a compilation cache instead of rebuilding from scratch, dramatically speeding up warm builds.

```bash
# Create a shared cache, mounted outside the workspace (default)
scion shared-dir create build-cache
```

Point the toolchain's cache at the mount. For a Go project, set `GOCACHE` to the shared path in each agent (for example, in the agent's shell profile or start command):

```bash
export GOCACHE=/scion-volumes/build-cache
```

The same approach works for other caches — for example a Bazel `--output_base=/scion-volumes/build-cache`, or a `node_modules/.cache` symlinked into the mount. All agents read from and write to the same cache, so the first agent to compile a target populates it for the rest.

#### Shared Artifacts (Producer/Consumer)

One agent produces build artifacts or data files that another consumes — a build/test or build/deploy split.

```bash
scion shared-dir create artifacts
```

The **producer** agent writes its output to the mount:

```bash
go build -o /scion-volumes/artifacts/myservice ./cmd/myservice
```

The **consumer** agent picks the artifact up from the same path for testing or deployment:

```bash
/scion-volumes/artifacts/myservice --run-tests
```

Because the mount is a plain directory, no Hub round-trip or git commit is needed to hand data between agents.

#### Shared Context / Knowledge Base

A directory of reference material — design docs, research findings, or generated context — that every agent can read, and selected agents can write to.

```bash
# Read/write for agents that curate the knowledge base
scion shared-dir create knowledge --in-workspace
```

With `--in-workspace`, the directory mounts at `/workspace/.scion-volumes/knowledge`, keeping reference material alongside the code an agent is editing. A research agent writes its findings:

```bash
echo "## API rate limits: 100 req/s per token" >> /workspace/.scion-volumes/knowledge/notes.md
```

A developer agent reads them while working. To make a knowledge base read-only for consumers so they cannot accidentally overwrite curated content, create it with `--read-only`:

```bash
scion shared-dir create knowledge --read-only
```

#### Coordination Files

Lightweight, file-based signalling between agents — status markers, lock files, or simple message passing — without a message broker.

```bash
scion shared-dir create coordination
```

For example, a build agent signals that a phase is complete by writing a marker file:

```bash
# Producer signals completion
touch /scion-volumes/coordination/build.done
```

A downstream agent waits for the marker before starting its work:

```bash
# Consumer waits for the signal
until [ -f /scion-volumes/coordination/build.done ]; do sleep 5; done
```

The same pattern supports simple lock files (create a file to claim a resource, remove it to release) or drop-box message passing (write a request file, poll for a response file). Keep coordination files small and treat them as ephemeral signals rather than durable state.

### Web Dashboard File Viewer

You can browse the contents of Shared Directories and view file previews directly from the Hub's Web Dashboard. In the project view, navigate to the Shared Directories tab to inspect files, view sizes, and review content previews without needing to attach to an agent.

### Storage Backends
- **Local Workstations:** Backed by directories on the host filesystem.
- **Kubernetes:** Backed by PersistentVolumeClaims (PVCs) with project-scoped lifecycle management, ensuring data persists across pod restarts and can be accessed by any agent in the project.

---

## The `cdw` Command

Scion provides a helper command, `cdw` (Change Directory to Worktree), to quickly navigate to an agent's workspace on your host.

```bash
scion cdw <agent-name>
```

- Spawns a new shell inside the agent's workspace directory.
- Works for both managed worktrees and manual mounts (if resolvable).

Will also take a branch/worktree name outside of scion agents, most useful for getting back to main.

```bash
scion cdw <agent-name>
```

## Cleanup

When you delete an agent:
```bash
scion delete <agent-name>
```
- **Worktrees**: The worktree directory and git's entry for that one worktree are removed. Scion does not run a repository-wide `git worktree prune`, so other worktrees' entries are left alone.
- **Branches**: By default, the branch is deleted. Use `--preserve-branch` (or `-b`) to keep it.
- **Explicit Workspaces**: Directories mounted via `--workspace` are **NOT** deleted. Scion only cleans up resources it created.

:::caution[Committed is not Pushed]
Committing changes within your agent's workspace is **not** an automatic guarantee of safety.
- **`--preserve-branch` does not push:** The `--preserve-branch` (or `-b`) flag only keeps the agent's branch inside the *local* repository clone on the host/broker. It does **not** push the branch to a remote origin repository.
- **Avoid losing work:** If you are operating on an ephemeral or remote broker container (where the local clone is discarded when the agent is deleted), any unpushed commits on the agent's branch will be **permanently lost**. Always ensure your agents run `git push` (or you push manually from the workspace) to save your commits upstream before deleting the agent.
:::
