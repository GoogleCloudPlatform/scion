# Resource Import Refactor: Factoring, Hub-Level Import, Name Fix, Progress & Performance

**Status:** Reviewed — all open questions resolved (see §4.6); ready for implementation
**Created:** 2026-06-01
**Author:** Agent (template-harness-refactor)
**Related:** [template-import-refactor.md](./template-import-refactor.md), [template-import.md](./template-import.md), [hub-template-admin.md](./hub-template-admin.md), [template-techdebt-cleanup.md](./template-techdebt-cleanup.md), [grove-level-templates.md](./grove-level-templates.md), [agnostic-template-design.md](./agnostic-template-design.md)

---

## 1. Overview

Scion can import two kinds of file-based resources from a remote URL or a workspace
path into the Hub's database + storage backend:

- **Templates** — full agent definitions (`scion-agent.yaml` + `home/` + optional bundled `harness-configs/`).
- **Harness-configs** — harness-specific config directories (`config.yaml` + files).

Today this import is exposed **only at the project level** (Project Settings →
Resources). This document proposes:

1. **Factor** the import mechanism so the template and harness-config paths stop
   duplicating fetch / discover / name / loop logic, and so the CLI and Hub share
   the same name-derivation rules.
2. **Expose import in the global "Hub Resources" view** so global-scoped templates
   and harness-configs can be imported from the web UI (today they can only be
   seeded at boot or via CLI).
3. **Fix the name bug** where importing a single resource from a deep URL
   (e.g. `…/tree/main/antigravity`) yields a cache-hash name instead of `antigravity`.
4. **Improve the import progress UX** — replace the generic spinner with per-resource
   progress ("Importing antigravity (2/5)…").
5. **Make import faster** where it is safely parallelizable.

This builds directly on the prior `ResourceStore` consolidation (the "§7.3" work in
`pkg/hub/resource_store.go`) and the `hub-template-admin.md` draft, which already
sketched a hub-level import endpoint but was never implemented.

---

## 2. Current State (as built)

### 2.1 Server-side import flow

| Concern | Templates | Harness-configs |
|---------|-----------|-----------------|
| HTTP handler | `handleProjectImportTemplates` (`pkg/hub/handlers.go:9314`) | `handleProjectImportHarnessConfigs` (`pkg/hub/handlers.go:9413`) |
| Route | `POST /api/v1/projects/{projectID}/import-templates` | `POST /api/v1/projects/{projectID}/import-harness-configs` |
| Remote import | `importTemplatesFromRemote` (`template_bootstrap.go:248`) | `importHarnessConfigsFromRemote` (`harness_config_bootstrap.go:130`) |
| Workspace import | `importTemplatesFromWorkspace` (`template_bootstrap.go:344`) | `importHarnessConfigsFromWorkspace` (`harness_config_bootstrap.go:170`) |
| Dir discovery | inline (`template_bootstrap.go:288-308`, `374-393`) | `discoverHarnessConfigDirs` (`harness_config_bootstrap.go:216`) |
| Create / sync one | `ResourceStore.Bootstrap` via `templateStore()` | `ResourceStore.Bootstrap` via `harnessConfigStore()` |
| Response | `{ templates: string[], count }` (sync) | `{ harnessConfigs: string[], count }` (sync) |

**What is already well-factored:** `ResourceStore.Bootstrap` (`resource_store.go:121`)
is a clean, kind-generic create-or-sync used by both kinds. The per-kind quirks
(template harness detection, bundled harness-config import, DefaultHarnessConfig
backfill) live behind the `resourcePersistence` interface. This layer is good and
should be kept.

**What is NOT well-factored — the duplication sits one level up**, in the
`import*FromRemote` / `import*FromWorkspace` functions. The two `…FromRemote`
functions are near-identical: auth-token minting → `FetchRemoteTemplate` → discover
dirs → loop `GetBySlug` + create-or-sync → collect names. The two `…FromWorkspace`
functions are likewise near-identical. The dir-discovery "is the root itself a
resource, else scan children" logic is copied **three times** (once per import fn
family, plus `discoverHarnessConfigDirs`).

Concrete inconsistencies caused by the duplication:

- The template remote path falls back to a project `GITHUB_TOKEN` secret
  (`template_bootstrap.go:267-278`); the harness-config remote path **does not**
  (`harness_config_bootstrap.go:140-147`). Same operation, different auth behavior.
- The name bug (below) had to be fixed-or-not in each copy independently.

### 2.2 The name bug (root cause confirmed)

`FetchRemoteTemplate` (`pkg/config/remote_templates.go:164`) caches the fetched
content at `…/cache/remote-templates/<sha256-prefix>` and copies the URL's
**sub-path contents directly into that hash directory** (`remote_templates.go:294-299`).
It returns only that hash path; the original leaf segment (`antigravity`) is discarded.

When the URL points directly at a single resource, the discovery code names the
resource after the cache directory:

- `template_bootstrap.go:293` — `templateDir{filepath.Base(cachePath), cachePath}` → name = `<hash>`
- `harness_config_bootstrap.go:218` — `{filepath.Base(root), root}` → name = `<hash>`

