# Artifact system

Status: design accepted (decisions D1–D20); implementation in phases tracked by ptone/scion#3202. Phase P0 (scaffolding) is ptone/scion#3203, phase P1 (vertical slice) is ptone/scion#3208. This document carries the proposed design, data model, API and UX of the artifact system so the design travels with the code. Prior art: ptone/scion#874 ("Praxis"), ptone/scion#518.

The feature is behind the `hub.artifacts` experiment (default off).

## Decisions referenced below

| # | Decision |
|---|---|
| D1 | An artifact is a published file or folder with a stable, versioned reference. Shared dirs remain for raw file sharing. |
| D2 | The artifact service owns the bytes. It is a net-new service built into the hub binary, designed so it can be split into a standalone server. |
| D3 | Decoupling: own package, own tables, own route prefix, own auth seam; no shared tables with the rest of the hub. |
| D4 | Review starts with CriticMarkup in-document; the core leaves room for deeper edit tracking later. |
| D5 | Share links and retention exist in the schema; artifacts are the collaboration medium for cross-project messaging. |
| D6 | An artifact is a bundle (1..n files, one entry file); a single file is the degenerate case; a version snapshots the manifest. |
| D7 | Principal-owned, home scope, ACL (scope / principal / link grants) with a neutral scope column. |
| D8 | HTML renders in a sandboxed iframe (strict CSP, no same-origin) served by the hub; a separate artifact origin when public links ship. |
| D9 | Publish uploads through the service, with a two-step `create version → upload files → finalize` contract so direct-to-object-store upload is additive later. |
| D10 | Message refs: structured ref in message metadata plus the `scion://artifact/<id>[@<seq>]` URL in the body; refs carry ids only, never a title; access is checked at read time, per reader. |
| D11 | A review is a new version (`kind: review`) carrying CriticMarkup; no overlay object. |
| D12 | Retention: hub setting `artifacts.default_retention_days` (0 = never) plus a per-artifact override; share links always expire. |
| D13 | Standalone auth seam: v1 defines only the Go `Host` interface, constrained to be remotable (strings in, strings and bools out). |
| D14 | The chat `AttachmentStore` coexists in v1; a follow-on moves attachments onto artifacts. |
| D15 | CLI `scion artifact publish/get/list/versions/share`; all agent-callable except `share`. Web: artifact page plus chat chips. |
| D16 | Deleting a project does not delete its artifacts; the home-scope grant may dangle. |
| D17 | Web users can publish, edit and review. |
| D18 | `current_seq` is the latest version of any kind. |
| D19 | Default limits: 32 MiB/file, 256 MiB/bundle, 200 files/bundle, share-link TTL 7 days (max 30). |
| D20 | A review version that changes text outside CriticMarkup is rejected at finalize (`422 unmarked_changes`). |
| D23 | Artifact chips and linkified `scion://artifact/` URLs in chat open an in-place preview dialog (as chat attachments do) with an *Open in artifact viewer* button; no side panel. |

## 5. Proposed design

### 5.1 Shape in one paragraph

A self-contained Go package, **`pkg/artifacts`**, compiled into the hub binary and mounted at **`/api/v1/artifacts/`**. It owns its tables (`artifact`, `artifact_version`, `artifact_file`, `artifact_grant`, `artifact_message_ref`) and nothing else in the hub schema; it stores bytes through `pkg/storage.Storage` under a namespace it alone writes (`hubs/{hub-id}/artifacts/...`); it asks the host three questions through a narrow, string-only interface — *who is this caller?*, *does the caller's credential allow X in scope S at all?* and *may this caller do X in scope S?* — and the hub answers them from its identity context and its existing authz engine. The same package, behind the same interface, can be linked into a standalone server later (D2); nothing in v1 exercises that, but the seam is the design.

```
                 ┌──────────────── hub binary ─────────────────┐
  CLI / agent ──►│  /api/v1/artifacts/*  ──► pkg/artifacts     │
  (REST)         │                            │  Host seam:    │
  web UI ───────►│  (hub auth middleware) ─────┤  Principal()   │──► AuthzService.CheckAccess
                 │                            │  Permits()     │
                 │                            │  Authorize()   │
                 │                            ▼                │
                 │              artifact_* tables   pkg/storage.Storage
                 └──────────────────────────────────┬──────────┘
                                                    ▼
                                      object store  or  local disk (single node)
```

