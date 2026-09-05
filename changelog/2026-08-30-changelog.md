# Release Notes (2026-08-30)

Per-resource permission-gated admin UI ships end-to-end (3-phase: permissions endpoint, nav guard, settings tabs), the dead project visibility field is replaced with real policy-based access control, and the messaging backfill CLI entry point lands with a resume data-loss fix.

## 🚀 Features
* **Per-resource permission-gated admin UI (#1416, #1417, #1418):** Three-phase rollout — admin-status endpoint extended with per-resource permissions array, binary admin nav/route guard replaced with per-item permission checks, and settings page tabs gated by the caller's actual permissions so a template-only role sees only the Templates tab.
* **Scheduler authorization with owner-based access control (#1415):** Scheduled events and schedules now enforce owner-based authorization with 8 new permissions, backfill seeding for existing projects, and full test coverage (996 lines added).
* **Messaging divergence diagnostics board (#1424):** Read-only admin endpoint exposing divergence counters with five machine-readable caveat fields (per-replica, since-boot, mismatch-composition, fails-open, not-go/no-go) for the conversation model migration.

## 🐛 Fixes
* **Project visibility eradicated (#1414):** Removed the non-functional Visibility field (private/team/public) and replaced it with membership-based Policy access control — per-type read policies, per-project member-read policies, CheckAccess gate on getProject, and startup backfill for existing projects.
* **Messaging backfill entry point + resume fix (#1426):** `scion server backfill` command added (dry-run default, `--execute` to apply) with startup warning for unattributed messages. DEF-81 fixed: resume checkpoint now uses compound `(created, id)` keyset cursor instead of strictly-greater-than timestamp, preventing permanent row loss on resume.
* **Default template hydration (#1429):** Embedded default template hydrated to disk when absent, fixing broker agents losing home files (#923).
* **GitCloneConfig depth zero (#1428):** `Depth` changed from `int` to `*int` so depth=0 correctly produces a full clone instead of being treated as unset (#1274).
* **405 Allow header (RFC 9110) (#1413):** All 20 MethodNotAllowed call sites now include the required Allow header with correct supported methods.
* **Admin page titles (#1412):** Missing PAGE_TITLES entries added for roles, role-bindings, and quotas pages.
* **Heredoc argv security comment (#1427):** False security comment on broker Exec heredoc replaced with a WARNING acknowledging the token exposure in outer argv.

## 🧪 Tests
* **Read-switch behavior pinned (#1425):** 23 tests pinning behavior at three ConversationReadSwitch call sites before Tranche G, including two confirmed open defects (DEF-59a slug resolution, DEF-64 manager DM visibility).

## 🔧 CI & Infrastructure
* **Race detection split (#1422):** Nightly race detection split into two parallel jobs — all packages except pkg/hub (90m) and pkg/hub with no_sqlite (45m).
* **405 Allow header lint guard (#1423):** Reporting-only CI job tracking 272 bare MethodNotAllowed calls for incremental remediation.
