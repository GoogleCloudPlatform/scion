# Shared storage: project identity files at every create, and the hub-native workspace leaf

**Date**: 2026-10-29
**Base**: the share-verify fix (7fc52cc7)

## Problem

On a shared workspace backend (NFS and volume backends), a broker checks
`<ProjectRoot>/.scion/{project-id,project-slug}` before it starts an agent
for a project dispatched without a project path (`sharedHubProjectPath` ->
`checkSharedProjectConfig`, pkg/runtimebroker start_context.go). If the
files are missing, the start is refused with 409 "project configuration
missing or does not match".

The hub wrote these files in a background pass at hub start
(`ensureProjectConfigIdentities`) and, at create, only for hub-native
projects (`initHubManagedProject`) and shared-workspace git projects
(`cloneSharedWorkspaceProject`).

These project shapes created after hub start therefore had no `.scion` until
the next restart, and every agent create in them failed:
- a git project in clone-per-agent mode, the git default with no workspace
  mode, or worktree-per-agent (create and clone);
- a registered project, which is dispatched by slug to every provider without
  a registered local path.

Separately, a hub-native project's first agent on a shared backend found no
workspace leaf, because nothing created `projects/<uuid>/workspace` until an
agent or a Files-tab write did.

## Change

### Project identity files at create

- `createProject` and the project clone path have a branch for git projects
  that are not shared workspaces. It calls `ensureSharedProjectIdentityBestEffort`,
  which runs `ensureProjectConfigIdentity` on a shared backend.
- `handleProjectRegister` calls the same helper in its `created` branch only,
  for any project shape. A re-register of an existing project changes
  nothing.
- Each shape has one seeding call site at create, and the helper is
  idempotent.
- It is best-effort: a failure is logged and the create goes on, and the next
  hub-start pass retries it.
- A project-id naming another project is never overwritten; it is reported,
  as before.
- Brokers never write `<ProjectRoot>/.scion`, and the broker check is
  unchanged.

### Why registered projects need it (dispatch analysis)

The hub sends a project path only when the dispatching broker has a provider
`LocalPath` for the project; otherwise `ProjectDispatchLocation` returns the
slug alone (httpdispatcher.go; `brokerUsesSlugProjectDir`).

A project created by `handleProjectRegister` is auto-linked to every
auto-provide broker with no `LocalPath` (`autoLinkProviders`). A register
without a path, or with a path only on the registering broker, leaves no path
on any other provider. On a shared backend those brokers run the identity
check.

### The hub-native workspace leaf

- **At create:** `initHubManagedProject` provisions the workspace through
  `provisionHubNativeWorkspace`. That is the same `provisionSharedWorkspace`
  a shared-workspace git project uses, with no clone:
  - the leaf and `<ProjectRoot>/provision` are created through the confined
    `EnsureLeaf` walk (mode 2775, default ACL);
  - `PrepareStateDir` runs;
  - then `provision.ProvisionShared` runs (shared-plain,
    `SentinelDir = <ProjectRoot>/provision`, the Layout uid/gid), which chowns
    the leaf and writes the sentinel.
  - No workspace lock file is placed in the leaf.
- **Only when this create makes the project root.** An existing root (a tree
  another hub on the same share made, or one left behind) is not touched at
  create, because its identity is unconfirmed until its configuration is
  checked. Its leaf is left to the startup backfill.
- **On failure:** a provisioning failure, or a later storage failure after
  this create made the root, rolls the create back with 503 (no host path,
  no clone wording). The project tree it made is removed through
  `shareddirs.DeleteProjectRoot`.
- **Startup backfill:** the startup pass provisions the leaf of an existing
  hub-native project whose leaf is absent, only after its identity files are
  confirmed. It is best-effort and logs ids only.
- **One predicate (`hubNativeWorkspaceProject`) decides hub-native:**
  - never a git project, an empty-per-agent project, the global project or a
    project linked from a checkout (origin label `linked`);
  - a project with the origin label `hub` qualifies;
  - a project without an origin label (older than the label) qualifies only
    when no provider has a registered local path.

### Hardening

- **Identity files are replaced atomically** (`writeRootFile`):
  - the content goes to a temporary file under the same `os.Root`
    (O_CREATE|O_EXCL|O_NOFOLLOW, 0644) and is fsynced;
  - an absent name is published with `Root.Link`, which fails if anything
    appeared meanwhile;
  - an existing name is replaced with `Root.Rename` after the same
    regular-file re-check;
  - a reader on NFS sees the old content or the new, never a truncated file.

  Symlinks, FIFOs and special files are still refused. Accepted limits: a
  symlink swapped in at the last moment is replaced (never followed); the
  share must support hard links; a crash can leave a uniquely named
  temporary file.
- **Storage errors are logged without paths** (`sanitizedStorageError`): the
  operation and errno only. This covers the identity-seed warnings and the
  workspace provisioning warnings.
- **A client-supplied project ID** on create and register must be a canonical
  lowercase UUID (400 otherwise), before any lookup or write. The hub and the
  CLI generate canonical UUIDs.

## Tests (pkg/hub, project_config_identity_create_test.go; hubshard default shard 5)

Identity at create, clone and register:
- `TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity`
- `TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles`
- `TestCreateProject_SharedBackend_IdentitySeedIdempotent`
- `TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds`
- `TestCloneProject_PerAgentGit_SharedBackend_SeedsProjectIdentity`
- `TestRegisterProject_SharedBackend_SeedsProjectIdentity`
- `TestRegisterProject_ReRegisterExisting_LeavesIdentityUnchanged`

The hub-native workspace leaf:
- `TestCreateHubNativeProject_SharedBackend_ProvisionsWorkspaceLeaf`
- `TestEnsureProjectConfigIdentities_BackfillsHubNativeWorkspace`
- `TestEnsureProjectConfigIdentities_GitProjectsGetNoWorkspaceLeaf`
- `TestEnsureProjectConfigIdentities_LinkedProjectsGetNoWorkspaceLeaf`
- `TestCreateHubNativeProject_LocalBackend_NoShareWrites`
- `TestCreateHubNativeProject_SharedBackend_ProvisioningFailureRollsBack`
- `TestCreateHubNativeProject_SharedBackend_ExistingRootNotProvisioned`
- `TestCreateHubNativeProject_SharedBackend_LaterFailureRemovesCreatedRoot`
- `TestCreateHubNativeProject_SharedBackend_LaterFailureKeepsExistingRoot`
  (the cleanup never removes a project root this create did not make)
- `TestHubNativeWorkspaceProject_Predicate` (the hub-native predicate case by
  case, including a provider lookup error)

Hardening:
- `TestWriteRootFile_AtomicReplaceLeavesNoTempFile`
- `TestCreateAndRegisterProject_RejectNonCanonicalClientID`
- `TestSanitizedStorageError_NoPath`

`TestRequestPathSites` impact: none. No request-path use is added.

## Follow-ups (not in this change)

- No hang guard on the request-path NFS I/O at create (the identity write and
  the workspace provisioning); a hung share can stall the create request. The
  hub-start pass already runs under a deadline.
- `provision.ProvisionShared` logs `host_path` at Info on first provisioning
  (operator log only); consider dropping it.
- Uniform warning texts across the shared-storage helpers.
- A hub-native create over an existing project root still writes the hub
  connection settings into that root's `.scion/settings.yaml`, even when its
  project-id names another project. This is unchanged here.
- Projects without an origin label: the default rule above (no provider local
  path means hub-native) awaits confirmation.