So `…/tree/main/antigravity` imports as e.g. `a1b2c3d4e5f6a7b8` instead of `antigravity`.
(The multi-resource case — URL points at a directory of resources — is correct,
because those names come from `entry.Name()`.)

**The fix already exists in the CLI but isn't shared.** `deriveHarnessConfigName`
(`cmd/harness_config_install.go:236`) correctly does `path.Base(u.Path)` on the
source URL — for `…/tree/main/antigravity` that yields `antigravity`. The Hub import
path simply doesn't call it. The workspace single-resource case has a milder variant
of the same bug (`filepath.Base(templatesDir)` yields e.g. `templates`).

### 2.3 Web UI

- **Project Settings** (`web/src/components/pages/project-settings.ts`) has both
  importers: `renderTemplatesContent` (1853) / `handleSyncTemplates` (1025) and
  `renderHarnessConfigsContent` (1957) / `handleImportHarnessConfigs` (1057). Both
  support URL and workspace modes. Progress is a single Shoelace `<sl-spinner>` with
  a static "Importing … from <url>…" message (`project-settings.ts:1921`, `2028`) —
  **no per-resource detail, no streaming**.
- **Hub Resources** (`web/src/components/pages/settings.ts`, route `/settings`) shows
  global templates and harness-configs via read-only `<scion-resource-list>` tabs.
  **There is no import button at the hub level.**
- SSE infrastructure exists (`web/src/client/sse-client.ts`) for agent/terminal
  streams but is **not** used for import.

### 2.4 Performance characteristics

- Remote fetch is a single repo tarball download + extract (`fetchGitHubTarball`),
  which is reasonable for one repo. Fallback is sparse git checkout.
- Per-resource work in the import loops is **sequential**: each resource does
  `GetBySlug` → `CollectFiles` → upload every file → DB create + update
  (`resource_store.go:130-208`). For a repo with several templates, each containing a
  bundled harness-config (`importTemplateHarnessConfigs`, also sequential), the
  per-file storage uploads dominate wall-clock and are the most likely cause of the
  "taking a while" the user reports.
- `FetchRemoteTemplate` always wipes and re-fetches (`remote_templates.go:178-180`);
  no caching across imports.

---

## 3. Goals & Non-Goals

### Goals
- One shared import pipeline for both resource kinds and both sources (remote /
  workspace), with one name-derivation rule shared with the CLI.
- Correct resource names from deep URLs (the `antigravity` case) and workspace paths.
- Hub-level (global-scope) import from the Hub Resources view, reusing the project
  import UI pattern.
- Per-resource progress in the UI for multi-resource imports.
- Measurable import speedup via safe parallelism.

### Non-Goals
- Changing the `ResourceStore` / `resourcePersistence` design (keep as-is).
- Template/harness-config data-model changes.
- The broader admin template-management page from `hub-template-admin.md` (list /
  delete / lock / clone). This doc covers only the **import** slice of that view.
- Cross-harness translation, template authoring wizard, versioning.

---

## 4. Design

### 4.1 Factor the import pipeline

Introduce a single kind-generic, source-generic import driver that sits **above**
`ResourceStore.Bootstrap` and **replaces** the four `import*From*` functions.

Proposed shape (in a new `pkg/hub/resource_import.go`):

```go
// resourceImportSpec describes one import request, independent of kind/source.
type resourceImportSpec struct {
    kind     storage.ResourceKind // template | harness-config
    scope    string               // project | global
    scopeID  string               // projectID for project scope, "" for global
}

// resourceDir pairs a derived resource name with its on-disk directory.
type resourceDir struct{ name, path string }

// importResources runs the shared pipeline: discover dirs under root, then
// create-or-sync each via the kind's ResourceStore, reporting progress.
func (s *Server) importResources(
    ctx context.Context, root string, spec resourceImportSpec,
    isResourceDir func(string) bool,
    newStore func(dir string) (*ResourceStore, error),
    progress func(ResourceImportEvent),
) ([]string, error)
```

- **Discovery** is unified into one `discoverResourceDirs(root, sourceURL, marker)`
  helper that owns BOTH the leaf-vs-parent decision AND per-branch naming (§4.2.1).
  It is the single place that classifies folders by the presence of a **marker file**
  (`scion-agent.yaml` for templates, `config.yaml` for harness-configs).
- **Fetch + auth** is unified into one `fetchRemoteForImport(ctx, projectID, url)`
  that does the GitHub-App-token → `GITHUB_TOKEN`-secret fallback once, for both kinds
  (fixing the harness-config auth gap).
- **Per-kind differences** stay where they already live: `isResourceDir`
  (`IsScionTemplate` vs `isHarnessConfigDir`) and `newStore` (`templateStore()` vs
  `harnessConfigStore(harness)`), both passed in as small closures.

The four public `import*From*` methods collapse to two thin wrappers
(`importFromRemote`, `importFromWorkspace`) parameterized by kind. The CLI
(`cmd/template_import.go`, `cmd/harness_config_install.go`) is migrated to call the
same `pkg/config`-level name derivation (§4.2) so there is exactly one rule.

**Alternatives considered:**
- **A. Minimal — just fix the bug in place.** Patch the three `filepath.Base` sites,
  leave the duplication. Lowest risk, fastest, but leaves the auth inconsistency and
  the per-copy maintenance burden, and the user explicitly asked to "make sure this
  is well factored" first.
