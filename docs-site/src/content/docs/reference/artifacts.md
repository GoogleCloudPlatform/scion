---
title: Artifacts
description: The scion artifact command and the /api/v1/artifacts routes for publishing files with stable references.
---

:::caution[Experimental]
Artifacts are behind the `hub.artifacts` experiment, which is **off by default**. While it is off, or when the hub's `artifacts` settings section is disabled, every `/api/v1/artifacts` route answers `404` and the web UI shows no artifact pages and no *Artifacts* item in the sidebar. An admin enables it under **Admin → Server Config → Experiments** (see [Experiments](/scion/reference/experiments/)). Artifacts require Hub mode.
:::

An **artifact** is a published file or folder (a *bundle*) with a stable reference, `scion://artifact/<id>`, that works from any runtime broker, in any project the reader can access, and in the web UI. The hub stores the bytes, so a reader never needs access to the publisher's filesystem or shared directories.

This page covers what is available today: publishing files and folders, versions, fetching, reviews, the web pages, artifact references in messages, share links, grants and expiry.

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

A document whose text already contains complete CriticMarkup marks (for example, a page that documents CriticMarkup itself) is hard to review with marks: rejecting every mark also changes that text, so the check below refuses a review that adds marks while the document's own marks are left as they are. A review passes when it wraps each of the document's marks in a deletion (`{--{++ins++}--}` keeps `{++ins++}` when every mark is rejected), and a review identical to the document passes. A lone opening token such as `{++` stays plain text.

**Publishing a review.** `scion artifact publish <file|folder> --review <ref>` publishes a review of the artifact `<ref>`; any reader with write access to the artifact (its owner, or a principal granted write) may review it. A single file reviews the current version's entry file, whatever the local file is named, and the version's other files are kept unchanged without being uploaded. A folder is the whole reviewed bundle. If `<ref>` names a version (`@<seq>`) that is no longer current, the CLI stops before uploading.

```text
$ scion artifact get scion://artifact/5f1c2d3e-... > plan.md
$ # add marks to plan.md
$ scion artifact publish plan.md --review scion://artifact/5f1c2d3e-...
scion://artifact/5f1c2d3e-...  (v3, review)
```

**A review changes only marks.** A review is checked against the version it was made against, its *base*: the client names it when it finalizes the review (`{"base": <seq>}`; the CLI and the web page send the version they started from), and it must still be the artifact's current version. For every text file, the review with every mark rejected must equal the base's text, compared after Unicode NFC normalisation with CRLF and CR line endings read as LF. Normalisation changes the line-ending style only, not whether the file ends with a newline: a review that adds or removes only a final newline does not equal the base and is refused with code `unmarked_changes`. The base's text is its content for a `publish` version, and its content with every mark rejected for a `review` version, so a review of a review is checked against the same published text. A file identical to the base's passes. Other files must be identical to the base's, every file of the base must be present, no file may be added, and the entry file may not change. Otherwise finalize answers `422` with code `unmarked_changes` and discards the review; the current version does not change. `details.files` lists up to 20 files, each with its `path`, a `change` (`modified`, `added`, `removed` or `entry`) and, for a modified text file, up to 20 `hunks`. Each hunk gives the base's line number (`line`), the number of lines on each side (`parent_lines`, `clean_lines`) and the start of each side (`parent`, `clean`), at most 256 bytes each; `truncated` marks a cut. `details.truncated` is `true` when more files differ. The CLI prints the hunks.

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

Settings (in the `artifacts` section): `remote_images_enabled` (default `true`), `remote_image_max_count` (images fetched per version, default `32`; further images are not fetched and get one warning), `remote_image_max_bytes` (per image, default 5 MiB, at most `max_file_bytes`), `remote_image_fetch_timeout_s` (per image, default `10`) and `remote_image_total_budget_s` (all images of one version, default `30`, between the fetch timeout and 60 seconds; images are fetched while the publish or finalize request is open, and the hub keeps that request open for the budget plus a margin). `remote_image_max_count` may not exceed `max_files`; when the two remote image caps are not set, they follow a lower `max_files` or `max_file_bytes`. A settings write with an invalid value, including a remote image cap above the matching file limit or a budget below the fetch timeout, is refused. A stored document with an invalid remote image value turns remote images off and is logged; any other invalid stored value disables the artifact service. A deployment that embeds the artifact service with its own limits and leaves the remote image limits unset gets remote image fetching off.