### 5.2 Core concepts

- **Artifact** — a named bundle with a stable id, an owner principal, a home scope (a scion project in v1), an optional publisher-chosen `key` (unique per owner + scope, so re-publishing under the same key appends a version), a title, and a current version pointer.
- **Version** — an immutable snapshot: a manifest of files, each `{path, size, sha256, media_type}`, one designated `entry` path, `kind` (`publish` | `review`), an optional `note`, and `created_by`. Versions are append-only; `seq` is monotonic per artifact. Content is content-addressed, so republishing an unchanged file is a metadata-only write.
- **Grant** — an ACL row `(artifact_id, subject_kind, subject_ref, permission)` with `subject_kind ∈ {scope, principal, link}`. The home scope's members get read through a synthetic grant written at create time, so visibility is always explicit in the table and a standalone deployment needs no scion knowledge to evaluate it. A `link` grant is a share link: a random capability token, stored only as a hash, with its own `expires_at`.
- **Reference** — the string form that travels in messages and docs: `scion://artifact/<id>` or `scion://artifact/<id>@<seq>`. The web page for an artifact is `/projects/<project-id>/artifacts/<id>`.

### 5.3 What stays outside the service

Chat attachments (`AttachmentStore`) keep working unchanged in v1 (D14). Shared dirs are untouched (D1). The service never reads a shared directory: publishing is the client sending bytes.

### 5.4 Host integration points

| Concern | Precedent | How `pkg/artifacts` uses it |
|---|---|---|
| **Own tables (D3)** | The web chat sub-store (`pkg/hub/webchannel_store.go` and its Postgres twin): tables created with `CREATE TABLE IF NOT EXISTS` at `Init`, not Ent entities, with a `webchat_migrations` ledger; the `*sql.DB` comes from the composite store and the store is wired after Ent `AutoMigrate` in `cmd/server_foreground.go`. | `artifacts.NewStore(db, driverName)` with `Init()` running dialect DDL and an `artifact_migrations` ledger. Ent entities in the shared schema are rejected: they would put artifact tables in the hub's migration graph. |
| **Bytes** | `pkg/storage.Storage` (`Upload`, `Download`, `GenerateSignedURL`, `Exists`); the hub's instance always exists (local fallback). | Object store: `GET …/files/{path}` answers `302` to a signed GET. Local provider: the service streams via `Download`. No new storage backend. |
| **Identity** | The hub's `Identity` / `AgentIdentity` from the request context; agent tokens arrive as `X-Scion-Agent-Token` or Bearer. | `Host.Principal(ctx) (kind, ref, homeScope string, ok bool)`. The hub adapter maps the identity to `(owner_kind, owner_ref)` and an agent's project to its home scope. **Owner refs are stable ids (agent UUID, user id), never names.** |
| **Authorization** | `AuthzService.CheckAccess` with capabilities derived from the permission registry. | Registry rows `artifact.read / create / update / delete / manage`. `Host.Authorize(ctx, scopeRef, permission)` is answered by the hub adapter (which alone knows agent token scopes) with `CheckAccess` on the scope's project. The service consults its own `artifact_grant` rows for everything else: the host says "may act in this scope" **or** a grant row says "this principal/link may" — either suffices for read; write/admin need the owner or an explicit grant. Agents use dedicated agent scopes: `project:artifact:read` for `artifact.read`, `project:artifact:write` for `artifact.create` / `artifact.update` (baseline and full agent roles; readonly agents read only). The agent scope is checked before any grant: it decides whether an agent may use the artifact API at all, grants decide which artifacts, so an agent without `project:artifact:read` gets 404 even with an explicit grant. |
| **Routes** | `registerRoutes()` with `guarded(pattern, h)`, which fails closed unless the pattern has a route metadata row. | `/api/v1/artifacts` and `/api/v1/artifacts/` are policy-classified; fine-grained checks happen inside the service. The share-link route `/api/v1/artifacts/shared/` is public and authenticates by token only. The service exposes `http.Handler` + `RegisterRoutes(mux, guard)` so a standalone binary mounts the same handler. |
| **Capability downloads** | The skill download signer is route-specific. | Share links do not reuse it: a `link` grant stores `sha256(token)`; `GET /shared/{token}` looks it up and checks `expires_at`. Sub-resource loads inside a rendered bundle use a short-lived per-view capability minted by the service with its own key. |
| **Cross-project** | Setting `messaging.cross_project_messaging_enabled` (default false, fail closed). | Granting an artifact to another project (`subject_kind: scope`) is allowed only when that setting is on; the grant check at read time does not depend on messaging. |
| **Settings** | Operational settings sections (`pkg/config/opsettings`). | Section `artifacts`: `enabled` (hub-level switch on top of the experiment; a malformed section fails closed), `max_file_bytes` (32 MiB), `max_bundle_bytes` (256 MiB), `max_files` (200), `default_retention_days` (0), `link_default_ttl_hours` (168), `link_max_ttl_hours` (720). |
| **Experiment** | `pkg/experiments/registry.go`. | `hub.artifacts`, web and server layers, default off. Checked on every request (no restart needed): while the experiment is off, or the `artifacts` settings section is disabled or malformed, every artifact route answers 404. The web checks `isFeatureEnabled("hub.artifacts")`. |
| **Message refs (D10)** | Message metadata and the server-built chat attachment map. | Refs travel in the hub-owned metadata key `artifacts`: a JSON array of canonical reference strings `scion://artifact/<id>[@<seq>]`, ids only, at most 10, never a title. The hub admits only refs the sender can read at send time (other send paths strip the key) and persists them in `artifact_message_ref(message_id, artifact_id, seq)` so chat history can render chips. Titles, versions and owners are resolved per reader, under the reader's own credential; an unreadable ref shows the same content as a missing one (see 5.5). |
| **CLI** | `cmd/cli_mode.go` `agentAllowed` (dotted paths, parents not implicit). | `scion artifact publish|get|list|versions|share`; all but `share` agent-callable (D15). |
| **Web** | The pluggable file browser / editor and `markdown-preview.ts`, `code-editor.ts`. | An artifact page hosts the entry file (markdown via `markdown-preview.ts`, HTML in a sandboxed iframe per D8); a bundle data source renders a version's manifest in the file browser. |

