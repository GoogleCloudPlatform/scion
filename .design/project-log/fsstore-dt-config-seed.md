# Seed the project identity files at create for per-agent git projects on shared storage

**Date**: 2026-10-28
**Base**: the share-verify hotfix head 7fc52cc7

## Problem

On a shared workspace backend (NFS and volume backends), a broker checks
`<ProjectRoot>/.scion/{project-id,project-slug}` before it starts an agent
(`sharedHubProjectPath` -> `checkSharedProjectConfig`, pkg/runtimebroker
start_context.go). If they are missing, the start is refused with 409
"project configuration missing or does not match".

The hub writes these files:
- for every project, in a background pass at hub start
  (`ensureProjectConfigIdentities`);
- at create, only for hub-native projects (`initHubManagedProject`) and
  shared-workspace git projects (`cloneSharedWorkspaceProject`).

A git project in clone-per-agent mode (or the git default with no workspace
mode, or worktree-per-agent) that is created after the hub starts therefore
had no `.scion` until the next hub restart. Every agent create in it failed
with the 409.

## Change

- `createProject` and the project clone path now have a third branch for git
  projects that are not shared workspaces. It calls the new
  `ensureSharedProjectIdentityBestEffort`, which on a shared backend runs
  `ensureProjectConfigIdentity` (the helper `seedSharedProjectIdentity` and the
  startup pass use).
- `handleProjectRegister` calls the same helper in its `created` branch only,
  for any project shape. Re-registering an existing project changes nothing.
- Each project shape has exactly one seeding call site at create. The helper
  is idempotent in any case, since `ensureProjectConfigIdentity` rewrites only
  a stale slug.
- It is best-effort. A failure is logged and the create goes on; the next
  hub-start pass retries it.
- A project-id naming another project is left unchanged and reported,
  exactly as for the other shapes.
- Only the identity files are written for these shapes: no workspace and no
  settings.
- Brokers still never write `<ProjectRoot>/.scion`, and the broker check is
  unchanged.

## Tests (pkg/hub, project_config_identity_create_test.go; hubshard default shard 5)

- `TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity`: for
  clone-per-agent, the git default and worktree-per-agent, a project created
  after hub start has both files with the right contents, at modes 0644 and
  a non-group-writable directory. No workspace or settings are created.
- `TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles`: nothing is
  seeded on the local backend.
- `TestCreateProject_SharedBackend_IdentitySeedIdempotent`: the hub-native
  seed, the new helper and the startup pass agree, and the files are
  unchanged.
- `TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds`:
  a differing project-id is kept and reported, and the create still succeeds.

- `TestCloneProject_PerAgentGit_SharedBackend_SeedsProjectIdentity`
  (review N-1): cloning a clone-per-agent or worktree-per-agent git project
  seeds the clone's identity files, with no workspace.
- `TestRegisterProject_SharedBackend_SeedsProjectIdentity` (git and hub-native
  shapes): a registered project gets both files at register time.
- `TestRegisterProject_ReRegisterExisting_LeavesIdentityUnchanged`: a
  re-register of an existing project leaves a mismatched project-id and a
  stale slug untouched.

All of these tests are in hubshard default shard 5.

## Why registered projects need the seed too (review N-2)

The broker runs the hub-seeded identity check whenever a dispatch carries a
project slug and no project path (`buildStartContext`, start_context.go).
The hub sends a project path only when the dispatching broker has a provider
`LocalPath` for the project; otherwise `ProjectDispatchLocation` returns the
slug alone (httpdispatcher.go; `brokerUsesSlugProjectDir` documents this).

A project created by `handleProjectRegister` is auto-linked to every
auto-provide broker with no `LocalPath` (`autoLinkProviders`). A register
without a path, or with a path only on the registering broker, leaves no
path on any other provider. An agent of such a project, dispatched to one of
those brokers on a shared backend, was refused with the same 409 until the
next hub-start pass. Registered projects therefore do not always dispatch
with a project path, and they are now seeded at register time as well.

## Hub-native workspace leaf at create, and its backfill (ruling (ii))

On a shared backend the first agent of a hub-native project (no git remote)
used to find no workspace leaf: nothing created `projects/<uuid>/workspace`
until an agent or a Files-tab write did. Now:

- **At create:** `initHubManagedProject` first runs
  `provisionHubNativeWorkspace`, which is the same `provisionSharedWorkspace`
  that shared-workspace git projects use, with no clone. That means:
  - the workspace leaf and `<ProjectRoot>/provision` are created through the
    confined `EnsureLeaf` walk (mode 2775, default ACL);
  - `PrepareStateDir` runs;
  - then `provision.ProvisionShared` runs (shared-plain,
    `SentinelDir = <ProjectRoot>/provision`, the Layout uid/gid), which chowns
    the leaf and writes the sentinel.
- **On failure:** the provisioning failure maps to
  `errWorkspaceStorageUnavailable`, with no clone wording and no host path.
  The create (and a project clone) is rolled back with 503, and a project
  root that this create made is removed.
- **No workspace lock:** a hub-native workspace gets no legacy lock in its
  root (`LegacyDir` is empty). That lock only excludes an older pod that may
  be cloning a git workspace, so the workspace stays empty.
- **Startup backfill:** the startup pass (`ensureProjectConfigIdentities`)
  provisions the leaf the same way for an existing hub-native project whose
  leaf is absent, only after its identity is confirmed (never next to a
  mismatch). It is best-effort, logs per project with ids only, and counts
  `workspaces_provisioned` and `workspaces_failed` in the summary line. A
  present leaf is left alone.
- **Never** for a git project (any mode), an empty-per-agent project or the
  global project (`hubNativeWorkspaceProvisioned`), so a hub-created leaf
  cannot change a git project's provisioning decision.
- **Tests (all default shard 5):**
  - `TestCreateHubNativeProject_SharedBackend_ProvisionsWorkspaceLeaf`
    (2775, the Layout owner, empty, the sentinel);
  - `TestEnsureProjectConfigIdentities_BackfillsHubNativeWorkspace`;
  - `TestEnsureProjectConfigIdentities_GitProjectsGetNoWorkspaceLeaf`;
  - `TestCreateHubNativeProject_LocalBackend_NoShareWrites`;
  - `TestCreateHubNativeProject_SharedBackend_ProvisioningFailureRollsBack`.

## Follow-ups (not in this change)

- **N-3:** request-path NFS I/O at create (the identity write through the
  confined walk) has no hang guard. A hung share can stall the create
  request; the hub-start pass already runs under a deadline.
- **N-4:** error-path hygiene in the shared helpers: make the warning texts
  and returned errors of `seedSharedProjectIdentity` and
  `ensureSharedProjectIdentityBestEffort` uniform, and make sure no host path
  beyond what the confined helpers report reaches a log line. That includes
  `provisionSharedWorkspace`'s own step warnings, which the backfill reuses.

`TestRequestPathSites` impact: none. The change adds no request-path use and
logs only `project_id` and `error`.