## HTML artifacts

An artifact whose entry file is HTML (a single page or a small site published as a folder) is shown in a sandboxed frame. The page's scripts run, but in an isolated origin with no access to the hub page around it, to your session or to the hub's API.

- The frame loads the version through a **view URL**, `/api/v1/artifacts/view/<capability>/<entry>`, issued to a reader of the artifact and valid for 30 minutes. Relative links in the bundle (`img/chart.png`, `css/site.css`) resolve under it, so the page loads its own files. The view URL names one version of one artifact and gives access to nothing else; it is not a share link.
- Every view response carries a `Content-Security-Policy` with a `sandbox` directive, so the page stays isolated even when the URL is opened directly. The page may load scripts, styles, images, fonts and media only from its own files under the view URL; it cannot make network requests from script, embed other frames or plugins, submit forms, open windows or navigate the page around it.
- **Remote images are not loaded in HTML artifacts; include them in the bundle.** When an HTML entry references images by absolute `http(s)` URL, the publish response and the CLI print that warning, and the viewer shows it above the frame.

## Share links

A share link lets anyone who holds it open an artifact's current version in a browser, without signing in. Links always expire.

- **Who creates them.** The artifact's owner, or a user holding an admin grant on it, while their credential allows `artifact.manage` in the artifact's home project. Agents cannot create share links. A caller that cannot read the artifact gets `404`, the same as for a missing one; a reader that may not manage links gets `403`.
- **Lifetime.** A link lasts `link_default_ttl_hours` (default 168, seven days) unless the request asks for less or more, up to `link_max_ttl_hours` (default 720, thirty days). A request for exactly the maximum succeeds; a longer one is rejected with `400` and code `ttl_too_long`, and no link is created (the lifetime is not shortened to the maximum). Separately, when the artifact itself expires sooner than the link would, the link is cut to end with the artifact and the response says so (`clampedToArtifactExpiry`). An artifact has at most 50 unexpired links at a time.
- **The token.** The link is `/api/v1/artifacts/shared/<token>`, where the token is 256 random bits. It is shown once, in the response that creates the link. The hub stores only its SHA-256 and never logs it; listing the links shows their ids and expiry, never the token. Keep the link private: anyone holding it can open the artifact until it expires or is revoked.
- **Opening a link.** The link answers `303` to a [view URL](#html-artifacts) for the current version's entry, and `/api/v1/artifacts/shared/<token>/files/<path>` to one for another file of it. Everything a link reaches is served through the view route, with its sandbox and `Content-Security-Policy`, and every response carries `Referrer-Policy: no-referrer`. The view URL lasts at most 30 minutes and no longer than the link, and stops working as soon as the link is revoked or expires. The view route serves files as they are: an HTML entry renders in its sandbox, while a Markdown entry opens as plain text.
- **When a link stops working.** A revoked or expired link, a link whose artifact was deleted or has expired, and a token that never existed all get the same `404`. Share-link reads are rate limited per client address (`429` with `Retry-After`). Revoking a link that has already expired still answers `204` until the next link created on the artifact clears expired links; after that it answers `404`.
- **Links and access.** A link never makes the artifact readable through the other routes and never adds it to anyone's list.

## Grants

A grant gives someone else access to one artifact. The subject is a person or an agent (`user:<id>`, `agent:<id>`) or a project (its members and agents), with one of three permissions: **read** (open the artifact and its versions), **write** (also publish new versions) and **admin** (also manage its grants and share links, change its expiry and move it). The owner holds admin implicitly.

- **The home project.** Every artifact has a read grant for its home project, listed first (`home: true`). The owner or an admin may raise it to write; it cannot be removed (move the artifact instead).
- **Other projects.** A grant to another project can be created, or its permission changed, only while the hub's `messaging.cross_project_messaging_enabled` setting is on. The setting is checked only when a grant is created or changed (and when an artifact is moved), not when it is read. Turning the setting off is not retroactive: grants to other projects that already exist stay in place and keep giving access. To end that access, remove those grants one by one; removing a grant works whether the setting is on or off.
- **Who manages grants.** The same users who manage share links: the owner or a user with an admin grant, with a credential that allows `artifact.manage` in the home project. Agents cannot manage grants.
- **Limits of a grant.** A grant never reaches past the caller's own credential: an agent token without `project:artifact:read`, or whose delegation is gone, gets `404` even on an artifact granted to it, and a user access token stays within its project boundary. An artifact has at most 100 grants. Grants do not expire on their own; they end when they are removed or when the artifact is deleted.

## Expiry, moving and deletion

- **Retention.** `default_retention_days` (default `0`, keep until deleted) gives each new artifact an expiry that many days after it is created. The owner or an admin can set, change or clear an artifact's expiry with `PATCH /api/v1/artifacts/{id}`; the response counts the share links that will end early (`linksCutShort`) and the grants that will be removed (`grantsRemoved`) when it expires. These counts describe what will happen when the artifact expires. The PATCH itself does not remove any grant or share-link row, so listing the grants immediately after the PATCH still shows them. From the moment the artifact expires, every read is refused when it is requested, on every route and through every share link; a share link therefore ends at its own expiry or the artifact's, whichever comes first. The rows are removed later by the hub's maintenance pass, which runs every 10 minutes and handles up to 500 expired artifacts each time: it deletes the artifact together with all its grants and share links. Between expiry and that pass, the rows still exist, but they give no access.
- **Moving.** `PATCH /api/v1/artifacts/{id}` with `scopeRef` moves an artifact to another home project, for example after its project was deleted (deleting a project never deletes its artifacts). Any move, that one included, needs the hub's `messaging.cross_project_messaging_enabled` setting on and the caller allowed to publish into the new project through a role there (owning the artifact is not enough). The new project gets a read grant (an existing grant of it is kept); the old home project loses its grant, so to keep its access, add a grant for it explicitly. A move that would take the artifact over the grant limit is refused with `409` and code `too_many_grants`. A move to a project that holds an admin grant on the artifact is refused with `409` and code `home_admin_grant`, because a home project's grant is read or write only; lower that grant first. Naming the current home project in `scopeRef` is not a move.
- **Storage.** File contents are stored once per hub and shared by every version and artifact with the same bytes. Contents no remaining artifact uses are deleted after `gc_grace_hours` (default 168, at least 24); until then a deleted artifact's contents still exist on the hub.

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

**Artifacts list.** The *Artifacts* item in the sidebar, under *Management* beside *Skills*, opens `/artifacts`. It lists the same artifacts as [the list endpoint](#listing): the ones you own, the ones shared with you, and the ones published in your projects, newest first. Each row shows the title (with the key, when the artifact has one), why you see it (*Owned*, *Project* or *Shared with you*), the owner, the home project, when it was last updated and a *Review pending* badge when the current version is a review. A row whose home project was deleted shows *Deleted project*; while the hub's cross-project sharing setting is on, its owner and users it grants admin also see **Move…**, which moves it to one of their projects (see [Expiry, moving and deletion](#expiry-moving-and-deletion)). Search filters by title or key; *Review pending*, *Owned by me* and *Shared with me* narrow the list further. *Load more* fetches the next page. A row opens the artifact's page. Owner and project names show as ids when you may not look them up.

**Artifact page.** `/projects/<project-id>/artifacts/<id>` shows the current version and `/projects/<project-id>/artifacts/<id>/v/<seq>` an earlier one. The header shows the title, owner, key, last update and the version's reference with a copy button, a **Version** menu listing the versions, **Edit**, **Review** (Markdown entries), **Share** (for the owner and users it grants admin) and, for a single file, **Download**. Three tabs:

- **Preview** renders the entry file: Markdown as formatted text, text and code (including JSON, YAML, CSV) in a read-only editor, PNG, JPEG, GIF and WebP images inline, and HTML in the sandboxed frame described under [HTML artifacts](#html-artifacts), with a border and a bar saying who published it, plus **Full screen** and **Open in new tab**. The view URL lasts 30 minutes; after that the page says the view has expired and offers **Reload view**. Other types (such as SVG and PDF) are offered as a download.
- **Files** lists the version's files with size and type, marks the entry file, and opens or downloads each one. Copies of remote images are counted, not listed.
- **History** lists the versions, newest first, with who published each one, when, and its note.

**Markdown preview.** The preview is a frame that runs no scripts and loads images from the hub only. A relative image path loads that file from the same version, and an absolute image URL loads the copy the hub fetched when the version was published. An image without a copy shows "Image not fetched", one whose fetch failed shows "Image could not be fetched", and a relative path that is not in the version shows "Image not in this version". Inline (`data:`) images are shown. Links open in a new tab; a relative link to a file of the version opens that file.

**Edit and new versions.** On the current version, when the entry file is Markdown or text, **Edit** opens it in an editor, and **Publish new version** saves the change as a new version with an optional note; the other files are kept as they are. If a publish fails part-way, publishing again continues it rather than starting over; an unfinished version that is not continued is removed after 24 hours. **Upload new version** (on the History tab) publishes a file or a folder as the next version. Earlier versions are never changed.

**Review.** On the current version, when the entry file is Markdown, **Review** opens it in an editor beside a live preview. Select text and choose **Comment**, **Suggest**, **Insert** or **Delete** to add a mark (see [Reviews](#reviews)); the toolbar counts the suggestions and comments. A selection inside or across an existing mark, one that contains CriticMarkup, or one whose mark an unclosed opener earlier in the text would swallow, is refused with a short hint, since marks do not nest. **Save review** publishes the marked-up copy as a version of kind `review`, with an optional note. It stays disabled until there is at least one mark, and while text outside the marks differs from the version you started from, the page says so (use **Edit** for a plain edit). If the hub refuses the review, the page names the lines that changed outside marks. If a newer version was published (or is being published) while you reviewed, the review is not saved; the review restarts from the current version (with any marks others added), and your discarded text is shown read-only with **Copy** so you can redo your marks. If the current version cannot be reviewed on the page, or cannot be loaded, Review mode closes with a notice that says which, and your text stays readable with **Copy**. Edit and Review are hidden while you review. While the current version is a review, the header shows a *Review pending* badge (also when an older version is shown) and a banner names the reviewer and counts the marks; both clear when a new version of kind `publish` is current. A review's preview has a **Marks · Clean · Accepted** switch: Marks shows insertions, deletions, substitutions and highlights inline and comments as numbered notes headed by the reviewer (in a right-hand margin when the window is at least 1100 pixels wide, or 1400 pixels in Review mode where the preview takes half the page; under the reference otherwise); Clean shows the text with every mark rejected, Accepted with every mark accepted, as `?resolve=` does.

**Share dialog.** **Share** on the artifact page opens a dialog with three parts:

- **Share links.** Choose a lifetime (1, 7 or 30 days) and **Create link**. The new link is shown once, with **Copy**; it cannot be shown again, so copy it then. When the artifact expires before the chosen lifetime ends, the dialog says the link ends with the artifact. The table lists the active links with who created them, when, and when they expire, each with **Revoke**. A link opens the current version in the sandboxed viewer described under [HTML artifacts](#html-artifacts).
- **People and projects.** Search for a user, or for a project when cross-project sharing is on, pick a permission (*Can view*, *Can edit*, *Can manage*: read, write, admin) and **Add**. Each grant's permission can be changed or the grant removed; the home project is listed first and cannot be removed. When cross-project sharing is off, the dialog says so and offers users only.
- **Retention.** Set when the artifact expires (never, or 1, 7, 30 or 90 days from now). Before you save a change, the dialog warns how many share links it cuts short and how many grants are removed with the artifact when it expires.

**Project page.** With artifacts on, the Files area of a project page has three views: **Artifacts** (shown first), **Shared dirs** and **Workspace**. Artifacts lists the artifacts homed in the project that you can open, and the artifacts other projects shared with it (badged *Shared with this project*, with the project they come from), newest first, with search, a *Shared with this project* filter, owner, version, last update and a *Review pending* badge; a row opens the artifact page. **New artifact** uploads a file, or a folder with a chosen entry file (hidden files and folders are skipped), with a title, an optional key and an optional note. Shared dirs and Workspace show the project's files as before.

## API

All routes are under `/api/v1/artifacts` and use the hub's usual authentication (session, user access token or agent token). Errors use the hub's JSON error envelope. The hub's request logs record every artifact request path as `/api/v1/artifacts/REDACTED` (paths that contain the word `artifacts` anywhere are treated the same way), and artifact requests are not traced, because some artifact URLs carry share-link tokens or view capabilities.

| Method and path | Purpose |
| :--- | :--- |
| `POST /api/v1/artifacts?name=<file>[&title=<title>][&scope=<project-id>]` | Publish the raw request body as a new single-file artifact. `scope` defaults to the caller's project (agents); users must set it. Optional header `X-Content-SHA256` (hex) is verified. Returns `201` with the artifact, its first version and any publish `warnings`. |
| `GET /api/v1/artifacts?mine=1[&q=<text>][&review_pending=1][&owner=me][&scope=<project-id>][&shared=1][&limit=<n>][&cursor=<c>]` | The artifacts the caller can read among those it owns, those shared with it directly, and those homed in projects it is a member of, newest first. See [Listing](#listing). |
| `GET /api/v1/artifacts/{id}` | The artifact and its current version, including the file manifest (`path`, `size`, `sha256`, `mediaType`, and for remote images `origin`, `sourceUrl` and `fetchStatus`; see [Images in Markdown artifacts](#images-in-markdown-artifacts)), and `canManage: true` when the caller may share and change the artifact (the owner, or a user with an admin grant). |
| `GET /api/v1/artifacts/{id}/files/{path}[?resolve=clean\|accept]` | A file of the current version. With `resolve`, see [Resolving marks](#resolving-marks). |
| `GET /api/v1/artifacts/{id}/versions/{seq}/files/{path}[?resolve=clean\|accept]` | A file of version `seq`. |
| `POST /api/v1/artifacts` (JSON manifest) | Start a two-step publish: a new artifact with a pending first version, or, when `key` names an artifact the caller already published in the scope, a new pending version of it. Returns `201` with the artifact, the pending version and `upload.required`, the paths to upload. |
| `POST /api/v1/artifacts/{id}/versions` (JSON manifest) | Start a new pending version of an artifact. With `"kind": "review"`, a review of the current version (`409` with code `nothing_to_review` when the artifact has no published version). |
| `PUT /api/v1/artifacts/{id}/versions/{seq}/files/{path}` | Upload one file of a pending version (raw body). Its size and SHA-256 must match the manifest; `X-Content-SHA256`, when sent, must too. Returns `204`. |
| `POST /api/v1/artifacts/{id}/versions/{seq}/finalize` | Make a pending version ready once every file has arrived; it becomes the current version unless a later one already is. Returns `200` with the artifact, the version and any `warnings` (remote images that could not be fetched); `409` with code `incomplete` and `details.missing` while files are missing. For a review, the optional JSON body `{"base": <seq>}` is required: `400` with code `base_required` without it, `422` with code `unmarked_changes` or `409` with code `stale_review`, both discarding the review (see [Reviews](#reviews)). |
| `POST /api/v1/artifacts/{id}/versions/{seq}/view` | For a version whose entry is HTML: a view URL for showing it in a sandboxed frame (`url`, `expiresAt`, and `remoteImages` when the entry references images on other servers). Requires read access; the URL is valid for 30 minutes. |
| `GET /api/v1/artifacts/view/{capability}/{path}` | A file of the version the view URL was issued for; see [HTML artifacts](#html-artifacts). |
| `PATCH /api/v1/artifacts/{id}` | Change an artifact: `{"expiresAt": "<time>"}` sets the expiry (`null` clears it; a time in the past is refused), `{"scopeRef": "<project-id>"}` moves it. Admin only. Both changes apply together or not at all. Returns `200` with `artifact`, and `linksCutShort` and `grantsRemoved` when it has an expiry; `403` with code `cross_project_sharing_disabled` for a move while that is off; `409` when the owner already has an artifact with the same key in the target project, with code `too_many_grants` when the move would exceed the grant limit, or with code `home_admin_grant` when the target project holds an admin grant on the artifact. See [Expiry, moving and deletion](#expiry-moving-and-deletion). |
| `GET /api/v1/artifacts/{id}/grants` | The artifact's grants (`id`, `subjectKind`, `subjectRef`, `permission`, `home`, `createdAt`, `createdBy`), the home project's first, and `crossProjectSharing`: whether grants to other projects (and moves to them) are on. Admin only. |
| `POST /api/v1/artifacts/{id}/grants` | Add a grant, `{"subjectKind": "principal" or "scope", "subjectRef": "user:<id>", "agent:<id>" or a project id, "permission": "read", "write" or "admin"}`, or change the permission of the subject's existing grant. Returns `201` when it added one and `200` when it changed one; `400` for an admin grant to the home project; `403` with code `cross_project_sharing_disabled` for another project while that is off; `409` with code `too_many_grants` at the cap. See [Grants](#grants). |
| `DELETE /api/v1/artifacts/{id}/grants/{grantId}` | Remove a grant. Returns `204`; `409` for the home project's grant. |
| `POST /api/v1/artifacts/{id}/links` | Create a share link. Optional body `{"ttlHours": <n>}`. Returns `201` with `link` (`id`, `createdAt`, `expiresAt`, `createdBy`), `url` (the link, shown only here) and `clampedToArtifactExpiry` when the artifact's own expiry shortened it; `400` with code `ttl_too_long` for a lifetime above the maximum (rejected, not shortened), `409` with code `too_many_links` at the per-artifact cap, `409` for an artifact with no published version. See [Share links](#share-links). |
| `GET /api/v1/artifacts/{id}/links` | The artifact's unexpired share links, oldest first, without tokens. |
| `DELETE /api/v1/artifacts/{id}/links/{linkId}` | Revoke a share link. Returns `204`. |
| `GET /api/v1/artifacts/shared/{token}[/files/{path}]` | Open a share link, with no credentials: `303` to a view URL. |
| `GET /api/v1/artifacts/{id}/versions[?limit=][&before=]` | The ready versions, newest first, without their files: up to `limit` (default 100, at most 500) with a version number below `before`; `nextBefore` in the response gives the next page. |
| `GET /api/v1/artifacts/{id}/versions/{seq}` | One ready version with its files, and `canManage` as for `GET /api/v1/artifacts/{id}`. |

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
| `scope` | Keep only artifacts homed in this project (a project ID) or shared with it; shared ones carry `sharedWithScope: true`. Artifacts shared with the project are included only when the caller may read in that project; otherwise only those homed there are kept. It narrows the list and never adds an artifact the list would not show without it. The project's Artifacts view in the web UI uses it. |
| `shared=1` | With `scope`, keep only the artifacts shared with that project from elsewhere (an empty list when the caller may not read in that project). Without `scope`, keep only the artifacts a grant shares with the caller (`access` is `shared`). |
| `limit` | Page size, 1-100 (default 50). |
| `cursor` | The `nextCursor` of the previous page. |

The response is `{"artifacts": [...], "nextCursor": "..."}`. Each entry has the same fields as `artifact` in the single-artifact response, plus `reviewPending` and `access`: `owned` (the caller owns it), `project` (the caller may read artifacts of its home project) or `shared` (a grant to the caller or to one of its projects). An artifact whose home project was deleted has `scopeDeleted: true`, and on those rows `canManage: true` when the caller may move it and cross-project sharing is on (moves need it). `nextCursor` is absent on the last page. Cursors are opaque and only work for the same caller and the same `q`, `review_pending`, `owner`, `scope` and `shared` values. A page may hold fewer than `limit` entries and still have a `nextCursor`, because one request examines a bounded number of rows. Keep following the cursor until it is absent. Artifacts created or updated while you page move to the front of the order: they appear on a fresh listing, not later in the current walk.

**File delivery.** On a hub with local storage the hub streams the bytes. On a hub with object storage (GCS) it answers `302` to a short-lived signed URL; add `?stream=1` to have the hub serve the bytes itself (the web page does this for text). Either way the response carries `Content-Disposition` (`inline` only for plain text, Markdown, CSV, TSV, JSON, YAML, TOML and raster images; `attachment` otherwise) and `X-Content-Type-Options: nosniff`; streamed responses also carry a sandboxing `Content-Security-Policy` and an `ETag`.

**Permissions.** Reading checks `artifact.read` in the artifact's home project; publishing checks `artifact.create` in the target project. Adding a version to an existing artifact also checks `artifact.create`, in the artifact's home project, and requires being its owner or holding a write grant on it. Project owners, admins and members hold both for their project; sharing an existing artifact, changing its expiry and moving it are left to its owner and to users it grants admin. Agent tokens carry them through the `project:artifact:read` and `project:artifact:write` scopes: `readonly`, `baseline` and `full` agents can read, `baseline` and `full` agents can publish, and no agent can delete artifacts or manage their grants. An agent token without `project:artifact:read` gets `404` for every artifact, even one shared with it directly. A user access token is limited by its own project boundary and permissions; removing a user from a project does not by itself stop such a token from reading artifacts the user owns or was given access to, so revoke the token for an immediate cut-off. Artifact access uses dedicated agent scopes; agents created from a credential issued before artifacts existed need to be recreated from a current credential before they can use artifacts.
