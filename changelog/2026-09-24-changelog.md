# Release Notes (2026-09-24)

Hub authorization was tightened across resource, workspace, and template routes, and symlink confinement was extended to attachment ingest. Hybrid tier Phase 2 shipped NFS shared-directory hardening, and the Hub now accepts Google bearer tokens directly for A2A.

## 🚀 Features
* **Hybrid tier Phase 2** (#1877, #1889): Adds `pkg/shareddirs`, a confined resolver with an O_NOFOLLOW component walk and an inode-anchored root for every NFS shared-dir operation (browser, archive, attachments, config delete). Hardens leaf modes and default ACLs (setgid `2775` leaves, `g:scion` default ACL, with fallback when ACLs are unsupported). Removes a project's NFS tree on project delete, fails closed on misconfiguration, and logs the layout at startup. Docs cover a safe export cutover with rollback steps and a corrected fstab entry. Local/unset behavior is unchanged.
* **Google bearer pass-through for A2A** (#1880): The Hub accepts Google ID tokens and OAuth access tokens directly in `Authorization: Bearer`, and the A2A bridge gains a `hubBearer` scheme that forwards the caller's token verbatim. Trust is configured per issuer (`expected_audience`, `allowed_domains`, `allowed_gcp_projects`). The Google path only runs after existing auth fails, so current clients are unaffected. The GE token exchange is now deprecated.
* **Out-of-box Vertex AI on single-node VM** (#1883): Deploy sets `GOOGLE_CLOUD_PROJECT`/`GOOGLE_CLOUD_LOCATION` for the harness, grants the VM service account `roles/aiplatform.user`, and enables the Vertex AI API, so a fresh hub can run inference without manual setup.
* **Inter-agent marker layout** (#1871): Expanded inter-agent markers in DM view gain date dividers, a two-line layout (24h time and sender→recipient above an indented body), and a timestamp in the full-content dialog.
* **Profile timezone in web settings** (#1884): Adds a timezone field to the profile settings page, with client-side IANA validation.

## 🔒 Security
* **Hub resource route authorization** (#1886): Authorizes once in the dispatcher for agent status updates (non-agent callers need update access), harness config routes (brokers keep read-only access), and project GitHub settings. Chat search now returns DM threads only to their participants.
* **Project workspace route authorization** (#1882): All project workspace endpoints (files, archive, pull, cache, sync status, WebDAV, legacy groves alias) go through one dispatcher that checks project access first. Non-read methods require update access.
* **Template file handler hardening** (#1881): The template files subtree checks access on the specific template before any read or write, and validates file paths the same way as workspace file handlers.
* **Cross-project agent delete** (#1875): Broker agent delete re-resolved agents by bare slug, so it could remove a same-slug agent's container, VM, or files in another project. Delete is now scoped to the requested project on every runtime, returns 404 when nothing matches, and fails closed on ambiguous matches.
* **Attachment ingest symlink confinement** (#1876): Attachment ingest and staging now resolve through an `os.Root` anchored on the project scratchpad, closing symlinks at intermediate directory components. A shared directory that is itself a symlink is refused.
* **Skill download capability URLs** (#1874): Local-storage Hubs issue 15-minute HMAC-SHA256 capability URLs bound to the exact skill, version, and path, signed after the creator's access check. This fixes 401s on broker skill downloads at dispatch. Cloud presigned URLs are unchanged.
* **Broker failure reason sanitization** (#1870): Broker-reported message failure reasons are stripped of control characters and invalid UTF-8 and truncated to 512 bytes before being stored or echoed into the sending agent's terminal.

## 🐛 Fixes
* **Docker agent listing failures** (#1888): `docker ps --format '{{json .}}'` made the daemon compute container sizes, which raced on busy agent `/tmp` dirs and broke agent listing, message delivery, and `scion look`. The runtime now requests only the five fields it parses.
* **Scheduled agent identity** (#1872): Agents dispatched by schedules now get `CreatorName` and the project-default GCP identity, gated by the same service-account authorization checks as manual agent creation.

## 🔧 CI & Infrastructure
* **Nightly manifest merge race** (#1887): Nightly manifest PRs now merge directly with retries instead of `gh pr merge --auto`. Auto-merge was rejected whenever the non-required CLA check had already failed.

## 📖 Docs
* **Nightly doc update** (#1879): Nightly update for 2026-09-23 covering JWT proxy auth, hybrid tier NFS storage, symlink hardening, the notification chime, and touch/mobile chat fixes.
