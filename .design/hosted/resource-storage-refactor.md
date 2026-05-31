# Resource Storage & Cache Refactor

## Status
**Largely implemented** — 7.1 (single read path / remove co-located shortcut)
and 7.2 (thin content-addressed cache, option (a)) have **landed**. 7.3 (shared
resource-store abstraction) has now **landed**: the content-hash consolidation,
the shared hub-side bootstrap **mechanics** (3a), the hub-side `ResourceStore`
(`ResourceRecord` + per-kind persistence, 3b), and the broker-side kind-generic
`Resolver` (the generalized `Hydrator` + `resolveLocalResource`) are all in.
**Step 4 (harness-config consume path) has landed** — harness-configs now
hydrate from the storage backend on any broker, exactly like templates, closing
the Section 4 asymmetry. Skills onboarding (step 5) and 7.4 (identifier/layout
cleanup) remain proposals. See the per-section **Status** notes below.

## 1. Purpose

Scion stores several *file-based resource types* — today **templates** and
**harness-configs**, soon **skills** and likely others — as directories of files
that are used when provisioning agents. We want the Hub to be a **stateless
control plane**: the durable source of truth for these resources should live in a
**configured storage backend** so that any node of a scaled-out, multi-broker
control plane can serve and consume them.

To make them usable operationally we currently also keep **local filesystem
copies** at various nodes, and templates additionally have a content-addressed
**hydration cache** on brokers.

### 1.1. Deployment modes (settled constraints)

Two primary modes drive this design, and a key decision is now settled
(see Section 7.1):

- **Workstation mode** — single user, hub running locally and co-located with the
  broker. The storage backend is the **local filesystem**, *not* GCS. Live-editing
  a resource directly from the workspace is **not required**.
- **Cloud / hosted mode** — distributed, multi-broker. The storage backend is
  **GCS**. Live-editing is **not required** here either.

**Decision:** Because live-edit is unneeded in both modes, we **remove the
co-located live-edit shortcut entirely**. This means the "source of truth" is
precisely *the configured `storage.Storage` backend* — local FS in workstation
mode, GCS in hosted mode — and there is a **single read path** through that
backend in all topologies. "GCS as source of truth" is the hosted-mode instance
of the more general "storage backend as source of truth."

This document:

1. Maps the *actual* current state of resource storage, the GCS sync step, and
   the cache (Section 2–4).
2. Answers the specific question: **how is the cache involved in the
   synchronization step?** (Section 5 — short answer: it isn't, and that
   mismatch is the source of the confusion.)
3. Proposes a prioritized set of cleanup/refactor opportunities to make GCS the
   unambiguous source of truth and to generalize the pattern across resource
   types (Section 6–8).

## 2. Current state: the three file locations (and why they're conflated)

The word "cache" is overloaded. There are actually **three distinct on-disk
locations** plus GCS, and only one of them is really a cache:

| # | Location | Who writes it | Who reads it | Role |
|---|----------|---------------|--------------|------|
| A | **Import-source dir** — `~/.scion/templates/<name>/`, `.scion/templates/<name>/`, `~/.scion/harness-configs/<name>/`, template-bundled `harness-configs/` | Humans, `scion … import`, embeds seeding | Hub at bootstrap; co-located broker at provision | **Source / seed**, not a cache |
| B | **GCS** — `gs://<bucket>/templates/<scope>/<scopeId>/<slug>/…`, `…/harness-configs/<scope>/<scopeId>/<slug>/…` | Hub bootstrap/import upload; CLI push | Broker hydrator (templates); CLI pull | **Intended source of truth** |
| C | **Broker hydration cache** — `~/.scion/cache/templates/<contentHash>/…` | `templatecache.Hydrator` | `hydrateTemplate` at provision | **Read-through cache** (templates only) |

The metadata/registry (UUID, slug, scope, `ContentHash`, file manifest, storage
URI) lives in a fourth place: the **Hub database** (`store.Template`,
`store.HarnessConfig`).

