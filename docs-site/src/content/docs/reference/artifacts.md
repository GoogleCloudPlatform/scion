---
title: Artifacts
description: The scion artifact command and the /api/v1/artifacts routes for publishing files with stable references.
---

:::caution[Experimental]
Artifacts are behind the `hub.artifacts` experiment, which is **off by default**. While it is off, or when the hub's `artifacts` settings section is disabled, every `/api/v1/artifacts` route answers `404` and the web UI shows no artifact pages. An admin enables it under **Admin → Server Config → Experiments** (see [Experiments](/scion/reference/experiments/)). Artifacts require Hub mode.
:::

An **artifact** is a published file or folder (a *bundle*) with a stable reference, `scion://artifact/<id>`, that works from any runtime broker, in any project the reader can access, and in the web UI. The hub stores the bytes, so a reader never needs access to the publisher's filesystem or shared directories.

This page covers what is available today: publishing files and folders, versions, fetching, and the artifact page. Message references, review and share links are planned.

Each artifact has numbered **versions**. A version is an immutable snapshot of the bundle: its files, one **entry** file (the one the web page opens and `get` prints), an optional note, and who published it. Publishing again under the same `--key` adds a version; the latest one is the artifact's **current** version, and `scion://artifact/<id>@<seq>` names one version for good.

## Ownership and access

- An artifact is **owned** by the agent or user that published it (recorded by stable id, never by name). The owner can always read it.
- It is **homed** in a project: an agent's own project, or the project a user publishes into. Members who may read in that project may read the artifact.
- Anyone else gets `404` — the same answer as for an artifact that does not exist, so responses do not reveal whether an artifact exists.
- Deleting the project does not delete its artifacts; the owner keeps access.
- Only the owner (or a principal the owner gave write access) can add versions.

## `scion artifact`

Available in every CLI mode, including agent mode.

### `scion artifact publish <file|folder>`

Publishes a file or a folder and prints its reference, the new version and the URL of its web page. An agent's artifact is homed in the agent's project; a user's in the hub project the current checkout is linked to.

```text
$ scion artifact publish design.md --title "Artifact system design" --key design
scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d  (v1)
https://hub.example.com/projects/<project-id>/artifacts/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
$ scion artifact publish design.md --key design --note "round 2"
scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d  (v2)
https://hub.example.com/projects/<project-id>/artifacts/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
$ scion artifact publish ./report --entry index.html --title "Q3 report"
```

- `--title <title>`: Artifact title, set when the artifact is created; a new version under an existing key keeps the title (the CLI prints a note). Default: the entry file's name.
- `--key <key>`: A stable key of your choosing. Publishing again under the same key (same publisher, same project) adds a version to that artifact instead of creating a new one. Files unchanged since the current version are not uploaded again.
- `--note <text>`: A note describing the version.
- `--entry <path>`: The entry file of a folder, relative to it. Default: `index.html`, `index.md` or `README.md` at the top of the folder, or the only file.

A folder is published as all the regular files under it, keeping their relative paths. Files and folders whose names start with `.` are left out, and symbolic links inside the folder are refused (the file or folder you name may itself be a link). The CLI sends each file's SHA-256, and the hub rejects bytes that do not match. Limits (hub settings): 32 MiB per file (`artifacts.max_file_bytes`), 256 MiB per version (`artifacts.max_bundle_bytes`) and 200 files per version (`artifacts.max_files`).

