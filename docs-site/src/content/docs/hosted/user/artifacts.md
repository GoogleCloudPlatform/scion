---
title: Using Artifacts
description: Publish files and folders as artifacts, view and review them, share them, and attach them in chat, from the web UI or the CLI.
---

An **artifact** is a file or a folder that you or an agent publish to the Hub. It gets a stable reference, `scion://artifact/<id>`, that works from any runtime broker and in the web UI, and it keeps every version you publish. The Hub stores the bytes, so readers never need access to the publisher's workspace or shared directories.

This guide walks through the common tasks. For every flag, limit, status code and API route, see the [Artifacts reference](/scion/reference/artifacts/).

:::caution[Experimental]
Artifacts are behind the `hub.artifacts` [experiment](/scion/reference/experiments/), which is off by default, and they need Hub mode. While the experiment is off, the web UI shows no artifact pages and every artifact command fails. See [The artifacts experiment is off](#the-artifacts-experiment-is-off).
:::

## Who does what

Each task below says where it can be done:

- **Web** steps are for people signed in to the web UI.
- **CLI** steps work for people using `scion` on their own machine, and for agents, unless the step says *users only*.
- Agents have no web UI. They publish, fetch and list versions with `scion artifact`, and send references with `scion message`. Sharing, deleting and changing an artifact's lifetime are for users. See [What agents cannot do](#what-agents-cannot-do).

Who can open an artifact:

- its **owner**, the user or agent that published it;
- members of its **home project**: the agent's own project, or the project a user published into;
- anyone it was **shared** with through a grant.

Anyone else gets the same "not found" answer as for an artifact that does not exist.

## Artifact or chat attachment?

The chat composer's paperclip offers both **Upload file** and **Attach artifact…**. They behave differently.

| | Chat file attachment | Artifact |
| :--- | :--- | :--- |
| What is sent | A copy of the file, with that one message | A reference; the file stays on the Hub |
| Versions | None | Every publish under the same key adds a version |
| Who can read it | Everyone with access to the project (see [Messaging](/scion/hosted/user/messaging/)) | The owner, the home project and the people and projects it is shared with |
| Review | No | Yes, with suggestion and comment marks |
| Sharing on its own | No; it goes where the message goes | Grants to other people and projects, and share links |
| Folders and HTML sites | No | Yes; HTML renders in a sandboxed frame |

Use a **chat attachment** for a quick, one-off file in a conversation, such as a screenshot or a log excerpt. Use an **artifact** for anything that will change, be reviewed, be read from another runtime broker, or be shared beyond the project: a design document, a report, a small HTML site.

## Publish a file or a folder

### CLI

```bash
scion artifact publish design.md --title "Artifact system design" --key design
```

The command prints the artifact's reference with its version, and the URL of its page in the web UI:

```text
scion://artifact/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d  (v1)
https://hub.example.com/projects/<project-id>/artifacts/5f1c2d3e-6b1a-4c55-9f3e-0d6e7a1b2c3d
```

To publish a folder, name the folder. The **entry** file is the one the web page opens first and `scion artifact get` prints; it defaults to `index.html`, `index.md` or `README.md` at the top of the folder, or to the only file. Use `--entry` to choose another:

```bash
scion artifact publish ./report --entry index.html --title "Q3 report" --key q3-report
```

Things to know before you publish:

- **Choose a descriptive key.** `--key` is how later publishes find the same artifact, and readers can see it. Name the content (`q3-report`), not yourself or the agent.
- **Hidden files are left out.** Files and folders whose names start with `.` are not published, and symbolic links inside the folder are refused. Keep build tools and sources outside the folder you publish, so that the folder holds exactly what readers should get.
- **Size limits apply.** By default a file may be up to 32 MiB, a version up to 256 MiB and 200 files; your Hub may set other limits.
- **Where it is homed.** An agent's artifact is homed in the agent's project. When you publish as a user, the artifact is homed in the Hub project that your checkout is linked to; link it first with `scion hub link` if the CLI asks you to.

### Web

Open a project, go to its **Files** area and choose the **Artifacts** view. **New artifact** uploads a file, or a folder with a chosen entry file, with a title, an optional key and an optional note. The new artifact is homed in that project.

You can also start from chat: when the **Attach artifact…** picker has nothing to list, it offers **New artifact**, and the new artifact is attached to your message.

## Publish new versions and get an artifact

Each artifact has numbered versions. A version never changes once it is published; the newest one is the **current** version. `scion://artifact/<id>` always names the current version, and `scion://artifact/<id>@<n>` names version `n` for good.

### Add a version (CLI)

Publish again under the same key, optionally with a note:

```bash
scion artifact publish design.md --key design --note "Addressed review comments"
```

Or name the artifact directly, which also works for an artifact without a key:

```bash
scion artifact publish design.md --version-of scion://artifact/5f1c2d3e-...
```

With `--version-of`, a single file replaces the current version's entry file, whatever the local file is called, and the version's other files are kept. A folder replaces the whole bundle. You can add versions to artifacts you own and to artifacts shared with you with edit access.

### Add a version (web)

On the artifact's page, **Edit** opens a Markdown or text entry file in an editor, and **Publish new version** saves your change as the next version. **Upload new version**, on the **History** tab, publishes a file or a folder as the next version. These buttons appear only when you may publish versions of that artifact.

### List versions and get an artifact (CLI)

```bash
scion artifact versions scion://artifact/5f1c2d3e-...
```

lists the versions, newest first, with who published each, when, and its note. The current version is marked `*`. Version numbers can skip: a version that was discarded, such as a refused review, keeps its number. The list shows why.

`scion artifact get` fetches an artifact from any runtime broker:

```bash
# The entry file of the current version, to stdout
scion artifact get scion://artifact/5f1c2d3e-... > design.md

# Every file of version 2, into a folder
scion artifact get scion://artifact/5f1c2d3e-...@2 --out ./design-v2/
```

`get` checks every file against what the Hub recorded at publish time, and it does not replace an existing file under `--out` unless you add `--force`.

## View an artifact

### Web

The **Artifacts** item in the sidebar (under *Management*) lists the artifacts you own, those shared with you and those published in your projects. Search by title or key, or narrow the list with *Review pending*, *Owned by me* and *Shared with me*. A project's **Artifacts** view lists the artifacts of that one project, including those other projects shared with it.

Opening an artifact shows its page with three tabs:

- **Preview** shows the entry file. Markdown is shown formatted; text and code (including JSON, YAML and CSV) in a read-only editor; PNG, JPEG, GIF and WebP images inline. Other types, such as SVG and PDF, are offered as a download.
- **Files** lists every file of the version, and opens or downloads each one.
- **History** lists the versions. Use the **Version** menu in the header, or a version's link here, to open an earlier one.

**Copy reference** in the header copies the version's `scion://artifact/` reference, ready to paste into a message or a command.

### How HTML artifacts render

An artifact whose entry file is HTML, such as a single page or a small site published as a folder, renders in a **sandboxed frame**:

- **Relative links work.** The page loads its own files from the same version, so `css/site.css` and `img/chart.png` resolve.
- **Scripts run, isolated.** The page's scripts run in an isolated origin with no access to the Hub page around it, your session or the Hub API. They cannot make network requests, open windows, submit forms or navigate away.
- **Keep every asset in the bundle.** Images, styles, scripts and fonts load only from the bundle's own files. Images referenced by absolute `http(s)` URL do not load; the publish prints a warning, and the viewer shows one above the frame.
- **The view expires.** A view lasts 30 minutes. After that the page says so and offers **Reload view**. **Full screen** and **Open in new tab** are on the bar above the frame.

Markdown artifacts are different: when you publish one, the Hub fetches the remote images it references and serves copies from then on. Images that could not be fetched show a placeholder instead.

### CLI

Agents and CLI users fetch the files with `scion artifact get` and read them locally. For an HTML site, fetch the whole bundle with `--out <folder>`.

## Review an artifact

A review is a version that carries suggestion and comment marks (in [CriticMarkup](https://github.com/CriticMarkup/CriticMarkup-toolkit)), so the owner can see exactly what the reviewer proposes and decide what to apply. A review may only add marks: if text outside the marks changed, the Hub refuses the review.

### Review in the web UI

On the current version of a Markdown artifact, choose **Review**. An editor opens beside a live preview.

1. Select text and choose **Comment**, **Suggest**, **Insert** or **Delete** to add a mark. Marks cannot be nested, and the toolbar refuses a selection that falls inside code; a short hint says why.
2. **Save review** publishes your marked-up copy as a version of kind *review*, with an optional note. It stays disabled until there is at least one mark, and while text outside your marks differs from the version you started from.
3. If someone publishes a newer version while you review, your review is not saved. The editor restarts from the current version, and your text is shown read-only with **Copy** so you can redo your marks.

While a review is the current version, the artifact shows a *Review pending* badge (also in the Artifacts list), and a banner names the reviewer and counts the marks. On a review, the preview's **Marks · Clean · Accepted** switch shows the marks, the text with every mark rejected, or the text with every mark accepted.

**Review** is shown to people who may publish versions of the artifact and to people given review access (see [Share an artifact](#share-an-artifact)). Use **Edit** for a plain edit instead of a review.

### Review from the CLI

```bash
scion artifact get scion://artifact/5f1c2d3e-... > plan.md
# add marks to plan.md, for example {~~old text~>new text~~} or {>>a comment<<}
scion artifact publish plan.md --review scion://artifact/5f1c2d3e-...
```

The [reference](/scion/reference/artifacts/#reviews) lists every mark and the rules for code.

### Act on a review (owner)

When an agent owns the artifact, the Hub sends it a message when someone else's review is published, with the commands to read it. To read a review:

```bash
scion artifact get scion://artifact/5f1c2d3e-...             # with the marks
scion artifact get scion://artifact/5f1c2d3e-... --clean     # every mark rejected
scion artifact get scion://artifact/5f1c2d3e-... --accept    # every mark accepted
scion artifact get scion://artifact/5f1c2d3e-... --kind publish  # the newest non-review version
```

Apply or answer the marks, then publish the resolved text as the next version with `--version-of`. The *Review pending* badge clears once an ordinary version is current again.

## Share an artifact

Sharing is for **users only**: the artifact's owner, or a user it gave *Can manage*. Agents cannot share artifacts, including their own, so nobody can share an artifact owned by an agent beyond its home project (see [What agents cannot do](#what-agents-cannot-do)).

On the artifact's page, **Share** opens a dialog with three parts.

### People and projects (grants)

Search for a user, or for a project when the Hub's cross-project sharing setting is on, pick a permission and choose **Add**:

| Permission | Allows |
| :--- | :--- |
| *Can view* | Open the artifact and its versions |
| *Can edit* | Also publish new versions, and review |
| *Can manage* | Also share it, change its lifetime, move it and delete it |

A grant to a project covers its members and its agents. The home project is listed first: it always has at least *Can view*, can be raised to *Can edit*, and cannot be removed. Grants stay until they are removed or the artifact is deleted.

### Share links

A share link lets anyone who holds it open the artifact's **current** version in a browser **without signing in**. Use one for readers who do not have an account on the Hub.

- Choose a lifetime (1, 7 or 30 days) and **Create link**. The link is shown **once**, with **Copy**; copy it then.
- Treat the link like a password: anyone holding it can open the artifact until it expires or you **Revoke** it. Every link expires, at the latest when the artifact itself expires.
- An HTML entry renders in its sandbox; a Markdown entry opens as plain text.
- A link gives no other access: it does not add the artifact to anyone's list.

From the CLI, users can do the same with `scion artifact share`:

```bash
scion artifact share scion://artifact/5f1c2d3e-... --ttl 7d
scion artifact share scion://artifact/5f1c2d3e-... --list
scion artifact share scion://artifact/5f1c2d3e-... --revoke <link-id>
```

### Grant review access to an agent's artifact

Because an agent cannot share its own artifacts, it cannot choose who reviews its work. For an artifact **owned by an agent**, these users may give a person review access even though they cannot otherwise manage the artifact:

- the user who delegated to the agent;
- the project owners and admins of the artifact's home project, and super-admins.

They see **Grant review access** on the artifact page instead of **Share**. Search for a user and choose **Add** to give them *Can review*: they can then publish reviews, and nothing else.

Review access does not give read access. The person must already be able to read the artifact, through its home project or a grant to one of their projects; if that access ends, so does review. It is refused for someone who already holds a grant of their own on the artifact; check that grant, because *Can edit* and *Can manage* already include review.

## Attach artifacts in chat

### Web

In a conversation, choose the paperclip and then **Attach artifact…**. The picker lists the artifacts you can read, with search and **Owned by me** and **This project** filters. Pick up to 10; they show as chips above the input and are sent as references to their current version, never as copies.

In a message, each artifact shows as a chip with its title, version and owner. Clicking a chip, or a `scion://artifact/` link in the text, opens a preview over the conversation, with **Copy link** and **Open in artifact viewer**. Markdown and HTML render there as on the artifact page. Each reader sees what their own access allows: a reader who cannot open the artifact sees **Artifact unavailable**.

### CLI

```bash
scion message @reviewer "Design ready for review." --artifact scion://artifact/5f1c2d3e-...
```

`--artifact` can be repeated, up to 10 times per message. An agent that receives the message gets one line per artifact with the `scion artifact get` command to fetch it.

Keep in mind:

- The Hub keeps only references that you can read, and drops the others with a warning.
- When you message one agent or one user directly, the Hub warns if the recipient may not be able to read a reference. The message is still delivered; ask whoever manages the artifact to share it with the recipient, or check the recipient agent's scopes (see [`missing_scope`](#missing_scope-when-an-agent-publishes)).
- Some messages do not carry artifact references, among them group (`group[...]`) and broadcast messages, @-mention copies, and messages that arrive through chat plugins. See [Artifacts in messages](/scion/reference/artifacts/#artifacts-in-messages).

## Expiry, moving and deleting

These actions are for **users** who manage the artifact. Agents cannot do any of them.

- **Expiry.** In the Share dialog, **Retention** sets when the artifact expires: never, or 1, 7, 30 or 90 days from now. Before you save, the dialog says how many share links end early and how many grants go with the artifact. From the moment it expires, nobody can open it and its share links stop working. Your Hub may also give new artifacts an expiry by default.
- **Moving.** **Move…** gives an artifact a new home project, for example after its project was deleted. It appears on the artifact page, and in the Artifacts list on rows whose home project was deleted, for the owner and users with *Can manage*, while the Hub's cross-project sharing setting is on. You must be allowed to publish in the new project. The old home project loses its access unless you add a grant for it.
- **Deleting.** Run `scion artifact delete <ref>` (users only; add `--yes` when not at a terminal). The owner, a user with *Can manage*, and the project owners and admins of the artifact's home project (and super-admins) can delete it. Deleting ends its grants and share links, and chat references to it show as unavailable. **There is no undelete.**

Deleting a project does not delete its artifacts, and it does not revoke their share links. Moving the artifact does not end them either. To end that access, revoke the links, set the artifact to expire under **Retention**, or delete it.

## What agents cannot do

Agents use the same `scion artifact` command, but in agent mode:

- **No share links.** `scion artifact share` is not available to agents, and the Hub refuses an agent's request to create one, even for an artifact the agent owns.
- **No grants.** Agents cannot add, change or remove grants, including review access.
- **No deleting, expiry changes or moves.** `scion artifact delete` is not available to agents.
- **Publishing needs the right agent role.** Agents started with `--role readonly` can read artifacts but not publish them; `baseline` and `full` agents can do both. An agent can publish new versions or reviews of someone else's artifact only with *Can edit* on it, granted to the agent or to its project.

An artifact an agent owns has no user owner and no one with *Can manage*, so nobody can share it beyond its home project, and **Grant review access** reaches only people who can already read it. When the work needs readers or reviewers outside the project, the user who delegated to the agent can fetch it (`scion artifact get <ref> --out <folder>`) and publish it as their own artifact, which they can then share.

## Troubleshooting

### "Not found" for an artifact

A `404` means that the artifact does not exist **or** that you cannot read it. The Hub gives the same answer for both, by design, so it does not reveal whether an artifact exists. Check the reference, then ask whoever manages the artifact (its owner, or a user with *Can manage*) to share it with you or your project. An artifact owned by an agent cannot be shared beyond its home project; see [What agents cannot do](#what-agents-cannot-do).

### `missing_scope` when an agent publishes

`scion artifact publish` fails with code `missing_scope` and a message such as "the credential does not carry the project:artifact:read scope needed to publish artifacts". The agent's token lacks the artifact scopes. The error names every missing scope: `project:artifact:read` to use artifacts at all, and `project:artifact:write` to publish. Without `project:artifact:read`, every read answers `404`, even for an artifact shared with the agent.

- **Check.** `scion whoami --full`, run inside the agent, lists the token's scopes.
- **Usual cause.** The agent's creator does not hold the artifact scopes, so they were left out of the agent's token. The Hub warns about this when it creates or starts the agent. Recreating the agent from the same parent gives it the same scopes, so that does not help.
- **Fix.** Once the creator holds the scopes, a Hub super-admin runs `scion reset-auth <agent> --reissue-scopes`, parents before children (see [Reset Auth and Scope Re-issue](/scion/reference/reset-auth/)). Agents created from an access token issued before artifacts existed need to be recreated from a current credential.
- **Role.** A `readonly` agent can never publish; use a `baseline` or `full` agent.

### The artifacts experiment is off

While `hub.artifacts` is off:

- **Web:** there is no *Artifacts* item in the sidebar, artifact pages show "not found", the project Files area has no *Artifacts* view, and the chat paperclip offers only file upload.
- **CLI:** every artifact request answers `404`. `scion artifact publish` adds a hint that the experiment may be off; `get` lists it among the possible causes.
- **Check.** Inside an agent, `scion whoami --full` lists the experiments that are on now. A Hub admin turns the experiment on under **Admin → Server Config → Experiments** (see [Experiments](/scion/reference/experiments/)).

If the experiment is on but artifacts are disabled in the Hub's settings, the web controls still appear, but every artifact request behind them, and every `scion artifact` command, answers `404` (ptone/scion#4932). Messages sent with `--artifact` lose their references, with a warning. Ask a Hub admin to check the Hub's `artifacts` settings.

### A review is refused

- **Text outside the marks changed** (`unmarked_changes`). The page, or the CLI, names the lines that changed outside marks. Undo those changes, or mark them, and save again. Use **Edit** or `--version-of` for a plain edit.
- **A newer version was published, or is being published** (`stale_review`). Review the current version again, once any version being published has finished.

### An HTML artifact looks broken

Check that every image, style sheet and script is inside the published folder and linked by a relative path. Remote images do not load in HTML artifacts, and scripts cannot fetch anything from the network.

## Related

- [Artifacts reference](/scion/reference/artifacts/): every flag, limit, permission rule and API route.
- [Messaging & Notifications](/scion/hosted/user/messaging/): chat, attachments and conversations.
- [Experiments](/scion/reference/experiments/): turning on `hub.artifacts`.
