---
name: scion-artifacts
description: Use when publishing a document, report, folder or small HTML site for others to read or review, adding a version, reviewing an artifact with CriticMarkup, fetching or listing artifacts, or sending a scion://artifact reference in a message. Covers scion artifact publish, get, versions, comments, list and view, scion message --artifact, and what agents cannot do.
experiment: hub.artifacts
---

# Scion Artifacts

An **artifact** is a file or folder you publish to the Hub. It gets a stable
reference, `scion://artifact/<id>`, that works from any Runtime Broker and in
the web UI, and it keeps every version. The Hub stores the bytes, so readers do
not need access to your workspace or shared directories.

## When to use one

Publish an artifact for content that will change, be reviewed, be read from
another broker, or be found later: a design, a report, a plan, a small HTML
site. Then send its reference in a message instead of pasting the content.

Use `scion message --attach <file>` instead for a one-off file that belongs to
one message, such as a log excerpt.

Who can read an artifact: you (its owner), the members and agents of its home
project (your project), and anyone a user shared it with. Anyone else gets
"not found", the same answer as for an artifact that does not exist.

## Publish

```bash
scion artifact publish design.md --title "Artifact system design" --key design
scion artifact publish ./report --entry index.html --title "Q3 report" --key q3-report
```

The command prints the reference with its version, and the artifact's web page:

```text
scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d  (v1)
https://hub.example.com/projects/<project-id>/artifacts/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
```

- `--title` names the artifact when it is created (default: the entry file
  name). It is ignored when a publish adds a version.
- `--key` is how later publishes find the same artifact. Readers can see it, so
  name the content (`q3-report`), not yourself.
- `--expires-in` sets how long the artifact is kept (for example `720h`, `30d`,
  or `never`). It applies when creating an artifact or publishing a new version
  under `--key`. For agents, the minimum is `24h`, and `never`, a duration
  longer than the Hub's default retention, or lengthening an existing artifact's
  expiry under `--key` is allowed only when the Hub has no default retention
  (`default_retention_days` is `0`).
- **Folders** are published as a bundle of their regular files. Files and
  folders whose names start with `.` are left out; symbolic links inside the
  folder are refused. Keep sources and build tools outside the folder.
- The **entry** file is the one the web UI opens and `get` prints: `--entry`,
  or else `index.html`, `index.md` or `README.md` at the top of the folder.
- **HTML** entries render in a sandboxed frame. Keep every image, style sheet
  and script in the bundle and link it by a relative path; remote images do not
  load and scripts cannot reach the network. Publishing warns about remote
  images.
- By default a file may be up to 32 MiB, and a version up to 256 MiB and 200
  files; the Hub may set other limits.

## Add a version

Publish again under the same key, optionally with a note:

```bash
scion artifact publish design.md --key design --note "Addressed review comments"
```

A key belongs to its publisher within a project: this adds a version only to
your own artifact with that key. Files unchanged since the current version are
not uploaded again.

Or name the artifact, which also works without a key and for an artifact
someone gave you edit access to:

```bash
scion artifact publish design.md --version-of scion://artifact/<id> --note "Round 2"
```

With `--version-of`, a single file replaces the current version's entry file,
whatever the local file is named, and the bundle's other files are kept. A
folder replaces the whole bundle. A reference with `@<seq>` must name the
current version, or the CLI stops before uploading.

`scion://artifact/<id>` always names the current version;
`scion://artifact/<id>@<seq>` names one version for good. A version never
changes once published.

## Fetch, list and inspect

```bash
scion artifact get scion://artifact/<id> > design.md
scion artifact get scion://artifact/<id>@2 --out ./design-v2/
scion artifact versions scion://artifact/<id>
scion artifact list
scion artifact list --mine --query report
```

- `get` writes the entry file to stdout, or with `--out` (`-o`) a file, or every
  file of a bundle under a folder. It checks each file against what the Hub
  recorded at publish, and it does not replace an existing file unless you add
  `--force`. A bare `<id>` works as a reference too.
- `versions` lists versions newest first, with kind, publisher, time, size and
  note; the current one is marked `*`. Numbers can skip (a refused review keeps
  its number).
- `list` prints one page of the artifacts you can read, newest first; `--all`
  follows every page. Filter with `--mine`, `--shared`, `--reviewed`,
  `--query <text>` or `--project-id <project-id>`.

### Check an HTML artifact

```bash
scion artifact view scion://artifact/<id> --check
```

`view --check` reports every `href` or `src` that names no file of the version
(or starts with `/`, or climbs above the bundle), and fails if it finds one.
Run it after publishing an HTML site. `view` also creates a short-lived view
link; in agent mode it prints only the version and the expiry, and
`--show-link` prints the link too. The link opens the page without signing in
and cannot be revoked: do not paste it into messages or tickets. Send the
`scion://artifact/` reference instead.

## Review an artifact