Key references:
- Import → DB+GCS: `pkg/hub/template_bootstrap.go:36` (`BootstrapTemplatesFromDir`),
  `:233` (`bootstrapSingleTemplate`), `:114` (`syncExistingTemplate`).
- Storage paths (slug-based, **not** UID-based):
  `pkg/storage/storage.go` `TemplateStoragePath` / `HarnessConfigStoragePath`.
- Broker cache: `pkg/templatecache/cache.go` (content-addressed, LRU),
  `pkg/templatecache/hydrator.go` (download + verify + store).
- Provision-time read: `pkg/runtimebroker/handlers.go:767` (`hydrateTemplate`).

## 3. The synchronization step (import → GCS)

For templates the "sync" the user described is the **bootstrap/import** path,
and it is a one-directional push from location **A → B (+DB)**:

```
local dir (A)
  → transfer.CollectFiles()        # walk, per-file SHA-256, size, mode
  → api.NewUUID()                  # UID assigned HERE, in the DB record only
  → storage.TemplateStoragePath(scope, scopeId, slug)   # GCS path uses SLUG
  → stor.Upload(objectPath, file)  # one PUT per file, to GCS (B)
  → computeContentHash(files)      # aggregate hash of sorted per-file hashes
  → store.UpdateTemplate(...)      # DB: files manifest + ContentHash + ACTIVE
```

Re-sync (`syncExistingTemplate`) short-circuits when the recomputed aggregate
hash matches the stored `ContentHash` (`template_bootstrap.go:133`), otherwise
re-uploads and **reconciles** GCS by deleting objects no longer in the manifest
(`:188`). Harness-configs have a structurally identical path
(`harness_config_bootstrap.go`, `bootstrapSingleHarnessConfigScoped`,
`syncExistingHarnessConfig`).

Notably, the upload/finalize/download URL helpers are **already shared** between
templates and harness-configs in `pkg/hub/storage_helpers.go`
(`generateUploadURLs`, `verifyAndFinalizeFiles`, `generateDownloadURLs`,
`rewriteLocal*URLs`). That is the seed of the generalization proposed below.

## 4. The cache (location C) and how resources are consumed

At provision time the broker calls `hydrateTemplate`
(`runtimebroker/handlers.go:767`):

1. **Co-located shortcut** — if the broker is co-located with the hub
   (`conn.IsColocated`) and the template exists in the local templates dir (A),
   return that path directly. **GCS and the cache are bypassed entirely.**
2. Otherwise, `Hydrator.HydrateWithHash` / `Hydrate`:
   - cache hit by `(ID, contentHash)` or by hash alone → return cached path (C);
   - else request signed **download URLs** from the Hub, download changed files
     (incremental vs any older cached version), verify each file's hash, then
     `cache.Store(...)` under `<contentHash>/` and return that path.

The Hub serves downloads as **signed GCS GET URLs** (`storage_helpers.go:95`),
so the broker pulls **B → C** directly; the Hub stays out of the data path
(local-storage dev mode proxies via `file://`-rewrite endpoints).