### 5.5 Security notes

- **Response equivalence covers content.** For a reader who may not read an artifact, the artifact routes, message reference views and send-time warnings return the same content as for an artifact that does not exist. Response time may differ between an existing but unreadable artifact and a missing one; that can reveal existence only to someone who already holds the artifact id, and `GET /api/v1/artifacts/{id}` behaves the same way. Timing equivalence is not a goal in v1.
- **Artifact references resolve only for request-authenticated callers.** The hub resolves message references (send-time admission, chat views) through the artifact service in process, but only when the identity in the request context is the one the authentication middleware derived from the request's credentials. A context the hub builds for its own decisions resolves nothing, so the service's `Host` only ever sees request-derived identities.

## 6. Data model

```sql
artifact (
  id            text PK,                -- UUID assigned by the service
  scope_kind    text NOT NULL,          -- 'project' in v1 (D7)
  scope_ref     text NOT NULL,          -- project id, never the slug
  owner_kind    text NOT NULL,          -- 'agent' | 'user'
  owner_ref     text NOT NULL,          -- agent id / user id (stable ids, not names)
  key           text NULL,              -- publisher-chosen stable key
  title         text NOT NULL,
  current_seq   int  NULL,
  expires_at    timestamptz NULL,       -- retention (D12)
  created_at, updated_at, deleted_at,
  UNIQUE (scope_kind, scope_ref, owner_kind, owner_ref, key) WHERE key IS NOT NULL AND deleted_at IS NULL
)
artifact_version (
  id text PK, artifact_id FK,
  seq int NOT NULL, kind text NOT NULL,  -- 'publish' | 'review'
  entry_path text NOT NULL,
  note text NULL,
  total_bytes bigint NOT NULL, file_count int NOT NULL,
  created_by_kind text, created_by_ref text, created_at,
  state text NOT NULL,                   -- 'pending' | 'ready' | 'failed' (two-step publish, D9)
  UNIQUE (artifact_id, seq)
)
artifact_file (
  version_id FK, path text,              -- relative path inside the bundle
  size bigint, sha256 text NULL,         -- NULL only for a remote fetch that failed
  media_type text,
  origin text NOT NULL DEFAULT 'upload', -- 'upload' | 'remote' (fetched by the hub at publish time)
  source_url text NULL, fetch_status text NULL, fetch_error text NULL,   -- remote files only
  PRIMARY KEY (version_id, path)
)
artifact_grant (
  id text PK, artifact_id FK,
  subject_kind text,                     -- 'scope' | 'principal' | 'link'
  subject_ref  text,                     -- scope ref / 'kind:ref' principal / link token hash
  permission   text,                     -- 'read' | 'write' | 'admin'
  expires_at timestamptz NULL, created_by_ref text, created_at,
  UNIQUE (artifact_id, subject_kind, subject_ref)
)
artifact_message_ref (                   -- D10: refs only, no title
  message_id text, artifact_id FK, seq int NULL,   -- seq NULL = current version
  PRIMARY KEY (message_id, artifact_id)            -- one ref per artifact per message
)
artifact_migrations (name text PK, applied_at)   -- ledger, as webchat_migrations
```

