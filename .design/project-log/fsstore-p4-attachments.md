# Project storage phase 4: attachments on artifact transport; scratchpad projection through the layout authority

**Date**: 2026-10-23
**Issue**: ptone/scion#5827 (sub-issues ptone/scion#5828–#5832, tracking ptone/scion#5805)
**Builds on**: phase 1 (`pkg/projectfs`, ptone/scion#5806)

## Summary

On a hub with a Cloud Run control plane, a GKE broker and Filestore, chat
attachments landed on the hub instance's disk in two places: the attachment
store under `~/.scion/attachments/`, and a scratchpad copy under the local
`project-configs/` tree. No agent mounts either one. The hub also refused to
stage into an NFS-backed scratchpad at all. This phase puts attachments on the
artifact transport when the hub has object storage. It makes the scratchpad
copy a projection resolved through the layout authority, so the copy lands in
the tree agents mount, on every topology.

Changes:

- **`pkg/projectfs` (additive)**: `AttachmentRelDir`, `AttachmentContainerPath`,
  `SharedDirTargets`, `WriteAttachment`, the channel constants and
  `ScratchpadName`. Container paths of projected attachments are computed
  only here, from `Mount.Target`.
- **Artifacts by default with object storage (ptone/scion#5828)**:
  - `hub.attachment_artifacts` resolves on when the hub's resource storage
    is an object store, unless an override is stored or the experiments
    document is malformed (`deploymentExperimentDefault`). The admin view
    reports the effective default.
  - The compiled default stays off.
  - The legacy `LocalDiskAttachmentStore` stays hub-private at
    `~/.scion/attachments`. Its `Get` reads through an `os.Root` with a
    regular-file and same-file check.
- **Projection through the authority (ptone/scion#5829)**:
  - `resolveScratchpad` resolves the scratchpad with the hub's shared-dir
    Layout. The NFS refusal and the home-directory fallback are deleted.
  - Staging writes through `Resolved.Open`. It creates the NFS leaf when it
    is missing, and never vivifies a local one.
  - Agent `--attach` ingest reads through `Resolved.Open(false)`.
  - Nothing is projected on a Cloud Run Service whose shared dirs are on
    local disk.
  - An existing file at a staging destination is reused only when it is a
    regular file with the attachment's exact bytes.
- **Envelope and container paths (ptone/scion#5830)**:
  - The inbox path is the primary attachment entry. The projection is a
    second entry, tagged in the envelope's `attachment_mounts` map
    (path -> shared dir), with an "also visible to sibling agents at …"
    body line.
  - Legacy staged paths are tagged through
    `StructuredMessage.AttachmentMounts`.
  - `GET /projects/{id}/shared-dirs` returns a response-only `target` per
    dir. Terminal drag-drop uses it, and the CLI probes
    `projectfs.SharedDirTargets`.
- **Plugins (ptone/scion#5831)**:
  - Telegram, Discord and Google Chat write downloads through
    `projectfs.WriteAttachment`, on the scratchpad that
    `runtime.ResolveSharedDirHostPath` resolves (now carrying the
    authority's `Resolved`), at `.attachments/<channel>/<id>/<name>`.
  - The legacy `~/.scion/projects/<slug>/downloads` fallback is removed: a
    download with no project is refused.
  - The operator `downloads_path` override stays.
- **Retention (ptone/scion#5832)**, inside the artifact maintenance pass, at
  most hourly:
  - Projections under every `.attachments/<channel>/` are swept after
    `messaging.attachment_inbox_retention_days` (default 7), on every
    topology. `.attachments` and each channel are opened without following
    links, as their own `os.Root`. On a workstation this is the one
    intended behaviour change. Downloads from chat history are unaffected.
  - Legacy blobs are swept after `gc_grace_hours`, and only when orphaned or
    when the hub has object storage. Nothing is swept while a referencing
    message is pending or deferred. Candidates come from the store layout:
    a directory is removed only when the attachment's metadata row records
    exactly that location. Any failed lookup keeps the blob.
  - On a hub without object storage, a blob that a sent message references
    is kept until the message is deleted, which is lifecycle work.

## Decisions

- **No `Layout.Realize` yet**: container paths come from `Mount.Target`
  plus the helpers above. When phase 3 adds `Realize`, it must keep `Target`
  unchanged for bind and pod mounts.
- **Envelope shape**: the inbox path comes first and the projection second,
  tagged with its mount. The hub does not model per-runtime mount sets. The
  delivered envelope keeps `attachments` as a list of strings and adds
  `attachment_mounts`.
- **Canonical blobs stay hub-private**: a blob served to other users never
  lives in a mount agents can write, and project-less (DM) uploads have no
  project tree.
- **Projection audience**: a projection must not be readable by anyone who
  cannot read the message, and every agent that mounts the scratchpad can
  read it.
  - With artifacts on, read at send time, only messages in a project topic
    thread are projected (`projectionAudienceIsProject`). Direct and group
    conversations get inbox delivery only.
  - With artifacts off (a workstation hub with local resource storage),
    there is no inbox delivery, so staging is kept for every send. A direct
    message's attachment is then readable by the project's other agents.
    This is documented as a known limitation of that configuration.
    Defaulting attachment artifacts on for local storage would remove it;
    that is noted on ptone/scion#5828.
  - `stageChatAttachments` is the only projection site, and its only caller
    is `sendAgentRouted`, which every chat v2 send (scheduled sends
    included) goes through. A source-level test pins both facts.
- **Retention keys**: projections use the inbox retention, blobs use
  `gc_grace_hours`. History downloads keep working on a workstation hub.
- `cmd/message.go` is the `scion` binary, not sciontool. `pkg/projectfs`
  imports only `pkg/api`, `pkg/shareddirs` and the standard library, so no
  separate dependency-free subpackage was needed.

## Container-path literals (for the docs and cleanup phase)

Outside tests and `pkg/projectfs`, the scratchpad container-path literal
remains only in comments that cite internal design documents
(`pkg/store/*`, `pkg/api/types.go`, `pkg/ent/schema`, `pkg/hubclient`,
`cmd/reincarnate.go`). None of them computes a path.

## Tests

- `pkg/projectfs`: rel dir and container paths (default, in-workspace,
  worktree), invalid components, targets, and `WriteAttachment` on local and
  nfs, including refusal of a symlinked `.attachments`.
- `pkg/messaging`: projection ordering and mount tag; the undeliverable,
  withheld, plain, legacy staged tag and legacy apply cases.
- `pkg/hub`:
  - NFS scratchpad resolution and projection into the uuid tree, for both
    shared-dir and workspace storage, with no stale-local fallback.
  - Ingest through the authority, symlink refusals, fail-closed resolution,
    and the Cloud Run local refusal.
  - Projection only for project topic threads: the DM and group cases, a
    send-time experiment toggle, and the source-level single-call-site pin.
  - The default-on resolution, and legacy `Get` through `os.Root`.
  - Projection and blob sweeps, covering a symlinked `.attachments`, stray
    files, missing rows, misplaced blobs, failed lookups and pacing.
  - Verified staging reuse, and the shared-dirs `target`.
- `pkg/runtime`: the resolved result carries the authority's `Resolved`.
- `cmd`: scratchpad target probing and the staging dir layout.
- web: the terminal upload target comes from `target`.
- extras: the download layout, and refusal of a download with no project.
