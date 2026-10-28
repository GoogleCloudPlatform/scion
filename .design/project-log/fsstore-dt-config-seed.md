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

`TestRequestPathSites` impact: none. The change adds no request-path use and
logs only `project_id` and `error`.