Ids are `TEXT` in both dialects, so a malformed id from a request path simply matches nothing. SQLite stores timestamps as fixed-width RFC 3339 UTC text, Postgres as `TIMESTAMPTZ`; both keep microsecond precision.

Blob layout: `hubs/{hub-id}/artifacts/blobs/sha256/<aa>/<bb>/<hex>` — content-addressed, immutable, shared across versions and artifacts within one hub. `artifact_file.sha256` is the pointer. Deleting a version marks rows; a periodic sweep removes unreferenced blobs after a grace period.

**Deleted home scope (D16).** When the project behind `scope_ref` no longer exists, the synthetic home-scope grant matches nobody; the owner (implicit admin), principal grants and link grants keep working, and the artifact still appears in the owner's listing and a hub-level *Artifacts* view. The service never joins to the project table. A later "move to scope" (`PATCH /{id}` with a new `scope_ref`, admin-only) re-homes an orphaned artifact.

Permission semantics: `read` = fetch metadata, versions, files; `write` = append versions (including reviews), edit title/key; `admin` = grants, delete. The owner holds admin implicitly. The home-scope default grant is read; scope admins may raise it.

## 7. API surface (REST + JSON, under `/api/v1/artifacts`)

| Method & path | Purpose | Permission |
|---|---|---|
| `POST /` | create artifact (title, key?, scope, entry_path, files manifest) → `{artifact, version(pending), upload: {...}}` | caller may create in scope |
| `POST /?name=<file>[&title=][&scope=]` | **single-file fast path**: the raw body is the only file; creates artifact + v1 `ready` in one request | caller may create in scope |
| `PUT /{id}/versions/{seq}/files/{path}` | upload one file (raw body, `Content-Length`, `X-Content-SHA256`) while the version is `pending` | write |
| `POST /{id}/versions/{seq}/finalize` | verify manifest vs received files, flip to `ready`, advance `current_seq` | write |
| `POST /{id}/versions` | start a new version (same two-step); `kind: publish\|review` | write |
| `GET /{id}` · `GET /{id}/versions` · `GET /{id}/versions/{seq}` | metadata | read |
| `GET /{id}/files/{path}` · `GET /{id}/versions/{seq}/files/{path}` | bytes: streamed on the local provider, otherwise `302` to a signed URL; `?stream=1` makes the hub serve the bytes on any provider. `Content-Disposition`, `X-Content-Type-Options: nosniff` and sandbox headers per D8. `?resolve=clean\|accept` applies the CriticMarkup projection to text files (implies stream, since it is a transform) | read |
| `GET /?scope=…&owner=…&q=…` | list / search | read (filtered) |
| `PATCH /{id}` · `DELETE /{id}` | title/key/expiry; soft delete | write / admin |
| `GET/POST/DELETE /{id}/grants` | ACL, including `POST /{id}/links` to mint a share link | admin |
| `GET /shared/{token}[/files/{path}]` | share-link read; no session required | link grant |

Unauthorized and absent artifacts both answer `404` with the same status and body; nothing distinguishes an absent artifact from an unreadable one. Publishing into a scope the caller may not create in answers `403`.