If the entry file is Markdown, the hub fetches the remote images it references while publishing (see [Images in Markdown artifacts](#images-in-markdown-artifacts)). An image that could not be fetched does not fail the publish; the CLI prints a warning for it on stderr.

### `scion artifact get <ref>`

Fetches an artifact. Without `--out` it writes the entry file to stdout; with `--out` it writes a single file, or every file of a bundle.

```text
$ scion artifact get scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d > design.md
$ scion artifact get scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d@1 --out ./docs/
$ scion artifact get scion://artifact/7a2b...@2 --out ./report-v2/
```

- `<ref>`: `scion://artifact/<id>`, `scion://artifact/<id>@<seq>` for a specific version, or a bare `<id>`.
- `--out`, `-o <path>`: For a single file, write to this file instead of stdout; if the path is an existing directory, the file is written into it under its own name. For a bundle (several files, or one file inside a folder), the directory to write every file into (created if needed), keeping relative paths.
- `--force`: Replace files that already exist under `--out`. Without it, `get` refuses to replace an existing file.

Every file is checked against the size and SHA-256 the hub recorded at publish time before it is written, to stdout or to disk, and moved into place atomically; a mismatch fails without writing that file. With `--out`, `get` writes only plain relative names (none starting with `.`), refuses to write through a symbolic link below the `--out` directory, and replaces an existing file only with `--force`.

### `scion artifact versions <ref>`

Lists an artifact's versions, newest first, with kind, publish time, publisher, file count, size and note. The current version is marked `*`.

```text
$ scion artifact versions scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
  REF                                                  KIND     PUBLISHED          BY             FILES  SIZE     NOTE
* scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d@2  publish  2026-10-06 18:20Z  agent:<id>     1      4.1 KiB  round 2
  scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d@1  publish  2026-10-06 17:02Z  agent:<id>     1      3.9 KiB
```

## Images in Markdown artifacts

Images in a Markdown artifact are fetched at publish time and served from the hub, so opening an artifact never makes the hub contact another server.

- **Remote images.** When a version whose entry file is Markdown is published, the hub fetches each absolute `https` image the entry references (`![alt](https://...)`, reference-style images, and `<img src="https://...">`) once, and stores it in the version as a file at `_remote/<sha256 of the URL>`. Images are taken from the first 2 MiB of the entry; if the entry is larger, the response and the CLI add one warning saying that later images were not fetched. Fetched images count toward the version's file and size limits (`max_files`, `max_bundle_bytes`); images past either limit are not fetched, with a warning. If the entry holds more image references than the hub reads, a warning says that later images were not fetched. Image URLs written in code are fetched as well, though they are not shown. Only PNG, JPEG, GIF and WebP images are kept; SVG and other types are not. An image that cannot be fetched (for example a non-`https` URL, an address the hub does not fetch from, a timeout, a missing image or another type) is recorded as failed, the publish still succeeds, and the response and the CLI list one warning per such image. Publishing a new version fetches again.
- **Which URLs.** An `<img src>` is read as the browser reads it, and a reference-style image uses the first definition of its label. A URL is fetched only in a plain form: `http` or `https`, a host name (no IP literal in brackets), an optional port, no user name or password, and no spaces or parentheses. Other images are not fetched and show as "not fetched" in the preview.
- **Reading a remote image.** `GET /api/v1/artifacts/{id}/versions/{seq}/files/_remote/<hash>` follows the same access checks as any other file. A fetched image is served with its detected type; a failed one answers `404` with the header `X-Artifact-Remote-Status: failed`. The version's file manifest lists each remote image with `origin: "remote"`, its `sourceUrl` and its `fetchStatus` (`ok` or `failed`).
- **Reserved names.** Files may not be published under `_remote/`.

Settings (in the `artifacts` section): `remote_images_enabled` (default `true`), `remote_image_max_count` (images fetched per version, default `32`; further images are not fetched and get one warning), `remote_image_max_bytes` (per image, default 5 MiB, at most `max_file_bytes`), `remote_image_fetch_timeout_s` (per image, default `10`) and `remote_image_total_budget_s` (all images of one version, default `30`, between the fetch timeout and 60 seconds; images are fetched while the publish or finalize request is open, and the hub keeps that request open for the budget plus a margin). `remote_image_max_count` may not exceed `max_files`; when the two remote image caps are not set, they follow a lower `max_files` or `max_file_bytes`. A settings write with an invalid value, including a remote image cap above the matching file limit or a budget below the fetch timeout, is refused. A stored document with an invalid remote image value turns remote images off and is logged; any other invalid stored value disables the artifact service.

## HTML artifacts

An artifact whose entry file is HTML (a single page or a small site published as a folder) is shown in a sandboxed frame. The page's scripts run, but in an isolated origin with no access to the hub page around it, to your session or to the hub's API.

- The frame loads the version through a **view URL**, `/api/v1/artifacts/view/<capability>/<entry>`, issued to a reader of the artifact and valid for 30 minutes. Relative links in the bundle (`img/chart.png`, `css/site.css`) resolve under it, so the page loads its own files. The view URL names one version of one artifact and gives access to nothing else; it is not a share link.
- Every view response carries a `Content-Security-Policy` with a `sandbox` directive, so the page stays isolated even when the URL is opened directly. The page may load scripts, styles, images, fonts and media only from its own files under the view URL; it cannot make network requests from script, embed other frames or plugins, submit forms, open windows or navigate the page around it.
- **Remote images are not loaded in HTML artifacts; include them in the bundle.** When an HTML entry references images by absolute `http(s)` URL, the publish response and the CLI print that warning, and the viewer shows it above the frame.

## Web page

`/projects/<project-id>/artifacts/<id>` shows the artifact's title, owner, version and reference, and renders the entry file: Markdown as formatted text, text and code (including JSON, YAML, CSV) in a read-only editor, and PNG, JPEG, GIF and WebP images inline. Other types (including HTML, SVG and PDF) are offered as a download. The Markdown preview loads no images from other hosts: they appear as their alt text. Inline (data:) images are shown.

## API

All routes are under `/api/v1/artifacts` and use the hub's usual authentication (session, user access token or agent token). Errors use the hub's JSON error envelope.

| Method and path | Purpose |
| :--- | :--- |
| `POST /api/v1/artifacts?name=<file>[&title=<title>][&scope=<project-id>]` | Publish the raw request body as a new single-file artifact. `scope` defaults to the caller's project (agents); users must set it. Optional header `X-Content-SHA256` (hex) is verified. Returns `201` with the artifact, its first version and any publish `warnings`. |
| `GET /api/v1/artifacts/{id}` | The artifact and its current version, including the file manifest (`path`, `size`, `sha256`, `mediaType`, and for remote images `origin`, `sourceUrl` and `fetchStatus`; see [Images in Markdown artifacts](#images-in-markdown-artifacts)). |
| `GET /api/v1/artifacts/{id}/files/{path}` | A file of the current version. |
| `GET /api/v1/artifacts/{id}/versions/{seq}/files/{path}` | A file of version `seq`. |
| `POST /api/v1/artifacts` (JSON manifest) | Start a two-step publish: a new artifact with a pending first version, or, when `key` names an artifact the caller already published in the scope, a new pending version of it. Returns `201` with the artifact, the pending version and `upload.required`, the paths to upload. |
| `POST /api/v1/artifacts/{id}/versions` (JSON manifest) | Start a new pending version of an artifact. |
| `PUT /api/v1/artifacts/{id}/versions/{seq}/files/{path}` | Upload one file of a pending version (raw body). Its size and SHA-256 must match the manifest; `X-Content-SHA256`, when sent, must too. Returns `204`. |
| `POST /api/v1/artifacts/{id}/versions/{seq}/finalize` | Make a pending version ready once every file has arrived; it becomes the current version unless a later one already is. Returns `200` with the artifact, the version and any `warnings` (remote images that could not be fetched); `409` with code `incomplete` and `details.missing` while files are missing. |
| `POST /api/v1/artifacts/{id}/versions/{seq}/view` | For a version whose entry is HTML: a view URL for showing it in a sandboxed frame (`url`, `expiresAt`, and `remoteImages` when the entry references images on other servers). Requires read access; the URL is valid for 30 minutes. |
| `GET /api/v1/artifacts/view/{capability}/{path}` | A file of the version the view URL was issued for; see [HTML artifacts](#html-artifacts). |
| `GET /api/v1/artifacts/{id}/versions[?limit=][&before=]` | The ready versions, newest first, without their files: up to `limit` (default 100, at most 500) with a version number below `before`; `nextBefore` in the response gives the next page. |
| `GET /api/v1/artifacts/{id}/versions/{seq}` | One ready version with its files. |

Status codes: `400` for a malformed request, `401` unauthenticated, `403` when the caller may not publish in the scope (with code `missing_scope` and `details.scope` naming the scope when an agent token lacks `project:artifact:read` or `project:artifact:write`; reads by such a token answer `404`), `404` for an absent or unreadable artifact, `413` when the file exceeds `artifacts.max_file_bytes` (rejected before anything is stored), `503` when the hub has no artifact storage configured.

**Two-step publish.** The manifest is `{"title", "key", "scope", "entry", "note", "files": [{"path", "size", "sha256", "mediaType"}]}`; `title`, `key` and `scope` apply only when posting to `/api/v1/artifacts` (and `title` only when that creates the artifact), and `entry` may be omitted for a single file. Paths are relative, use `/`, may not contain a name starting with `.`, and may not start with `_remote/`, a prefix the hub reserves; no path may also be a folder of another. A file whose path and SHA-256 match the current version's needs no upload. Only the publisher of a pending version may upload to it or finalize it, and one finalize request at a time completes it (a finalize cut short, for example by a hub restart, can be retried after a few minutes); an artifact has at most 4 pending versions at a time, and a version still pending after 24 hours is discarded (an artifact left with no version is removed, freeing its key). Limits are checked against the manifest (`413`) and again at finalize, so a limit lowered meanwhile applies. Until its first version is finalized, a new artifact is visible only to its publisher.

**File delivery.** On a hub with local storage the hub streams the bytes. On a hub with object storage (GCS) it answers `302` to a short-lived signed URL; add `?stream=1` to have the hub serve the bytes itself (the web page does this for text). Either way the response carries `Content-Disposition` (`inline` only for plain text, Markdown, CSV, TSV, JSON, YAML, TOML and raster images; `attachment` otherwise) and `X-Content-Type-Options: nosniff`; streamed responses also carry a sandboxing `Content-Security-Policy` and an `ETag`.

**Permissions.** Reading checks `artifact.read` in the artifact's home project; publishing checks `artifact.create` in the target project. Adding a version to an existing artifact also checks `artifact.create`, in the artifact's home project, and requires being its owner or holding a write grant on it. Project owners, admins and members hold both for their project; changing, deleting or sharing an existing artifact is left to its owner. Agent tokens carry them through the `project:artifact:read` and `project:artifact:write` scopes: `readonly`, `baseline` and `full` agents can read, `baseline` and `full` agents can publish, and no agent can delete artifacts or manage their grants. An agent token without `project:artifact:read` gets `404` for every artifact, even one shared with it directly. A user access token is limited by its own project boundary and permissions; removing a user from a project does not by itself stop such a token from reading artifacts the user owns or was given access to, so revoke the token for an immediate cut-off. Artifact access uses dedicated agent scopes; agents created from a credential issued before artifacts existed need to be recreated from a current credential before they can use artifacts.