- **B. Shared driver above `ResourceStore` (recommended).** Removes the duplication
  the user is worried about without touching the well-tested `Bootstrap` core.
- **C. Push everything into `pkg/config/templateimport`** as a kind-agnostic library
  the CLI and Hub both call. Most thorough, but the Hub stages (storage upload, DB,
  auth-token minting) are Hub-specific and don't belong in `pkg/config`. Over-reach.

> Recommendation: **B**, with the name-derivation helper extracted to `pkg/config`
> (the one piece genuinely shared with the CLI). See Open Question Q1.

### 4.2 Fix resource naming

Add a shared, tested helper in `pkg/config` (next to `FetchRemoteTemplate`):

```go
// DeriveResourceName returns the intended resource name from a source URL or
// path: the last meaningful path segment (e.g. ".../tree/main/antigravity" -> "antigravity").
func DeriveResourceName(source string) string
```

This generalizes the existing CLI `deriveHarnessConfigName` (which already handles
`file://`, rclone `:backend:path`, and http(s) URLs). The CLI switches to it; the
Hub import path uses it for the single-resource case.

#### 4.2.1 Leaf vs parent detection drives naming

A source URL/path resolves to one of two shapes, distinguished by the **marker file**:

- **Leaf** — the fetched root *itself* contains the marker file. It is a single
  resource. Its name must come from the **URL's leaf segment** (`DeriveResourceName`),
  because the cache directory is a hash and the leaf's contents were copied into it.
  (Workspace leaf: use the real directory's `filepath.Base`, not a hash.)
- **Parent** — the fetched root is a directory whose *children* may each contain a
  marker file. Each valid child is one resource named by its **own directory name**
  (`entry.Name()` — already correct). Children without the marker are **skipped**
  (these are the "invalid resource folders"). The URL leaf here is the parent folder
  name (e.g. `templates`) and must **not** be used as a resource name.

So the URL-derived leaf name is consumed **only on the leaf branch**. Detection itself
works on the fetched cache contents and is unaffected by the hash-directory issue —
the hash only costs us the leaf name. This is why detection + naming belong together in
one `discoverResourceDirs` helper rather than being split across the fetch function and
the caller.

Two layered options for *where* the name ultimately comes from (not mutually
exclusive):

1. **URL/path leaf (primary).** Use `DeriveResourceName(sourceURL)` for the
   single-resource case; use `entry.Name()` for the multi-resource case (already
   correct). Fixes the reported bug directly. For the workspace single-resource case,
   use `filepath.Base(absolutePath)` of the *real* directory (not the cache hash).
2. **Explicit `name:` in the resource config (secondary, optional).** If a
   `scion-agent.yaml` / `config.yaml` carries an explicit name field, prefer it over
   the derived name. This makes a resource self-describing regardless of how it was
   fetched. Requires a small schema addition. See Open Question Q2.

A subtlety for `FetchRemoteTemplate`: rather than re-parsing the URL at every call
site, we can change it to return a small struct (`RemoteFetch{ CachePath, LeafName }`)
so the leaf name is computed once where the URL is known. This is cleaner but touches
the function's signature and all callers. See Open Question Q3.

### 4.3 Hub-level (global) import

Follow `hub-template-admin.md` §2.6 / §3.2: add hub-scoped import that reuses the
shared pipeline with `scope = global`, `scopeID = ""`.

**Endpoint options:**
- **A. Dedicated endpoints** `POST /api/v1/templates/import` and
  `POST /api/v1/harness-configs/import` (body `{ sourceUrl }`). Clean separation;
  must register before the `/{id}` wildcard routes. (This is what `hub-template-admin.md`
  recommended.)
- **B. One unified endpoint** `POST /api/v1/resources/import` with body
  `{ kind, scope, scopeId, sourceUrl }`. Fewer routes, matches the unified pipeline,
  and naturally supports a future "import into project X from the admin view".
- **C. Extend the existing project endpoints** with an optional `scope=global`. Muddies
  project-scoped semantics; rejected in the prior doc too.

> **Decision (Q4): B.** One unified endpoint `POST /api/v1/resources/import` with body
> `{ kind, scope, scopeId, sourceUrl }`. `kind` is always supplied by the UI — the two
> kinds are presented and imported separately (no mixed-kind tree), so the endpoint
> imports exactly one kind per call and scans for that kind's marker file. The response
> is per-kind (imported / skipped / failed names).

**Authorization:** global import requires hub-admin (consistent with the Hub Resources
view already living under the admin nav section). Workspace-path mode is **not**
available for global import (no project workspace to resolve) — URL only, matching
`hub-template-admin.md` §2.6.

**Web UI:** add an "Import" affordance to the Hub Resources tabs
(`web/src/components/pages/settings.ts`) that reuses the same import form component as
project settings, with the scope fixed to global and the workspace-mode option hidden.
Factoring the project-settings import form into a shared `<scion-resource-import>`
component avoids a third copy of the import UI. See Open Question Q5.

### 4.4 Progress UX

The import is currently one blocking POST. To show "what is being imported right now"
we need the server to emit per-resource progress and the client to render it. Options:

- **A. Streaming response (NDJSON / chunked).** The import endpoint streams one JSON
  line per lifecycle event (`discovered`, `importing <name> i/N`, `imported <name>`,
  `done`). The client reads the stream and updates the spinner label. Self-contained
  to the request; no job store. Works with the existing fetch in the UI (switch to a
  streamed reader). **Recommended** — smallest moving-parts cost for the UX win.
- **B. SSE channel.** Reuse `sse-client.ts`. More infra (channel naming, correlation
  IDs); better if multiple viewers should see the same import. Overkill for a
  user-initiated action.
- **C. Async job + poll.** `POST` returns a job ID; client polls `GET …/import-jobs/{id}`.
  Most robust for very long imports (survives navigation), but introduces a job model
  and store. Heavier than the problem warrants today.
- **D. Two-phase (discover then import-each).** `POST …/discover` returns the resource
  list; client then imports each with its own request, driving the progress bar
  client-side. No streaming needed, naturally parallelizable from the client, but
  chattier and reorders error handling.

> Recommendation: **A** for the progress feature; revisit **C** only if imports grow
> large enough to outlive a page session. See Open Question Q6.

The event payload carries per-item `name` and `status` plus an aggregate
`completed`/`total` so the UI can render "Imported 2/5…" and a final summary (imported /
skipped / failed). This also lets us surface per-resource failures instead of a single
opaque error. Under parallelism the aggregate counter is the source of truth for the
progress bar — see "Concurrency + progress reporting" below for the exact event model.

**Concurrency + progress reporting.** Once the per-resource loop is parallelized
(§4.5), items finish out of order, so a discovery-position "i/N" index is no longer
meaningful. The progress model must therefore separate the **aggregate** from the
per-item detail:

- `discovered` — carries `total` (N) and the full list of discovered names.
- `started` — `{ name }` when a worker picks up a resource (lets the UI show which
  items are currently in flight, e.g. "Importing: antigravity, foo, bar…").
- `completed` / `failed` / `skipped` — `{ name, completed, total }`, where `completed`
  is a **monotonic counter** of finished items (any terminal status), assigned by the
  emitter as each finishes — independent of start/discovery order.
- `done` — final summary: imported / skipped / failed name lists.

Because events originate from multiple goroutines, they funnel through a **single event
sink** (one channel, or a mutex-guarded writer) that (a) serializes writes to the NDJSON
stream and (b) owns the `completed` counter so the aggregate is always consistent. The
UI renders a progress bar from `completed/total` plus the in-flight names — both correct
regardless of how many workers run concurrently. This keeps the user-facing "X/Y
imported" accurate under parallelism, which was the specific concern raised in review.

**Multi-node control plane.** Streaming (A) is the *best* fit for a future stateless,
multi-node hub API, not merely a tolerable one. The whole import is a single HTTP
request: the load balancer pins that one connection to one node, and both the work
(fetch → upload → DB) and the progress emission happen in that handler. There is **no
cross-request state** to share between nodes — which is exactly the cost (C) incurs,
where the starting `POST` and the polling `GET`s can land on different nodes and thus
require a shared job store (DB/Redis) reachable by all of them. Two caveats to note for
(A) under multi-node: (1) the LB / reverse proxy must allow long-lived streaming
responses (NDJSON emits frequently, so the connection is never idle, but proxy
read/idle timeouts should still be verified); (2) if the connection drops mid-import,
the client loses visibility into the final state — but the import is **idempotent by
slug** (create-or-sync via `ResourceStore.Bootstrap`), so simply re-running it is safe
and converges. Durable resumability (surviving a navigation/refresh) remains the one
thing only (C) buys; revisit if imports grow long enough to need it.

### 4.5 Performance

Layered, each independently shippable:

1. **Parallelize per-resource import** within one request. The resource loop in the
   import pipeline (and the bundled `importTemplateHarnessConfigs` loop) is
   embarrassingly parallel — each resource is an independent DB row + storage prefix.
   Use a bounded worker pool (`errgroup` with a small limit) over the discovered dirs.
   Per Q7, imports are ≤ ~a dozen items, so a small bound (≈6–8) — or simply all of
   them — is safe; no backend constraint forces a tighter cap. This is the biggest
   expected win for the multi-resource case the user reported as slow.
2. **Parallelize per-file uploads** inside `uploadResourceFiles` with a bounded pool.
   Helps the single-large-resource case (many files). Independent of #1.
3. **Cache the remote fetch.** `FetchRemoteTemplate` currently always re-downloads.
   Add a content/ETag- or commit-SHA-keyed cache with a short TTL so repeated imports
   of the same URL skip the tarball download. Lower priority; the user's slowness is
   more likely uploads than fetch.

> Parallelism interacts with the progress design: events must be emitted safely from
> workers (serialize through a channel). The streaming approach (§4.4 A) handles this
> naturally.

---

## 4.6 Resolved decisions (from review)

- **Q1 → A.** Adopt the shared driver above `ResourceStore` (§4.1 Alternative B);
  collapse the four `import*From*` functions into one kind/source-generic pipeline.
- **Q2 → A.** Name stays source-derived (URL leaf / dir name); no `name:` field in the
  resource config for now. **Follow-up (deferred):** allow renaming a resource record
  in the DB once it's on the hub (a post-import, hub-side rename — distinct from the
  config-declared name in Q2 option B). Tracked in §8 Follow-ups.