**Asymmetry — harness-configs have no equivalent consume path.** There is a
hub-side store and download API and a `hubclient` download client, but the
broker resolves harness-configs at provision time from the **local filesystem**
(`config.FindHarnessConfigDir`, precedence template > grove > global) or from
versioned settings — *not* by hydrating from GCS. There is no
`harnessconfigcache`, no provision-time hydrator. In practice the GCS copy of a
harness-config is **write-only for a remote broker**: it is uploaded by
bootstrap/CLI but never read back during provisioning. A remote broker that
lacks the local directory simply won't find the config. This is the single
biggest gap relative to the stated goal ("use them effectively when
provisioning agents from any node").

## 5. So how is the cache involved in synchronization? (direct answer)

**It isn't.** The content-addressed cache (C) participates only in the
*provision-time read* path on a remote broker. The import→GCS *sync* path
(A→B+DB) never touches it. The confusion arises because three different
filesystem locations all read like "a local copy," and because:

- On a **combo / co-located hub-broker** (the common dev and single-node case),
  provisioning takes the **co-located shortcut** and reads directly from the
  import-source dir (A). So in that topology **both GCS (B) and the cache (C)
  are pure overhead** — written during bootstrap, never read during provision.
- The cache only earns its keep on a **remote broker** that is *not* co-located
  with the hub and provisions the *same* template *repeatedly*. That is exactly
  the topology that is least exercised today.

In other words, the cache's validation/eviction/incremental-download machinery
(`cache.go` ~520 lines, `hydrator.go` ~290 lines) currently pays off in a narrow
slice of deployments, while adding indirection that every reader has to reason
about.

## 6. Problems worth fixing

1. **GCS is not unambiguously the source of truth.** The co-located shortcut
   reads from the import-source dir (A), so on the dominant topology GCS is
   never the read path. "GCS is the source of truth" is aspirational, not
   actual. Any divergence between A and B is invisible until a remote broker
   hits it.

2. **Identifier indirection.** A UID (DB UUID) is minted at import, but GCS
   paths are **slug-scoped** (`templates/<scope>/<scopeId>/<slug>/`) and the
   cache is **content-hash-keyed**. The UID is used only as a DB key and API
   handle. Three identifier spaces (UID / slug+scope / contentHash) for one
   resource. Mutable slug paths in GCS also mean a rename or scope change
   silently orphans objects unless reconcile runs.

3. **Cache complexity vs. value.** Content-addressing + secondary
   `templateID → contentHash` index + LRU eviction with shared-hash refcount
   checks + incremental per-file download is a lot of surface for a read-through
   cache that, in co-located deployments, is never read. Hashing happens 3–4×
   per resource (collect, finalize-verify, hydrate-verify, optional recompute).

4. **Templates and harness-configs are ~80% duplicated but only partially
   shared.** `storage_helpers.go` is shared; bootstrap/sync, DB models
   (`HarnessConfig` mirrors `Template`, reuses `TemplateFile`), handlers, and
   scope-path functions are copy-paste-parallel. Adding **skills** as a third
   near-identical copy would triple the maintenance surface.

5. **No generalized consume path.** Templates hydrate from GCS; harness-configs
   don't; skills will need *something*. Without a shared resource-fetch
   abstraction, each new resource type reinvents (or, like harness-configs,
   omits) the broker-side fetch.

## 7. Proposal

### 7.1. Single read path through the storage backend; remove the co-located shortcut

> **Status: ✅ Implemented.** The import-source-dir shortcut is gone and
> provisioning resolves through the connection's storage backend in every
> topology. See **Implementation notes** at the end of this subsection.

Per the settled decision in Section 1.1, live-edit is not required in either
mode, so:

- **Remove the co-located live-edit shortcut** in `hydrateTemplate`
  (`runtimebroker/handlers.go:777`) outright. Provisioning always resolves a
  resource **through the configured storage backend**, never by reading the
  import-source dir (A) directly. One read path for every topology.
- Treat the import-source dir (A) strictly as a **seed**: bootstrap reads it once
  to populate the storage backend + DB, and it is never a provision read path.
- The "co-located zero-copy" case falls out naturally from the storage
  abstraction rather than from a special case:
  - **Workstation mode** — the backend *is* the local filesystem
    (`pkg/storage/local.go`), so resolution is already a local path read; no
    network, no signed URLs, no special-casing in the broker. To avoid a
    pointless double-copy, the local backend's bucket directory can simply *be*
    the canonical resource directory, so "seed" and "store" coincide on one
    machine.
  - **Hosted mode** — the backend is GCS; the broker hydrates via signed URLs
    (and a cache, per 7.2) exactly as today, but with no co-located bypass to
    reason about.

This removes `TemplatesDir` branching from the provision path and makes the
storage backend the *only* thing that defines where truth lives.

**Implementation notes (as landed):**

- `hydrateTemplate` (`runtimebroker/handlers.go`) no longer reads the
  import-source dir. It resolves through the connection's storage backend: a new
  `resolveLocalTemplate` reads the resource **directly from the local backend's
  on-disk location** when the backend is local, otherwise it hydrates from
  remote storage (signed URLs + cache, per 7.2).
- `HubConnection.TemplatesDir` was **removed** and replaced with
  `HubConnection.LocalStorage storage.Storage` (set only for a co-located
  connection whose backend is local FS). `IsColocated` was **kept** — it is still
  used by the heartbeat loop, not for resource resolution.
- The broker reaches the on-disk path via a new
  `storage.LocalStorage.ObjectFSPath(objectPath)` accessor, gated behind a small
  `localObjectResolver` interface assertion in the broker. (That assertion is a
  pragmatic seam; its proper home is the `ResourceResolver` /`LocalDirBackend`
  in 7.3, which should subsume it rather than each kind re-asserting.)
- Plumbing: `runtimebroker.ServerConfig.ColocatedStorage` is populated from
  `hubSrv.GetStorage()` in `cmd/server_foreground.go` only in co-located mode.

**Follow-up (not yet done):** co-located resolution still issues a metadata
`Templates().Get` over **loopback HTTP** even though hub and broker share a
process and a DB. It's cheap, but the deeper cleanup is to let co-located mode
talk to the hub **in-process** (shared store/service) instead of looping back
through HTTP — that benefits far more than template resolution and is tracked as
its own item, orthogonal to this doc.

### 7.2. Collapse the cache to a thin content-addressed store (or drop it)

> **Status: ✅ Implemented — option (a), thin CAS.** See **Implementation
> notes** at the end of this subsection.

The broker hydration cache is now a **hosted-mode-only concern**: in workstation
mode the storage backend is local FS, so resolution is already a local read and
the cache adds nothing — it should be **disabled when the backend is local**.
That alone removes the cache from the entire single-node story and narrows its
job to "avoid re-downloading from GCS on a remote broker."

For that remaining hosted case, two viable directions; recommend **(a)** unless
benchmarks justify the current machinery:

- **(a) Thin CAS.** Keep `~/.scion/cache/<type>/<contentHash>/` as a simple
  content-addressed store: `Get(contentHash) → path | miss`,
  `Put(contentHash, files) → path`, size-bounded LRU. Drop the secondary
  `templateID → hash` index (the Hub already returns `contentHash` with
  metadata, so the broker can look up by hash directly) and drop incremental
  per-file download (download the whole resource on miss; resources are small
  dir trees, and CAS already dedupes across versions that share content). This
  removes `GetAnyVersion`, `GetFileHashes`, per-file diffing, and the
  store-nil-under-second-ID path.
- **(b) No broker cache.** Hydrate to a temp dir per provision and delete after
  the agent's home is composed. Simplest possible; acceptable if provisioning
  is infrequent relative to download cost. Measure first.

Either way, **hash verification stays** (integrity), but the *number of hashing
passes* drops to: collect-at-import (1) + verify-at-hydrate (1).

**Implementation notes (as landed — option (a)):**

- `templatecache.Cache` is now keyed solely by content hash:
  `Get(contentHash) → path|miss` and `Put(contentHash, files) → path`, with a
  size-bounded LRU. The secondary `templateID → hash` index, `GetByHash`,
  `GetAnyVersion`, `GetFileHashes`, the per-file incremental download path, and
  the store-nil-under-second-ID path were all **removed**. Because entries are
  keyed by hash, eviction no longer needs shared-hash refcounting.
- `Hydrator` simplified to: metadata → `cache.Get(hash)` → download-whole-on-miss
  → verify each file's hash → `cache.Put(hash, files)`.
- "Disable cache when backend is local" is realized structurally: the local
  direct-read path (7.1) never constructs a hydrator call, so the cache is only
  exercised on the remote/GCS path.

**Follow-ups (not yet done, low priority):**

- The shared cache is still *constructed* in pure workstation mode (the cache
  dir is created) even though nothing reads it there. Could be built lazily only
  when a remote/GCS connection exists.
- There is no self-healing reconcile between the on-disk `<contentHash>/` dirs
  and the index, so a crash mid-`Put` can leave the `TotalSize` accounting
  slightly off (it self-corrects on the next `Get` miss for that hash). A
  `Reconcile()` that walks `basePath` vs. the index at startup would make this
  crash-safe.

### 7.3. Generalize to a single resource-store abstraction

> **Status: ✅ Landed.** All sub-items are in.
> - **✅ Content-hash consolidation** (the `ResourceStore`-owns-the-hash item
>   below) — the hub's duplicate `computeContentHash` is gone; all call sites
>   delegate to `transfer.ComputeContentHash`.
> - **✅ Shared bootstrap mechanics** (3a): the duplicated hub-side upload loop,
>   the template stale-object reconcile loop, the `FileInfo`→manifest conversion,
>   and the parallel slug-scoped path functions are shared, `kind`-keyed helpers
>   — `storage.ResourceStoragePath`/`ResourceStorageURI` (keyed by
>   `storage.ResourceKind`) and `toResourceFiles`/`uploadResourceFiles`/
>   `reconcileResourceStorage` in `pkg/hub/storage_helpers.go`.
> - **✅ The interfaces themselves** (3b): the hub-side `ResourceStore`
>   (`pkg/hub/resource_store.go`) collapses the parallel `bootstrapSingle*`/
>   `syncExisting*` routines onto one kind-generic `Bootstrap`. Rather than a
>   risky DB-model union, a shared `ResourceRecord` is a *view* over the common
>   fields and a per-kind `resourcePersistence` bridges it to the concrete
>   `store.Template`/`store.HarnessConfig` (mutating the loaded model in place so
>   the typed `Config` payload survives). The broker-side `Resolver`
>   (`pkg/templatecache/resolver.go`) is the generalized download-and-cache
>   algorithm parameterized by a `resourceFetcher` over `hubclient.Templates()`/
>   `HarnessConfigs()`; `Hydrator` is now a thin template wrapper. The 7.1
>   `localObjectResolver` seam folded into the kind-aware `resolveLocalResource`
>   (one `ObjectFSPath` assertion for all kinds).
> - **✅ Harness-configs are now usable from any broker** (step 4): they hydrate
>   from the storage backend through the shared `Resolver`, closing the Section 4
>   asymmetry. A full `Template`/`HarnessConfig` → single DB-model convergence was
>   intentionally **not** done (the adapter `ResourceRecord` delivers the
>   abstraction at far lower blast radius); it can follow with 7.4 if desired.
>
> **Open follow-ups (next session):**
> - `make ci` passes; `make ci-full` (golangci-lint) has **not** been run to
>   completion. A scoped `golangci-lint --new-from-rev=main` surfaced one
>   pre-existing `errcheck` on the unchecked `f.Close()` in
>   `uploadResourceFiles` (`pkg/hub/storage_helpers.go`, carried over from the 3a
>   extraction) — fix to `_ = f.Close()` and run `make ci-full` before opening a
>   PR.
> - Consider a dedicated harness-config `Resolver` unit test with a mock
>   `HarnessConfigService` (Phase C reused the template Hydrator tests for the
>   shared core; the harness-config adapter path is currently covered only by the
>   broker fall-back guards, not an end-to-end download).

Introduce one set of interfaces that templates, harness-configs, and skills all
implement, replacing the per-type copies.

```go
// A storable, file-based resource type.
type ResourceKind string // "template" | "harness-config" | "skill" | ...

// Hub-side: registry + GCS sync. Backed by store + storage.Storage.
type ResourceStore interface {
    // Seed/import a local dir into GCS+DB (idempotent by content hash).
    Bootstrap(ctx, kind, name, dir, scope, scopeID) (*ResourceRecord, error)
    Get(ctx, kind, ref) (*ResourceRecord, error)        // ref = id|slug|name
    SignedDownload(ctx, rec) ([]FileURL, error)         // GCS GET URLs
    SignedUpload(ctx, rec, files) ([]FileURL, error)    // GCS PUT URLs
    Finalize(ctx, rec, manifest) error                   // verify + activate
}

// Broker-side: resolve a resource to a local path for provisioning.
type ResourceResolver interface {
    Resolve(ctx, kind, rec) (localPath string, err error)
}
```

- `storage_helpers.go` already provides the signed-URL/finalize primitives —
  lift them behind `ResourceStore`.
- `bootstrapSingleTemplate` / `bootstrapSingleHarnessConfig` collapse into one
  `Bootstrap` parameterized by `kind` and a path-function lookup.
- `Template` and `HarnessConfig` DB models converge on a shared
  `ResourceRecord` (kind, id, name, slug, scope, scopeId, harness?, contentHash,
  files, storageURI, status). Keep thin type-specific wrappers only where the
  config payload genuinely differs (`TemplateConfig` vs `HarnessConfigData`).
- The broker gets **one** `ResourceResolver` with pluggable backends
  (`GCSCacheBackend`, `LocalDirBackend`) used for *all* kinds. This is what
  finally gives harness-configs (and skills) a real provision-time fetch from
  GCS, closing the gap in Section 4. `LocalDirBackend` subsumes the 7.1
  `localObjectResolver`/`ObjectFSPath` seam.
- **`ResourceStore` must own the one canonical content-hash function.**
  > **Status: ✅ Pulled forward.** The hub's hand-rolled `computeContentHash`
  > (`template_handlers.go`) was a *separate implementation* from the canonical
  > `transfer.ComputeContentHash` used by the broker-side `Hydrator`, the
  > `transfer` collector, and the `hubclient` manifest builder — and it diverged
  > on empty input (the hub hashed an empty byte stream; `transfer` returns
  > `""`). The hub function is now a thin adapter that converts
  > `[]store.TemplateFile` → `[]transfer.FileInfo` and delegates to
  > `transfer.ComputeContentHash`, so every call site shares one implementation
  > and the hub/broker can no longer drift. A regression test
  > (`pkg/hub/content_hash_test.go`) pins the adapter to `transfer` and the
  > empty-input contract. Ownership lives in `transfer` until a `ResourceStore`
  > abstraction subsumes it.

  Originally: `Hydrator.computeContentHash` and the hub's
  `transfer.ComputeContentHash` (collect/finalize) were *separate call sites*
  that had to stay byte-identical or cache keys would silently diverge.
  Consolidating onto a single implementation resolves the hash-canonicalization
  open question (§9) and was cheap enough to pull forward ahead of the full
  abstraction.

### 7.4. Identifier cleanup (optional, higher blast radius)

> **Status: ⏳ Not started.** Unchanged by the 7.1/7.2 work — GCS paths are
> still slug-scoped, so a rename/scope change still orphans objects unless
> reconcile runs. Stage after 7.3 as a dedicated migration.

Consider making the **GCS object path content-addressed or UID-addressed**
instead of slug-scoped, so the source of truth is immutable and renames/scope
changes don't orphan objects:

- `…/<kind>/<uid>/<file>` (stable, requires DB to map slug→uid; reconcile-free),
  or `…/<kind>/blobs/<contentHash>` with a small per-resource manifest object.
- Keep slug/scope purely as *query/display* metadata in the DB.

This is the cleanest long-term shape but touches storage layout migration; stage
it after 7.1–7.3 land.

## 8. Suggested sequencing

1. ✅ **Document + decide** (this doc): confirm GCS-as-truth, pick cache
   direction (chose **7.2a**), confirm the co-located live-edit shortcut is
   removed.
2. ✅ **Remove the co-located shortcut / single read path** (7.1) and ✅ **slim
   the cache** (7.2a). *Done together, for templates, ahead of the abstraction* —
   the original plan slotted cache-slimming as step 4 (after the resolver), but
   it was low-risk to land directly alongside 7.1 with templates as the proving
   ground.
3. ✅ **Extract `ResourceStore` / `ResourceResolver`** (7.3) with templates as the
   first adopter; no behavior change. Validated the abstraction against the
   existing, most-complete path, and folded in the 7.1 `localObjectResolver` seam.
   - ✅ **3a. Shared bootstrap mechanics** — kind-keyed storage-path helpers
     (`storage.ResourceStoragePath`/`ResourceStorageURI` + `ResourceKind`) and
     `toResourceFiles`/`uploadResourceFiles`/`reconcileResourceStorage` in
     `pkg/hub/storage_helpers.go`, adopted by both template and harness-config
     bootstrap/sync. (Plus the earlier ✅ content-hash consolidation.)
   - ✅ **3b. The interfaces themselves** — hub-side `ResourceStore` +
     `ResourceRecord` (adapter view, not a DB-model union) in
     `pkg/hub/resource_store.go`, and the broker-side kind-generic `Resolver` in
     `pkg/templatecache/resolver.go` (`Hydrator` now wraps it). Built on the 3a
     primitives; landed alongside step 4.
4. ✅ **Onboard harness-configs** to the broker-side resolver — harness-configs
   now hydrate from the storage backend on any broker, matching templates
   (`hydrateHarnessConfig` + `StartOptions.HarnessConfigPath`; hub stamps
   `HarnessConfigID`/`Hash` in `populateAgentConfig`).
5. ⏳ **Onboard skills** as the third `kind` — should be nearly free if 7.3 holds.
6. ⏳ **(Optional)** identifier/layout cleanup (7.4) as a dedicated migration.

Out-of-band cleanup surfaced during the 7.1/7.2 work (orthogonal to this doc):
co-located mode still talks to the hub over **loopback HTTP** rather than
in-process (see 7.1 follow-up); and ✅ the canonical content-hash function is now
**owned in one place** (`transfer.ComputeContentHash`; see 7.3) — the hub's
duplicate was removed, eliminating the hub/broker drift risk.

## 9. Open questions

- **Co-located live-edit**: ~~hard requirement?~~ **Resolved** — not required in
  either deployment mode; the shortcut is removed (Section 1.1 / 7.1).
- **Workstation seed = store?**: in workstation mode, should the local storage
  backend's bucket dir literally *be* the user's resource dir (no copy), or a
  separate managed dir that bootstrap copies into? (Affects whether edits to the
  user's dir need an explicit re-seed.)
- **Cache value**: do we have (or can we gather) numbers on remote-broker
  re-provision frequency and resource sizes? That decides 7.2a vs 7.2b — now a
  purely hosted-mode question.
- **Hash canonicalization**: ~~`ComputeContentHash` ordering/exclusions are
  assumed identical broker-side and hub-side.~~ **Resolved** — the hub's
  duplicate implementation was removed; all hub call sites now delegate to the
  same `transfer.ComputeContentHash` the broker uses, with a regression test
  guarding against future drift (see §7.3). A shared `ResourceStore` should take
  over ownership when it lands, but the two can no longer diverge in the
  meantime.
- **Versioning**: all current docs defer versioning. A content-addressed GCS
  layout (7.4) makes immutable versions almost free — worth keeping in view so
  the refactor doesn't foreclose it.
- **Skills specifics**: do skills need scope/precedence semantics identical to
  templates/harness-configs, or their own (e.g. per-agent vs per-project)? This
  affects whether `scope`/`scopeID` stays in the shared record or moves to a
  per-kind policy.

## 10. Related documents

- `.design/hosted/hosted-templates.md` — current template storage design (impl).
- `.design/hosted/harness-config-hub-storage.md` — proposed, mirrors templates.
- `.design/hosted/sync-design.md` — workspace sync via the same signed-URL
  pattern; shares `pkg/transfer`.
- `.design/hosted/hosted-architecture.md` §4.2 — direct-to-storage + incremental
  sync principle.
- `.design/agnostic-template-design.md` / `.design/decouple-templates.md` —
  future template/harness-config composition; orthogonal to storage but should
  be kept compatible with the shared `ResourceRecord`.
