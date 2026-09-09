---
title: Release Notes
---

Scion release notes are published weekly.

## Latest: Week of August 31 -- September 6, 2026

This week completed the authorization foundation refactor, replacing the legacy dual Policy/RoleBinding grant model with a unified, positive-authority RoleBinding system and shipping end-to-end access boundaries with a full admin UI. The Cloud Run sandbox received significant hardening for the single-node tier, including a gVisor capability discovery that forced a UID strategy change. Several critical credential-management fixes landed — notably a P0 GITHUB_TOKEN injection regression and a GCP Secret Manager mapping error that caused approximately 15 hours of Discord downtime.

[Read the full release notes for this week ->](/scion/release-notes/2026-08-31/)

## Previous Weeks

- [Week of August 24 -- 30, 2026](/scion/release-notes/2026-08-24/)
- [Week of August 17 -- 23, 2026](/scion/release-notes/2026-08-17/)
- [Week of August 10 -- 16, 2026](/scion/release-notes/2026-08-10/)
- [Week of August 3 -- 9, 2026](/scion/release-notes/2026-08-03/)
- [Week of July 27 -- August 2, 2026](/scion/release-notes/2026-07-27/)
- [Week of July 19--25, 2026](/scion/release-notes/2026-07-19/)
- [Week of July 12--19, 2026](/scion/release-notes/2026-07-12/)

---

Looking for older release notes? See the [Prior Release Notes Archive](/scion/release-notes-archive/) for daily entries from the early development period (Feb--Jul 2026).
