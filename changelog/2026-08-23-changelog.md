# Release Notes (2026-08-23)

The local secret backend gained AES-256-GCM encryption at rest, user-scoped templates were wired end-to-end, and agent DM conversations can now be promoted to shared space threads.

## 🔒 Security
* **[Secret]:** Encrypt local secret backend at rest — adds AES-256-GCM encryption with domain-separated key derivation from the hub signing secret. Legacy plaintext values are transparently readable and re-encrypted on next write. 22 files changed, +553/−92 lines (#1253).

## 🚀 Features
* **[Hub]:** Complete user-scoped template support — wires the existing data model (schema, constants, slug lookup) to API handlers (`/api/v1/users/me/templates`), CLI resolution user step, web UI profile page, and store listing. Authorization hardened with `ScopeID` forced from auth context on both list and create to prevent cross-user poisoning/injection. 8 functional tests for ownership enforcement and scope isolation (#1255).
* **[Chat]:** Promote agent DM conversations to shared space threads — atomic message re-keying, SSE live updates, frontend button and dialog (#1256).

## 🐛 Fixes
* **[Hub]:** Respect intentional deletion of seeded policies — `hub-member-read-all` and `hub-member-create-projects` were silently recreated on every server restart after deletion via the admin API, undoing operator security fixes with no warning (#1254).
* **[Web]:** Keep the SSE feed alive across backgrounded tabs — the shared SSE client carried live updates for the whole SPA but stopped permanently after 10 failed reconnect attempts (worst case on mobile browsers). Fixes reconnection to survive tab backgrounding (#1258).
* **[Sciontool]:** Cap assistant replies in the hub's unit (runes), not bytes — the truncation hook measured `64 * 1024` bytes while the hub rejects above `MaxMessageLength` runes, causing replies in the gap to be posted whole and refused with 400, silently dropping them instead of truncating (#1257).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 22 — P0 permission escalation patches, RBAC empty-role least-privilege, default harness Claude→Antigravity, clickable file-path links, file viewer dialog (#1260).