- **Q3 → C.** Leaf-vs-parent detection AND naming live together in one shared
  `discoverResourceDirs(root, sourceURL, marker)`; `FetchRemoteTemplate` returns the
  cache path (plus optionally the parsed URL parts). Leaf → `DeriveResourceName(URL)`;
  parent → child dir names; children without the marker file are skipped.
- **Q4 → B (machinery only).** Unified, kind-generic import pipeline behind one
  endpoint `POST /api/v1/resources/import` with body `{ kind, scope, scopeId, sourceUrl }`.
  **`kind` is always supplied by the UI** — templates and harness-configs are always
  presented and imported as **distinct kinds**. There is **no mixed-kind discovery**:
  authors won't create directories mixing both kinds, so each import scans for exactly
  one marker file. (This resolves the mixed-kind half of Q9: **no**.)
- **Q5 → A.** Hub Resources import is **global-scope only** (`scope=global`,
  `scopeId=""`). Project-scoped import stays in each project's settings page. URL is the
  only source mode at the hub level. A project picker in the hub view is a possible
  later addition (§8 Follow-ups).
- **Q6 → A.** Streaming NDJSON progress response. Confirmed multi-node-friendly: the
  import is one request pinned to one node with no cross-request shared state (see §4.4
  "Multi-node control plane"). Re-import is idempotent by slug, so a dropped connection
  is safe to retry. Async-job+poll (C) deferred unless imports need durable resumability.