### Phase P1 implementation notes

- **Fast-path discriminator.** The presence of `?name=` selects the single-file publish, leaving `POST /` with a JSON body free for the two-step create. `name` must be a plain file name; it becomes the entry path. `title` defaults to the name. `scope` defaults to the caller's home scope (an agent's project); users must pass it. An optional `X-Content-SHA256` header is verified against the body.
- **Limits.** `max_file_bytes` comes from the `artifacts` settings section on every request. A declared `Content-Length` over the limit is rejected with `413` before the body is read; an undeclared length is cut off at limit + 1 byte while spooling, and nothing over the limit reaches storage.
- **Agent scopes before grants.** For an agent, the hub adapter requires `project:artifact:read` (reads) or `project:artifact:write` (publish) before the service looks at any grant; agents created from a credential issued before artifacts existed need to be recreated from a current credential before they can use artifacts.
- **Evaluation order.** Every artifact read runs three steps in this order. (1) The credential check (`Host.Permits`): the caller's credential itself must allow the permission in the artifact's home scope. For a user access token that means its permissions and project boundary; for an agent, its dedicated agent scope and, at use time, its delegation chain in its own project (so an agent whose chain no longer allows the permission is refused even for what it owns or was granted). This step is necessary on every path and fails closed. (2) The owner (implicit admin), or host policy for `artifact.read` in the home scope (`Host.Authorize`). (3) An unexpired `artifact_grant` row: a principal grant matching the caller, or a scope grant for a scope the host authorizes. Ownership and grants never reach past step 1. The credential check is the token's project boundary and permissions (and, for an agent, its dedicated scope), not the holder's current project membership, because owner and principal-grant access is designed to survive loss of the home scope. As a consequence, removing a user's project membership does not by itself stop reads through an outstanding access token of artifacts the user owns or was granted; revoke the token for an immediate cut-off. Publishing runs the credential check and host policy for `artifact.create` in the target scope. Share-link reads (a later phase) authenticate by link token instead. Expired (`expires_at`) and soft-deleted artifacts are unreadable to everyone.
- **Delivery.** One code path sets the safety headers for both deliveries: `Content-Disposition` is `inline` only for script-free types (plain text, markdown, CSV, TSV, JSON, YAML, TOML, PNG, JPEG, GIF, WebP) and `attachment` for everything else (HTML, SVG, PDF, unknown); `X-Content-Type-Options: nosniff`; private caching. Streamed responses add `Content-Security-Policy: default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; sandbox` and an `ETag` of the digest. Redirects carry the content type and disposition into the signed URL as response-header overrides.
- **`?stream=1`.** The general "hub serves the bytes" mode. The web page uses it to read text (a cross-origin redirect to the object store would be blocked by `fetch()`); `<img>` and the CLI keep the redirect. On the local provider it changes nothing.
- **Clients follow redirects without credentials.** The CLI fetches the signed URL with a client that carries no hub token or transport credentials.
- **Media type.** Chosen from the file extension first, then a specific declared `Content-Type`, then content sniffing.

## 8. UX

### 8.1 Agent flow (CLI, hub REST)

```
scion artifact publish design.md --title "Artifact system design"
  → scion://artifact/5f1c…  (v1)
    https://<hub>/projects/<project-id>/artifacts/5f1c…
scion artifact publish design.md --key artifacts/design --note "round 4"   # same key → v2
scion artifact publish ./report --entry index.html --title "Q3 report"     # bundle
scion artifact get scion://artifact/5f1c…            # prints the entry file to stdout, or writes it with --out
scion artifact get scion://artifact/5f1c…@2 --out ./v2/
scion artifact versions scion://artifact/5f1c…
scion message @someone "Design ready" --artifact scion://artifact/5f1c…
```

`publish` streams the file(s) to the hub (D9); the hub reads nothing from shared directories. `get` works from any broker and any project the caller has a grant in (D5, D10). Agent-callable verbs are listed in `cmd/cli_mode.go` `agentAllowed`; `artifact.share` is not (D15). Phase P1 ships `publish <file> [--title]` and `get <ref> [--out]`.

