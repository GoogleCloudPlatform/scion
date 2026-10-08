---
title: Artifacts
description: The scion artifact command and the /api/v1/artifacts routes for publishing files with stable references.
---

:::caution[Experimental]
Artifacts are behind the `hub.artifacts` experiment, which is **off by default**. While it is off, or when the hub's `artifacts` settings section is disabled, every `/api/v1/artifacts` route answers `404` and the web UI shows no artifact pages and no *Artifacts* item in the sidebar. An admin enables it under **Admin → Server Config → Experiments** (see [Experiments](/scion/reference/experiments/)). Artifacts require Hub mode.
:::

An **artifact** is a published file or folder (a *bundle*) with a stable reference, `scion://artifact/<id>`, that works from any runtime broker, in any project the reader can access, and in the web UI. The hub stores the bytes, so a reader never needs access to the publisher's filesystem or shared directories.

This page covers what is available today: publishing files and folders, versions, fetching, reviews, the web pages, and artifact references in messages. Share links are planned.

Each artifact has numbered **versions**. A version is an immutable snapshot of the bundle: its files, one **entry** file (the one the web page opens and `get` prints), an optional note, who published it, and its **kind**: `publish`, or `review` for a version that carries a reviewer's marks (see [Reviews](#reviews)). Publishing again under the same `--key` adds a version; the latest one is the artifact's **current** version, and `scion://artifact/<id>@<seq>` names one version for good.

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
- `--version-of <ref>`: Publish the file or folder as the next version of the artifact `<ref>`, which you own or may write, whether or not it has a key. A single file replaces the current version's entry file, whatever the local file is named, and the version's other files are kept unchanged without being uploaded; a folder is the whole new bundle, its entry defaulting to the current version's. If `<ref>` names a version (`@<seq>`) that is no longer current, the CLI stops before uploading. `--title` and `--key` do not apply.
- `--review <ref>`: Publish the file or folder as a review of the artifact `<ref>` instead of as a new artifact or version. See [Reviews](#reviews). `--title` and `--key` do not apply.

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
- `--clean`: Resolve the CriticMarkup marks in every text file by rejecting them all: the text the reviewer started from. See [Reviews](#reviews).
- `--accept`: Resolve the marks by accepting them all.
- `--kind <publish|review>`: Fetch the newest version of that kind at or before the version `<ref>` names (the current version for a bare reference). `--kind publish` skips reviews. The CLI prints the version it used on stderr.

Every file is checked against the size and SHA-256 the hub recorded at publish time (before `--clean` or `--accept` is applied) before it is written, to stdout or to disk, and moved into place atomically; a mismatch fails without writing that file. With `--out`, `get` writes only plain relative names (none starting with `.`), refuses to write through a symbolic link below the `--out` directory, and replaces an existing file only with `--force`. A path ending in `/` names a directory. On Linux and macOS these checks are part of each write, except that on a file system without hard links (such as FAT or exFAT) the existing-file check is made just before the write. On Windows all of these checks are made just before each write.

### `scion artifact versions <ref>`

Lists an artifact's versions, newest first, with kind, publish time, publisher, file count, size and note. The current version is marked `*`.

```text
$ scion artifact versions scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
  REF                                                  KIND     PUBLISHED             BY             FILES  SIZE     NOTE
* scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d@2  publish  2026-10-06 18:20 UTC  agent:<id>     1      4.1 KiB  round 2
  scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d@1  publish  2026-10-06 17:02 UTC  agent:<id>     1      3.9 KiB
```

## Reviews

A review is a version of kind `review`: a complete copy of the artifact whose text carries [CriticMarkup](https://github.com/CriticMarkup/CriticMarkup-toolkit) marks. The reviewer marks up the current version; the owner reads the review back and publishes the resolved text as the next version (`--version-of`).

| Mark | Meaning | `--clean` (reject all) | `--accept` (accept all) |
| :--- | :--- | :--- | :--- |
| `{++text++}` | Insert | removed | `text` |
| `{--text--}` | Delete | `text` | removed |
| `{~~old~>new~~}` | Substitute | `old` | `new` |
| `{>>text<<}` | Comment | removed | removed |
| `{==text==}` | Highlight | `text` | `text` |

Marks do not nest: a mark ends at the first closing token of its kind, and other marks inside it are plain text. A mark with no closing token, or a substitution with no `~>`, is plain text. Marks may span lines. Marks inside Markdown code spans and fences are marks too.

A document whose text already contains complete CriticMarkup marks (for example, a page that documents CriticMarkup itself) cannot be reviewed with marks: rejecting every mark also removes that text, so the check below refuses every review that adds marks. A lone opening token such as `{++` stays plain text, and a review identical to the document passes.

**Publishing a review.** `scion artifact publish <file|folder> --review <ref>` publishes a review of the artifact `<ref>`; any reader with write access to the artifact (its owner, or a principal granted write) may review it. A single file reviews the current version's entry file, whatever the local file is named, and the version's other files are kept unchanged without being uploaded. A folder is the whole reviewed bundle. If `<ref>` names a version (`@<seq>`) that is no longer current, the CLI stops before uploading.

```text
$ scion artifact get scion://artifact/5f1c2d3e-... > plan.md
$ # add marks to plan.md
$ scion artifact publish plan.md --review scion://artifact/5f1c2d3e-...
scion://artifact/5f1c2d3e-...  (v3, review)
```

**A review changes only marks.** A review is checked against the version it was made against, its *base*: the client names it when it finalizes the review (`{"base": <seq>}`; the CLI sends the version it started from), and it must still be the artifact's current version. For every text file, the review with every mark rejected must equal the base's text, compared after Unicode NFC normalisation with CRLF and CR line endings read as LF. The base's text is its content for a `publish` version, and its content with every mark rejected for a `review` version, so a review of a review is checked against the same published text. A file identical to the base's passes. Other files must be identical to the base's, every file of the base must be present, no file may be added, and the entry file may not change. Otherwise finalize answers `422` with code `unmarked_changes` and discards the review; the current version does not change. `details.files` lists up to 20 files, each with its `path`, a `change` (`modified`, `added`, `removed` or `entry`) and, for a modified text file, up to 20 `hunks`. Each hunk gives the base's line number (`line`), the number of lines on each side (`parent_lines`, `clean_lines`) and the start of each side (`parent`, `clean`), at most 256 bytes each; `truncated` marks a cut. `details.truncated` is `true` when more files differ. The CLI prints the hunks.

**Reviews and new versions.** A finalized review becomes the artifact's current version, so `get` returns the marked-up text. Finalize answers `409` with code `stale_review` and discards the review when its base is no longer the current version, or when a version of kind `publish` newer than the base is still being published (pending), so a review never becomes current over a version its reviewer did not see; the message then says that a newer version is being published, to be reviewed once it is finalized. A pending review does not block other reviews. Review the current version again. A finalize of a review without a base answers `400` with code `base_required`.

**Notice to the owner.** When an agent owns the artifact, the hub sends it a system message (category `artifact-review`) when someone else's review is finalized:

```text
A review (v3, by a user) was published on your artifact; it is now the current version.
Artifact: v3 - scion artifact get scion://artifact/5f1c2d3e-...@3
Without the marks: scion artifact get scion://artifact/5f1c2d3e-...@3 --clean
Apply or answer the marks, then publish the result as the next version: scion artifact publish <file> --version-of scion://artifact/5f1c2d3e-...
```

The message comes from the hub and carries the reference in its text only; whether the owner may read the version is decided when it runs `get`. The *Review pending* badge in the artifacts list stays until a version of kind `publish` is current again.

**Reading a review.** `scion artifact get <ref> --clean` returns the base text, `--accept` the text with every suggestion applied, and `--kind publish` the newest published version. Over the API, add `?resolve=clean` or `?resolve=accept` to a file route.

## Images in Markdown artifacts

Images in a Markdown artifact are fetched at publish time and served from the hub, so opening an artifact never makes the hub contact another server.

- **Remote images.** When a version whose entry file is Markdown is published, the hub fetches each absolute `https` image the entry references inline (`![alt](https://...)` and `<img src="https://...">`) once, and stores it in the version as a file at `_remote/<sha256 of the URL>`. Images are taken from the first 2 MiB of the entry; if the entry is larger, the response and the CLI add one warning saying that later images were not fetched. Fetched images count toward the version's file and size limits (`max_files`, `max_bundle_bytes`); images past either limit are not fetched, with a warning. If the entry holds more images than the hub reads, a warning says that later images were not fetched. Reference-style images (`![alt][label]` with the URL in a definition elsewhere) are not fetched yet (ptone/scion#3678); they show as "not fetched" in the preview. Image URLs written in code are fetched as well, though they are not shown. Only PNG, JPEG, GIF and WebP images are kept; SVG and other types are not. An image that cannot be fetched (for example a non-`https` URL, an address the hub does not fetch from, a timeout, a missing image or another type) is recorded as failed, the publish still succeeds, and the response and the CLI list one warning per such image. Publishing a new version fetches again.
- **Which URLs.** An `<img src>` is read as the browser reads it. A URL is fetched only in a plain form: `http` or `https`, a host name (no IP literal in brackets), an optional port, no user name or password, and no spaces or parentheses. Other images are not fetched and show as "not fetched" in the preview.
- **Reading a remote image.** `GET /api/v1/artifacts/{id}/versions/{seq}/files/_remote/<hash>` follows the same access checks as any other file. A fetched image is served with its detected type; a failed one answers `404` with the header `X-Artifact-Remote-Status: failed`. The version's file manifest lists each remote image with `origin: "remote"`, its `sourceUrl` and its `fetchStatus` (`ok` or `failed`).
- **Reserved names.** Files may not be published under `_remote/`.

Settings (in the `artifacts` section): `remote_images_enabled` (default `true`), `remote_image_max_count` (images fetched per version, default `32`; further images are not fetched and get one warning), `remote_image_max_bytes` (per image, default 5 MiB, at most `max_file_bytes`), `remote_image_fetch_timeout_s` (per image, default `10`) and `remote_image_total_budget_s` (all images of one version, default `30`, between the fetch timeout and 60 seconds; images are fetched while the publish or finalize request is open, and the hub keeps that request open for the budget plus a margin). `remote_image_max_count` may not exceed `max_files`; when the two remote image caps are not set, they follow a lower `max_files` or `max_file_bytes`. A settings write with an invalid value, including a remote image cap above the matching file limit or a budget below the fetch timeout, is refused. A stored document with an invalid remote image value turns remote images off and is logged; any other invalid stored value disables the artifact service.

## HTML artifacts

An artifact whose entry file is HTML (a single page or a small site published as a folder) is shown in a sandboxed frame. The page's scripts run, but in an isolated origin with no access to the hub page around it, to your session or to the hub's API.

- The frame loads the version through a **view URL**, `/api/v1/artifacts/view/<capability>/<entry>`, issued to a reader of the artifact and valid for 30 minutes. Relative links in the bundle (`img/chart.png`, `css/site.css`) resolve under it, so the page loads its own files. The view URL names one version of one artifact and gives access to nothing else; it is not a share link.
- Every view response carries a `Content-Security-Policy` with a `sandbox` directive, so the page stays isolated even when the URL is opened directly. The page may load scripts, styles, images, fonts and media only from its own files under the view URL; it cannot make network requests from script, embed other frames or plugins, submit forms, open windows or navigate the page around it.
- **Remote images are not loaded in HTML artifacts; include them in the bundle.** When an HTML entry references images by absolute `http(s)` URL, the publish response and the CLI print that warning, and the viewer shows it above the frame.

## Artifacts in messages

A message can name artifacts. The message carries only the reference, never the bytes and never a copy of the title; each reader sees what their own access allows.

```text
$ scion message @reviewer "Design ready for review." --artifact scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
```

- **Sending.** `--artifact` (repeatable, at most 10 per message) adds the reference to the message text and to the message's `artifacts` metadata entry. The Hub keeps only references the sender can read at send time; it drops the others and returns a warning that does not say why a reference was dropped. If artifacts are not enabled on the hub, every reference is dropped and the warning says so.
- **What an agent receives.** The delivered message keeps the `artifacts` metadata entry and ends with one fetch hint per artifact, built from the reference alone:
  ```text
  Artifact: v2 - scion artifact get scion://artifact/5f1c2d3e-...@2
  Artifact: current - scion artifact get scion://artifact/5f1c2d3e-...
  ```
  The hint carries no title. Whether the recipient may read the artifact is decided when it runs `scion artifact get`, from any broker, without shared directories.
- **What web chat shows.** A message with artifact references shows a chip for each one, in the order the references appear in the message text (references attached without appearing in the text follow): the title, version and owner of an artifact you can read, or **Artifact unavailable** for any other reference, the same as for an artifact that does not exist. `scion://artifact/` references in message text are links. Clicking a chip or a link opens the artifact in a preview over the conversation, with **Copy link** and **Open in artifact viewer**. Text and images show in the preview; Markdown shows as its source there, and renders in the artifact viewer. Chat history responses include `messageArtifacts`, keyed by message ID, resolved for the viewing user.
- **Attaching in web chat.** The composer's paperclip offers **Upload file** and **Attach artifact…**. The picker lists the artifacts you can read, with search and **Owned by me** / **This project** filters, and attaches the current version of up to 10 per message. Picked artifacts show as chips above the input; they travel as references, never as copies.
- **Where references are kept.** Direct messages to an agent, messages from an agent to a user or conversation, and web chat messages to agents carry references. Group (`group[...]`) and broadcast messages, @-mention copies, and messages arriving through chat plugins do not; any `artifacts` metadata they arrive with is removed.

## Web pages

**Artifacts list.** The *Artifacts* item in the sidebar, under *Management* beside *Skills*, opens `/artifacts`. It lists the same artifacts as [the list endpoint](#listing): the ones you own, the ones shared with you, and the ones published in your projects, newest first. Each row shows the title (with the key, when the artifact has one), the owner, the home project, when it was last updated and a *Review pending* badge when the current version is a review. Search filters by title or key; *Review pending* and *Owned by me* narrow the list further. *Load more* fetches the next page. A row opens the artifact's page. Owner and project names show as ids when you may not look them up.

**Artifact page.** `/projects/<project-id>/artifacts/<id>` shows the current version and `/projects/<project-id>/artifacts/<id>/v/<seq>` an earlier one. The header shows the title, owner, key, last update and the version's reference with a copy button, a **Version** menu listing the versions, **Edit**, **Review** (Markdown entries) and, for a single file, **Download**. Three tabs:

- **Preview** renders the entry file: Markdown as formatted text, text and code (including JSON, YAML, CSV) in a read-only editor, PNG, JPEG, GIF and WebP images inline, and HTML in the sandboxed frame described under [HTML artifacts](#html-artifacts), with a border and a bar saying who published it, plus **Full screen** and **Open in new tab**. The view URL lasts 30 minutes; after that the page says the view has expired and offers **Reload view**. Other types (such as SVG and PDF) are offered as a download.
- **Files** lists the version's files with size and type, marks the entry file, and opens or downloads each one. Copies of remote images are counted, not listed.
- **History** lists the versions, newest first, with who published each one, when, and its note.

**Markdown preview.** The preview is a frame that runs no scripts and loads images from the hub only. A relative image path loads that file from the same version, and an absolute image URL loads the copy the hub fetched when the version was published. An image without a copy shows "Image not fetched", one whose fetch failed shows "Image could not be fetched", and a relative path that is not in the version shows "Image not in this version". Inline (`data:`) images are shown. Links open in a new tab; a relative link to a file of the version opens that file.

**Edit and new versions.** On the current version, when the entry file is Markdown or text, **Edit** opens it in an editor, and **Publish new version** saves the change as a new version with an optional note; the other files are kept as they are. If a publish fails part-way, publishing again continues it rather than starting over; an unfinished version that is not continued is removed after 24 hours. **Upload new version** (on the History tab) publishes a file or a folder as the next version. Earlier versions are never changed.

**Review.** On the current version, when the entry file is Markdown, **Review** opens it in an editor beside a live preview. Select text and choose **Comment**, **Suggest**, **Insert** or **Delete** to add a mark (see [Reviews](#reviews)); the toolbar counts the suggestions and comments. **Save review** publishes the marked-up copy as a version of kind `review`, with an optional note. It stays disabled until there is at least one mark, and while text outside the marks differs from the version you started from, the page says so (use **Edit** for a plain edit). If the hub refuses the review, the page names the lines that changed outside marks. Edit and Review are hidden while you review. While the current version is a review, the header shows a *Review pending* badge and a banner names the reviewer and counts the marks; both clear when a new version of kind `publish` is current. A review's preview has a **Marks · Clean · Accepted** switch: Marks shows insertions, deletions, substitutions and highlights inline and comments as numbered notes (in the right margin when the preview is wide enough, under the reference otherwise); Clean shows the text with every mark rejected, Accepted with every mark accepted, as `?resolve=` does.

**Project page.** With artifacts on, the Files area of a project page has three views: **Artifacts** (shown first), **Shared dirs** and **Workspace**. Artifacts lists the artifacts homed in the project that you can open, newest first, with search, owner, version, last update and a *Review pending* badge; a row opens the artifact page. **New artifact** uploads a file, or a folder with a chosen entry file (hidden files and folders are skipped), with a title, an optional key and an optional note. Shared dirs and Workspace show the project's files as before.

## API

All routes are under `/api/v1/artifacts` and use the hub's usual authentication (session, user access token or agent token). Errors use the hub's JSON error envelope.

| Method and path | Purpose |
| :--- | :--- |
| `POST /api/v1/artifacts?name=<file>[&title=<title>][&scope=<project-id>]` | Publish the raw request body as a new single-file artifact. `scope` defaults to the caller's project (agents); users must set it. Optional header `X-Content-SHA256` (hex) is verified. Returns `201` with the artifact, its first version and any publish `warnings`. |
| `GET /api/v1/artifacts?mine=1[&q=<text>][&review_pending=1][&owner=me][&scope=<project-id>][&limit=<n>][&cursor=<c>]` | The artifacts the caller can read among those it owns, those shared with it directly, and those homed in projects it is a member of, newest first. See [Listing](#listing). |
| `GET /api/v1/artifacts/{id}` | The artifact and its current version, including the file manifest (`path`, `size`, `sha256`, `mediaType`, and for remote images `origin`, `sourceUrl` and `fetchStatus`; see [Images in Markdown artifacts](#images-in-markdown-artifacts)). |
| `GET /api/v1/artifacts/{id}/files/{path}[?resolve=clean\|accept]` | A file of the current version. With `resolve`, see [Resolving marks](#resolving-marks). |
| `GET /api/v1/artifacts/{id}/versions/{seq}/files/{path}[?resolve=clean\|accept]` | A file of version `seq`. |
| `POST /api/v1/artifacts` (JSON manifest) | Start a two-step publish: a new artifact with a pending first version, or, when `key` names an artifact the caller already published in the scope, a new pending version of it. Returns `201` with the artifact, the pending version and `upload.required`, the paths to upload. |
| `POST /api/v1/artifacts/{id}/versions` (JSON manifest) | Start a new pending version of an artifact. With `"kind": "review"`, a review of the current version (`409` with code `nothing_to_review` when the artifact has no published version). |
| `PUT /api/v1/artifacts/{id}/versions/{seq}/files/{path}` | Upload one file of a pending version (raw body). Its size and SHA-256 must match the manifest; `X-Content-SHA256`, when sent, must too. Returns `204`. |
| `POST /api/v1/artifacts/{id}/versions/{seq}/finalize` | Make a pending version ready once every file has arrived; it becomes the current version unless a later one already is. Returns `200` with the artifact, the version and any `warnings` (remote images that could not be fetched); `409` with code `incomplete` and `details.missing` while files are missing. For a review, the optional JSON body `{"base": <seq>}` is required: `400` with code `base_required` without it, `422` with code `unmarked_changes` or `409` with code `stale_review`, both discarding the review (see [Reviews](#reviews)). |
| `POST /api/v1/artifacts/{id}/versions/{seq}/view` | For a version whose entry is HTML: a view URL for showing it in a sandboxed frame (`url`, `expiresAt`, and `remoteImages` when the entry references images on other servers). Requires read access; the URL is valid for 30 minutes. |
| `GET /api/v1/artifacts/view/{capability}/{path}` | A file of the version the view URL was issued for; see [HTML artifacts](#html-artifacts). |
| `GET /api/v1/artifacts/{id}/versions[?limit=][&before=]` | The ready versions, newest first, without their files: up to `limit` (default 100, at most 500) with a version number below `before`; `nextBefore` in the response gives the next page. |
| `GET /api/v1/artifacts/{id}/versions/{seq}` | One ready version with its files. |

Status codes: `400` for a malformed request (including a list request without `mine=1` or with an invalid cursor), `401` unauthenticated, `403` when the caller may not publish in the scope (with code `missing_scope` and `details.scope` naming the scope when an agent token lacks `project:artifact:read` or `project:artifact:write`; reads by such a token answer `404`), `404` for an absent or unreadable artifact, `413` when the file exceeds `artifacts.max_file_bytes` (rejected before anything is stored), `503` when the hub has no artifact storage configured.

**Two-step publish.** The manifest is `{"title", "key", "scope", "kind", "entry", "note", "files": [{"path", "size", "sha256", "mediaType"}]}`; `kind` is `publish` (the default) or `review`, and a review is posted to `/api/v1/artifacts/{id}/versions`; `title`, `key` and `scope` apply only when posting to `/api/v1/artifacts` (and `title` only when that creates the artifact), and `entry` may be omitted for a single file. Paths are relative, use `/`, may not contain a name starting with `.`, and may not start with `_remote/`, a prefix the hub reserves; no path may also be a folder of another. A file whose path and SHA-256 match the current version's needs no upload. Only the publisher of a pending version may upload to it or finalize it, and one finalize request at a time completes it (a finalize cut short, for example by a hub restart, can be retried after a few minutes); an artifact has at most 4 pending versions at a time, and a version still pending after 24 hours is discarded (an artifact left with no version is removed, freeing its key). Limits are checked against the manifest (`413`) and again at finalize, so a limit lowered meanwhile applies. Until its first version is finalized, a new artifact is visible only to its publisher.

### Resolving marks

`?resolve=clean` or `?resolve=accept` on a file route serves a text file (`text/*`, JSON, YAML, TOML or XML) with its CriticMarkup marks resolved as in [Reviews](#reviews). The hub produces the bytes, so the response is always streamed by the hub, never redirected to object storage. It carries `X-Artifact-Resolve` naming the projection, an `ETag` distinct from the stored file's, and the usual file headers. Other files ignore the parameter and are served unchanged, without `X-Artifact-Resolve`. `resolve=raw` or no parameter serves the stored bytes; any other value answers `400` whatever the artifact. Access checks are those of any file read: an artifact the caller cannot read answers the same `404` as one that does not exist.

### Listing

`GET /api/v1/artifacts?mine=1` lists, newest update first:

- artifacts the caller owns;
- artifacts shared with the caller directly;
- artifacts homed in a project the caller is a member of (for an agent, its own project), or shared with such a project.

Every listed artifact passes the same check as `GET /api/v1/artifacts/{id}` for the same caller, so the list never shows an artifact the caller could not open. A caller that may read nothing gets `200` with an empty list. That includes a user access token without `artifact:read` and an agent token without `project:artifact:read`. Membership is read on every request, so after a user leaves a project, that project's artifacts drop out of the list unless the user owns them or they were shared with the user directly. A hub-wide role does not add artifacts: an admin sees the same kinds of rows as anyone else.

`mine=1` is required: without it the request is rejected with `400`, because there is no hub-wide listing. Optional parameters:

| Parameter | Effect |
| :--- | :--- |
| `q` | Keep artifacts whose title or key contains this text (at most 200 characters). Case is ignored for ASCII letters. For other letters, whether case is ignored depends on the database, so search for them in their exact case. |
| `review_pending=1` | Keep artifacts whose current version is a review awaiting the owner. |
| `owner=me` | Keep only artifacts the caller owns. |
| `scope` | Keep only artifacts homed in this project (a project ID). It narrows the list and never adds an artifact the list would not show without it. The project's Artifacts view in the web UI uses it. |
| `limit` | Page size, 1-100 (default 50). |
| `cursor` | The `nextCursor` of the previous page. |

The response is `{"artifacts": [...], "nextCursor": "..."}`. Each entry has the same fields as `artifact` in the single-artifact response, plus `reviewPending`. `nextCursor` is absent on the last page. Cursors are opaque and only work for the same caller and the same `q`, `review_pending`, `owner` and `scope` values. A page may hold fewer than `limit` entries and still have a `nextCursor`, because one request examines a bounded number of rows. Keep following the cursor until it is absent. Artifacts created or updated while you page move to the front of the order: they appear on a fresh listing, not later in the current walk.

**File delivery.** On a hub with local storage the hub streams the bytes. On a hub with object storage (GCS) it answers `302` to a short-lived signed URL; add `?stream=1` to have the hub serve the bytes itself (the web page does this for text). Either way the response carries `Content-Disposition` (`inline` only for plain text, Markdown, CSV, TSV, JSON, YAML, TOML and raster images; `attachment` otherwise) and `X-Content-Type-Options: nosniff`; streamed responses also carry a sandboxing `Content-Security-Policy` and an `ETag`.

**Permissions.** Reading checks `artifact.read` in the artifact's home project; publishing checks `artifact.create` in the target project. Adding a version to an existing artifact also checks `artifact.create`, in the artifact's home project, and requires being its owner or holding a write grant on it. Project owners, admins and members hold both for their project; changing, deleting or sharing an existing artifact is left to its owner. Agent tokens carry them through the `project:artifact:read` and `project:artifact:write` scopes: `readonly`, `baseline` and `full` agents can read, `baseline` and `full` agents can publish, and no agent can delete artifacts or manage their grants. An agent token without `project:artifact:read` gets `404` for every artifact, even one shared with it directly. A user access token is limited by its own project boundary and permissions; removing a user from a project does not by itself stop such a token from reading artifacts the user owns or was given access to, so revoke the token for an immediate cut-off. Artifact access uses dedicated agent scopes; agents created from a credential issued before artifacts existed need to be recreated from a current credential before they can use artifacts.