- **Q7 → concurrency is fine.** Imports are rarely more than ~a dozen items, so a small
  bounded worker pool over the per-resource loop is safe and sufficient. No special
  backend constraints to design around; I'll still read the store/storage layers and
  pick a conservative default bound (≈6–8, or simply "all of them" given the small N).
  Under parallelism the user-facing aggregate "X/Y imported" is driven by a monotonic
  completed-counter funneled through a single event sink (see §4.4 "Concurrency +
  progress reporting").
- **Q8 → A.** This doc covers **only the import slice**. The broader hub admin
  management view (list / filter / delete / lock / clone / archive) is **future work**,
  tracked in [hub-template-admin.md](./hub-template-admin.md); this effort implements
  the import portion of that doc's §2.6 / §3.2 and supersedes those import sections. See
  §8 Follow-ups for the cross-reference.
- **Q9 → one level + stream-reported skips.** In the parent case, scan **only the
  immediate children** of the root for the marker file (no recursion); this matches how
  `.scion/templates` and `.scion/harness-configs` are laid out today. Recursion is a
  later addition only if a real use case appears. Child folders lacking the marker file
  are **skipped and reported** via the progress stream (a `skipped` event per folder)
  and in the final summary, e.g. "Imported 3, skipped 2 (no scion-agent.yaml)".

---

## 5. Open Questions / Ambiguities (all resolved — see §4.6)

> All nine questions below were reviewed and resolved with the project owner; the
> resolutions are consolidated in §4.6 and folded into the design above. They are kept
> here for traceability of what was decided and why.


1. **Factoring depth (Q1).** Adopt the shared driver above `ResourceStore`
   (Alternative B in §4.1), or just patch the bug minimally (A) and defer the
   refactor? The task framing ("make sure this is well factored *then* …") suggests B.
2. **Explicit name field (Q2).** Should a resource's config (`scion-agent.yaml` /
   `config.yaml`) be able to declare its own `name`, taking precedence over the
   URL/dir-derived name? Adds self-describing resources but is a (small) schema change.