**Delivery envelope.** A message with an artifact ref renders to the agent as today's envelope plus the `artifacts` metadata entry and one fetch hint per ref in the body footer, built from the ref alone: `Artifact: v2 - scion artifact get scion://artifact/5f1c…@2`, or `Artifact: current - scion artifact get scion://artifact/5f1c…` for an unpinned ref. The hint carries no title and involves no lookup at delivery; whether the recipient may read the artifact is decided when it runs `get`. This keeps the agent's path to the bytes one command long and visible in the envelope.

### 8.2 Review flow (D4, D11)

1. A human opens the artifact page and clicks **Review**. The entry markdown opens in the file editor with a CriticMarkup toolbar: select text → *Comment* inserts `{>>…<<}`, *Suggest* inserts `{~~old~>new~~}`, plus `{++…++}` / `{--…--}`. The preview renders the marks.
2. **Save review** publishes version N+1 with `kind: review`, `created_by` = the user. The page shows a *review pending* badge; the owning agent receives a system message with the ref and fetch hint (`type: artifact_review`).
3. The agent runs `scion artifact get <ref>` (latest version, i.e. the review), applies or answers the marks, and publishes version N+2 (`kind: publish`). The badge clears. History shows `publish → review → publish`.

**Review versions are complete copies.** A review version holds the full content of the entry file (and the whole bundle manifest) with CriticMarkup inline, not a delta, so `get` of any version is self-contained.

**Resolving marks (the `resolve` transform).** A pure Go package `pkg/artifacts/critic` implements CriticMarkup parsing and three deterministic projections:

| mode | `{++ins++}` | `{--del--}` | `{~~old~>new~~}` | `{>>comment<<}` / `{==hl==}` |
|---|---|---|---|---|
| `raw` (default) | unchanged | unchanged | unchanged | unchanged |
| `clean` — reject all | removed | `del` kept | `old` | removed (highlight text kept, braces removed) |
| `accept` — accept all | `ins` kept | removed | `new` | removed |

Exposed in three places sharing one implementation: API `GET …/files/{path}?resolve=clean|accept` (text media types only), CLI `scion artifact get <ref> --clean | --accept`, and the web preview toggle (*Marks · Clean · Accepted*).

**Unmarked edits are rejected (D20).** On finalize of a review version the service checks `clean(review.<text file>) == parent.<same file>` for every text file (byte comparison after NFC + LF normalisation; files absent from the parent, or non-text files that differ, also count). Any mismatch fails finalize with `422 {reason: "unmarked_changes", files: [{path, hunks: [...]}]}`; the pending version is discarded. Invariant: **`clean(review) == its parent`, always.**

Rule (D18): **`current_seq` is the latest version of any kind.** `get --clean` or `get --kind publish` serve consumers who want the baseline.

### 8.3 Web

- **Artifact page** `/projects/<project-id>/artifacts/<id>[/v/<seq>]`: title, owner, version selector, rendered entry, *Files* (file browser over the version manifest), *Edit* (saves a `kind: publish` version by the user, D17), *Review* (D11), *Share* (links and grants, user-only), *History*. The project *Artifacts* tab has *New artifact* (upload through the two-step API). A hub-level `/artifacts` view lists artifacts the user owns or has grants on, including those whose home project is gone (D16).
- **Renderers** by entry media type: markdown → `markdown-preview.ts` (later with a CriticMarkup extension); text/code → `code-editor.ts` read-only; raster images → `<img>`; HTML bundles → sandboxed iframe whose `src` is the hub file route with a per-view capability so relative sub-resources resolve (D8); CSV/JSON → code view in v1. Phase P1 ships the page with the markdown, text and image renderers; other types are offered as a download.
- **Chat**: chat history and send responses carry each message's refs resolved for the viewer (`messageArtifacts`); a readable ref renders a chip (title · vN · owner), an unreadable or missing one only as unavailable, with no title. Clicking a chip or a linkified `scion://artifact/` URL opens an in-place preview dialog with the same renderer and an *Open in artifact viewer* button (D23; no side panel). Composer: *Attach artifact* picker; drag-drop in the composer still creates a chat attachment in v1 (D14).
- **Project → Artifacts tab**: list with search, owner, updated, review-pending filter.
- Everything is behind experiment `hub.artifacts`.