A review is a version of kind `review`: a copy of the current version with
[CriticMarkup](https://github.com/CriticMarkup/CriticMarkup-toolkit) marks.

| Mark | Meaning |
|---|---|
| `{++text++}` | Insert |
| `{--text--}` | Delete |
| `{~~old~>new~~}` | Replace |
| `{>>text<<}` | Comment |
| `{==text==}` | Highlight; put a comment right after it |

```bash
scion artifact get scion://artifact/<id> > plan.md
# add marks to plan.md
scion artifact publish plan.md --review scion://artifact/<id> --note "Two questions on rollout"
```

- **Change nothing outside marks.** The Hub rejects the review
  (`unmarked_changes`) and lists the lines when the text with every mark
  rejected differs from the version you reviewed.
- Marks do not nest. Text in Markdown code is literal: to suggest a code change,
  mark around the code, for example ``{~~`old()`~>`new()`~~}``, or comment
  beside it.
- If another version is published before your review is saved, it is refused
  (`stale_review`). Fetch the current version and review again.
- The review becomes the current version, and the owner is notified.

### Review drafts (web UI, experimental)

When the `hub.artifacts_review_drafts` experiment is on (alpha, off by default;
requires `hub.artifacts`), human reviewers in the web UI can save in-progress
reviews as private Hub drafts (**Save Draft** / **Submit Review**, ~30-second
autosave, private `Draft` badge and **Resume draft**, and a move-to-current
flow when a newer version is published).

- **Private to the reviewer:** drafts are visible only to their author (no
  override for artifact owners, stewards, or admins), and saving or discarding a
  draft never notifies you. You are notified only when a reviewer submits the
  finalized review.
- **Retention:** unused drafts expire 30 days after their last save and are
  deleted with the artifact.
- **CLI:** there are no `scion artifact` draft commands; agents review artifacts
  with `scion artifact publish --review`.

### Request a review of your artifact

When `hub.artifacts_review_requests` is enabled on the Hub, an owning agent can
ask its delegating user or another project member to review its artifact:

```bash
scion artifact request-review scion://artifact/<id> --delegator --note "Ready for review"
scion artifact request-review scion://artifact/<id> --user <user-id> --note "Please check section 3"
scion artifact review-requests scion://artifact/<id>
scion artifact request-review scion://artifact/<id> --withdraw --user <user-id>
```

- `--delegator` resolves your delegating user on the Hub (agent mode only).
- Reviewers who read the artifact via a role in your project receive a bounded
  review grant (at most 10 bounded grants per artifact) that never confers read
  access on its own.
- Use `scion artifact review-requests` to inspect open requests and read dismiss
  notes; when a reviewer dismisses your request, the Hub also delivers a system
  message with their note.
- `--withdraw` withdraws an open request you created and removes its bounded
  grant if no human has edited it.

### Act on a review of your artifact

When someone else reviews an artifact you own, the Hub sends you a message with
the commands to read it.

```bash
scion artifact comments scion://artifact/<id>
scion artifact get scion://artifact/<id> --clean
scion artifact get scion://artifact/<id> --accept
scion artifact get scion://artifact/<id> --kind publish
```

`comments` lists the newest review's comments, suggestions and highlights with
file and line (`@<seq>` for a specific review, `--format json` for scripts).
`get --clean` gives the text with every mark rejected, `--accept` with every
mark accepted, and `--kind publish` the newest version that is not a review.
Apply or answer the marks, then publish the result with `--version-of`. The
artifact shows *Review on latest* until an ordinary version is current again.

## Send a reference in a message

```bash
scion message @reviewer "Design ready for review." --artifact scion://artifact/<id>
```

- `--artifact` takes `scion://artifact/<id>[@<seq>]`, repeatable up to 10
  times. The reference is added to the message text, and an agent recipient
  gets the `scion artifact get` command to fetch it.
- The Hub keeps only references you can read and drops the others with a
  warning. For a direct message it also warns when the recipient may not be
  able to read a reference; the message is still delivered.
- `--artifact` cannot be used with `group[...]` recipients.
- A plain `scion://artifact/<id>` in the message text is also shown as a link
  in the web chat. Keep it out of backticks.

## What agents cannot do

Sharing, grants, deleting and moving are for users (agents can set publish-time
retention with `--expires-in`, subject to the Hub's limits, but cannot change
expiry afterwards). In an agent,
`scion artifact share`, `scion artifact grants` and `scion artifact delete`
do not exist: they fail with `unknown command "share" for "scion artifact"`
(or `"grants"`, `"delete"`). That is expected, not an old CLI.

- To make an artifact readable outside your project, ask the user who
  delegated to you, or an owner of your project, to share it.
- You can publish versions or reviews of someone else's artifact only with edit
  access, granted to you or to your project.
- An agent started with `--role readonly` can read artifacts but not publish.

## When it fails

- **`404` or "not found":** the artifact does not exist or you cannot read it;
  the Hub does not say which. Check the reference, then ask whoever manages the
  artifact to share it.
- **`missing_scope` on publish:** your token lacks the artifact scopes.
  `scion whoami --full` lists them on its `Scopes:` line. Report it to the user
  who started you; a Hub admin must reissue your scopes.
- **Every command answers `404`** (and `scion artifact list` is empty): run
  `scion whoami --full`.
  - If its `Scopes:` line lacks `project:artifact:read`, that is the cause: a
    token without it gets `404` for every artifact. Usually the user or agent
    that created you does not hold the artifact scopes, so they were left out
    of your token. Report it as for `missing_scope`.
  - If its `Experiments:` line lacks `hub.artifacts`, artifacts are off on this
    Hub. Attach files with `--attach` or point to a path under `/scion-volumes/`
    instead.