3. **Where naming lives (Q3, reframed).** Given leaf-vs-parent detection (§4.2.1), the
   URL leaf name is only used on the leaf branch. Options: (A) re-derive at the call
   site and pass into discovery; (B) have `FetchRemoteTemplate` return a `LeafName`
   (misleading — fetch can't know if it grabbed a leaf or a parent); (C) keep fetch
   returning the cache path (+ optionally the parsed URL parts) and let the shared
   `discoverResourceDirs(root, sourceURL, marker)` own all naming. Lean: **C**.
4. **Hub import endpoint shape (Q4).** Dedicated per-kind endpoints (A, matches prior
   doc), or one unified `/api/v1/resources/import` (B, matches the unified pipeline)?
5. **Hub import scope selector (Q5).** At the hub level, import global-only, or also
   offer "import into project X" from the Hub Resources view (per `hub-template-admin.md`
   §2.6, which had a Global/Grove selector)?
6. **Progress mechanism (Q6).** Streaming NDJSON response (A) vs async job + poll (C).
   Streaming is lighter; async survives navigation. Which fits expected import sizes?
7. **Concurrency safety (Q7).** Any known constraint in the store / storage backend
   (Postgres pool limits, GCS rate limits, non-reentrant code) that caps how
   aggressively the per-resource loop can be parallelized?
8. **Scope of this doc vs hub-template-admin.md (Q8).** Should this doc also pull in
   the read/list/delete admin view, or stay strictly the import slice and let the rest
   of `hub-template-admin.md` proceed separately?
9. **Scan depth & skipped-folder reporting (Q9).** (Mixed-kind discovery resolved =
   no, per Q4.) In the parent case, do we scan only one level of children (current
   behavior) or recurse to find resources nested deeper? And how do we report
   invalid/skipped folders (children lacking the kind's marker file) so the user
   understands what was and wasn't imported?

---

## 6. Phases

Ordered so each phase is independently shippable and the user-visible fixes land early.

### Phase 0 — Name fix (smallest, highest signal) — **Status: DONE**
- Extract `DeriveResourceName` into `pkg/config`; unit-test the `antigravity`,
  `/tree/main/...`, archive, rclone, and bare-repo cases.
- Use it in the three Hub single-resource discovery sites (and the workspace variant).
- Migrate the CLI `deriveHarnessConfigName` to the shared helper.
- Result: `…/tree/main/antigravity` imports as `antigravity`. No API/UI change.

**Implementation notes (Phase 0):**
- `DeriveResourceName` added in `pkg/config/resource_name.go` (+ tests in
  `pkg/config/resource_name_test.go`): handles `file://`, rclone `:backend:path`,
  http(s)/bare-`github.com` URLs, archives, and plain paths.
- Hub remote leaf naming now uses it: `template_bootstrap.go`
  (`importTemplatesFromRemote`) and `harness_config_bootstrap.go`
  (`discoverHarnessConfigDirs` gained an optional `sourceURL` param; the remote
  caller passes the URL, the workspace caller passes `""` so the real dir's
  `filepath.Base` is still used). The workspace template leaf already used the real
  directory's base name and was left unchanged.
- CLI `deriveHarnessConfigName` (`cmd/harness_config_install.go`) now delegates to
  `config.DeriveResourceName`; existing CLI tests still pass.
- Drive-by: removed a pre-existing unused `storage` import in `template_bootstrap.go`
  that was breaking `go build ./...` on the branch.

### Phase 1 — Factor the import pipeline — **Status: DONE**
- Add `pkg/hub/resource_import.go` shared driver (discover + fetch/auth + loop).
- Collapse the four `import*From*` functions into kind-parameterized wrappers.
- Fix the harness-config `GITHUB_TOKEN` auth gap by sharing the fetch path.
- Pure refactor: behavior-preserving, covered by existing + new tests.

**Implementation notes (Phase 1):**
- New `pkg/hub/resource_import.go` holds the shared, kind/source-generic driver:
  - `resourceImportKind` bundles the per-kind knobs (`noun` for logs/errors,
    `isResourceDir` marker check, `newStore` factory). Built via
    `Server.templateImportKind()` / `Server.harnessConfigImportKind()`. The
    harness-config `newStore` loads `config.yaml` to resolve the harness type and
    skips a dir if that fails.
  - `importFromRemote` / `importFromWorkspace` are the two source-generic entry
    points; `fetchRemoteForImport` does the GitHub-App-token → `GITHUB_TOKEN`-secret
    fallback once for **both** kinds (closing the harness-config auth gap);
    `discoverResourceDirs` owns the leaf-vs-parent decision + naming (leaf →
    `DeriveResourceName(URL)`, workspace leaf → real `filepath.Base`, parent →
    child dir names, non-marker children skipped); `importResourceDirs` runs the
    create-or-sync loop.
- The loop now calls `ResourceStore.Bootstrap(..., force=true)` directly per dir,
  dropping the redundant pre-loop `GetBySlug` + create-vs-sync branch (Bootstrap
  already does that internally; for a re-import the prior code always force-synced).
- The four `import*From*` methods in `template_bootstrap.go` /
  `harness_config_bootstrap.go` are now one-line wrappers over the shared driver.
  Removed the now-dead `discoverHarnessConfigDirs`, `importHarnessConfigDirs`, and
  the `harnessConfigDir` type, plus the duplicated three-times inline discovery.
  Workspace path validation is unified on the stricter `filepath.Rel` check
  (the template path previously used a looser `strings.HasPrefix`).
- Tests: existing template/harness-config import + workspace tests still pass;
  added `TestImportHarnessConfigsFromRemote_WithProjectGithubToken` guarding the
  newly-shared `GITHUB_TOKEN` fallback for harness-config remote import.
- Note: `pkg/config` has pre-existing env-dependent test failures in this sandbox
  (hub-context env vars set); unrelated to this phase, which touches only `pkg/hub`.

### Phase 2 — Hub-level import (backend + UI) — **Status: DONE**
- Add the hub import endpoint(s) (shape per Q4) with admin authz, URL-only.
- Factor the project-settings import form into a shared `<scion-resource-import>`
  component; mount it in the Hub Resources tabs with scope=global.

**Implementation notes (Phase 2):**
- New unified endpoint `POST /api/v1/resources/import` (`handleResourcesImport`
  in `pkg/hub/handlers.go`, registered in `server.go`) with body
  `{ kind, scope, scopeId, sourceUrl }` per Q4→B. It resolves the kind to the
  shared `resourceImportKind` (template / harness-config) and runs the Phase-1
  `importFromRemote` driver. URL is the only source mode (no workspace) — matching
  the hub-level import design.
  - **Global scope** (`scope=global`, `scopeId=""`) is hub-admin-only: it runs
    `authzService.CheckAccess` on an ownerless/parentless `{Type: template |
    harness_config}` resource with `ActionCreate`, which grants only on the admin
    bypass (or an explicit hub-wide policy). projectID is `""`, so no project
    GitHub auth is attempted.
  - **Project scope** is also supported (the endpoint is genuinely unified, per
    the "future import into project X" note): `scopeId` is required and authz
    mirrors the per-project import handlers via the shared `authorizeProjectImport`
    helper. The existing per-project endpoints (which still own workspace-mode
    import) are unchanged.
- `fetchRemoteForImport` now treats `projectID == ""` as an unauthenticated
  (global) fetch — it skips both the GitHub App token mint and the project
  `GITHUB_TOKEN` secret fallback, which are project-scoped.
- Web: new shared `web/src/components/shared/resource-import.ts`
  (`<scion-resource-import>`) renders the mode toggle + source input + button +
  status, posting to the per-project endpoint (project scope, URL/workspace) or
  the unified endpoint (global scope, URL-only), and emits a `resource-imported`
  event so the host refreshes its list. It replaces the two inline import blocks
  in `project-settings.ts` (removing the now-dead sync/hcImport state + handlers)
  and is mounted in the Hub Resources tabs (`settings.ts`) with `scope=global`.
  The Hub Resources page is already admin-gated at the router level
  (`ADMIN_ROUTES`), so the import UI shows there unconditionally; the backend
  still enforces admin.
- Tests: `pkg/hub/resource_import_handler_test.go` covers global import as admin
  (lands a global-scoped template), global forbidden for a member (403, nothing
  imported), invalid kind (400), and missing sourceUrl (400). Web typecheck +
  build pass.
- Note: progress UX is intentionally still the simple spinner/success/error model
  here — per-resource streaming progress is Phase 3.

### Phase 3 — Progress UX — **Status: DONE**
- Server emits per-resource progress events (mechanism per Q6).
- Client renders "Importing <name> (i/N)…" and a final imported/skipped/failed summary,
  in both project and hub import surfaces.

**Implementation notes (Phase 3):**
- Server progress model (`pkg/hub/resource_import.go`): added `ResourceImportEvent`
  (+ `ResourceImportEventType` constants `discovered`/`started`/`completed`/`failed`/
  `skipped`/`done`/`error`) and an `importProgressFunc` callback threaded through
  `importFromRemote` / `importFromWorkspace` / `importResourceDirs`. `discoverResourceDirs`
  now also returns the child folders it skipped (those lacking the kind's marker file)
  so they can be reported. The per-resource loop emits `discovered` (total + names),
  a `skipped` event per non-marker folder, `started` per resource, then `completed`/
  `failed` with a **monotonic completed counter** (assigned as each item finishes, so it
  stays correct once Phase 4 parallelizes the loop), and a final `done` summary. The
  marker filename moved onto `resourceImportKind.marker` for skip reasons. The four thin
  `import*From*` wrappers pass `nil` progress, preserving their signatures (and all
  existing tests).
- Streaming transport (`pkg/hub/handlers.go`): new `streamImport` helper writes NDJSON
  (one JSON event per line via `json.Encoder`, flushed) behind an `http.Flusher`, with
  events serialized through a mutex (parallel-safe for Phase 4). It is **content-
  negotiated**: `handleResourcesImport` and both per-project import handlers stream only
  when the client sends `Accept: application/x-ndjson`; otherwise they return the
  existing buffered JSON summary (keeping the CLI / existing tests working). All
  pre-flight validation + authz runs before the stream is committed, so those still
  return proper HTTP status codes; failures reached after the stream starts (fetch
  failure, nothing found) surface as an in-band `error` event.
- Web (`web/src/components/shared/resource-import.ts`): the shared import component now
  requests NDJSON, reads the stream, and renders an `<sl-progress-bar>` driven by
  `completed/total` plus the in-flight resource name(s) ("Importing antigravity
  (2/5)…"), then a final "Imported X, skipped Y, failed Z" summary with the skipped/
  failed name lists. Falls back to the single-JSON path when the response isn't streamed.
  Registered `sl-progress-bar` in `web/src/client/main.ts`. Works for both the project
  settings and Hub Resources surfaces (same component).
- Tests: `pkg/hub/resource_import_handler_test.go` gains `…_StreamsProgress` (asserts the
  discovered→completed→done sequence and that the resource still lands) and
  `…_StreamErrorEvent` (a post-stream failure is reported as an `error` event, not an
  HTTP error). Full `pkg/hub` suite + `go vet` + `go build ./...` pass; web
  typecheck + build pass.

### Phase 4 — Performance — **Status: DONE**
- Parallelize the per-resource import loop (bounded pool), then per-file uploads.
- Optional: cache `FetchRemoteTemplate` by commit SHA / ETag.
- Validate against a multi-template repo; record before/after timings.

**Implementation notes (Phase 4):**
- **Per-resource loop parallelized** (`importResourceDirs`, `pkg/hub/resource_import.go`):
  the discovered dirs now import through a bounded `errgroup` pool
  (`resourceImportConcurrency = 6`). Each worker writes its outcome into a
  per-index slot, so the returned/reported `imported` and `failed` lists stay in
  discovery order (compacted via the new `compactNames`); the monotonic
  `completed` counter is an `atomic.Int64` so the aggregate carried on each
  progress event is consistent across workers. This is exactly the model Phase 3
  set up (single mutex-serialized event sink + monotonic counter), so no progress
  changes were needed — the streaming handler's mutex already makes
  `importProgressFunc` goroutine-safe.
- **Per-file uploads parallelized** (`uploadResourceFiles`,
  `pkg/hub/storage_helpers.go`): files upload through a bounded `errgroup` pool
  (`fileUploadConcurrency = 8`), each writing its manifest entry into its own slot
  so the manifest preserves input order without locking. Storage backends (GCS /
  local FS) are safe for concurrent uploads to distinct object paths. This speeds
  up the single-large-resource case and, because every import path (templates,
  harness-configs, bundled, project + global) routes through
  `ResourceStore.Bootstrap → uploadResourceFiles`, all of them benefit.
- **Bundled harness-config loop parallelized** (`importTemplateHarnessConfigs`,
  `pkg/hub/template_bootstrap.go`): a bounded `errgroup`
  (`bundledHarnessConfigConcurrency = 4`, kept small because this runs *inside* a
  per-resource import goroutine, so effective concurrency is the product of the
  two pools).
- **Test infra:** `mockStorage` (`pkg/hub/bootstrap_test.go`) gained a mutex
  guarding its `objects` map, since the import path now exercises storage
  concurrently (and must be clean under `go test -race`).
- **Concurrency bounds rationale (Q7):** imports are ≤ ~a dozen items, so small
  bounds are both safe and sufficient; the SQLite store already runs WAL +
  `busy_timeout(5000)` + a 4-conn pool, so concurrent DB writes serialize
  gracefully, and real backends (Postgres / GCS) are concurrency-safe.
- **Tests:** added `TestImportTemplatesFromWorkspace_ParallelManyTemplates`
  (12 templates × multiple files, asserts every resource imported exactly once,
  order preserved, all files uploaded). The import-path tests pass under
  `go test -race`; the full `pkg/hub` suite passes (the full suite under `-race`
  exceeds the default 10-min binary timeout, so race-cleanliness was verified on
  the import-path subset).
- **Validation / timings:** measured with a latency-injecting storage wrapper
  (25 ms/upload) importing 12 templates × 4 files = 48 uploads. Sequential lower
  bound ≈ 1.2 s; parallel actual ≈ 63 ms — a **~19× speedup** for the
  multi-resource case the user reported as slow. (Measured with a throwaway
  timing test; not committed to avoid a flaky wall-clock assertion in CI.)
- **Deferred (optional):** caching `FetchRemoteTemplate` by commit SHA / ETag was
  left out. It is explicitly optional and lower-priority in §4.5 (the user's
  slowness is dominated by uploads, now parallelized), and a content/ETag cache
  touches the fetch signature + invalidation semantics with little expected
  payoff here. Tracked as a follow-up if repeated same-URL imports become common.

---

## 7. Key Files

| Area | File |
|------|------|
| Remote fetch + name derivation | `pkg/config/remote_templates.go`; new `DeriveResourceName` |
| Template import | `pkg/hub/template_bootstrap.go` |
| Harness-config import | `pkg/hub/harness_config_bootstrap.go` |
| Shared create/sync core (keep) | `pkg/hub/resource_store.go` |
| New shared import driver | `pkg/hub/resource_import.go` (new) |
| HTTP handlers + routes | `pkg/hub/handlers.go` (`~4526`, `9314`, `9413`) |
| CLI import | `cmd/template_import.go`, `cmd/harness_config_install.go` |
| Project import UI | `web/src/components/pages/project-settings.ts` |
| Hub Resources UI | `web/src/components/pages/settings.ts` |
| Shared resource list | `web/src/components/shared/resource-list.ts` |
| New shared import form | `web/src/components/shared/resource-import.ts` (new) |
| Nav / routes | `web/src/components/shared/nav.ts`, `web/src/client/main.ts` |

---

## 8. Follow-ups (out of scope for the phases above)

- **Hub-side resource rename.** Let an admin rename a template / harness-config record
  in the DB after it's on the hub (updates the display name / slug without re-import).
  Raised during review of Q2; distinct from a config-declared name. Needs a rename
  endpoint + slug-collision handling + UI affordance on the resource detail/list page.
- **Remaining `hub-template-admin.md` slice (future work).** The admin resource
  management view — list with filters/sorting/pagination, delete (with file cleanup),
  lock/unlock, clone, archive, usage indicators — as specified in
  [hub-template-admin.md](./hub-template-admin.md) §2.2–2.5, §2.4, §4, and Phases 1–2/4.
  **This doc supersedes that doc's import sections** (§2.6 hub-level import, §3.2 import
  endpoint, Phase 3) — when picking up the admin view, treat import as already designed
  here and build the management actions around it. Cross-reference back to this doc from
  `hub-template-admin.md` when that work starts.
