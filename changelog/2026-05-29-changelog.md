# Release Notes (2026-05-29)

This release focuses on the execution of **Phase 2 of the "Grove to Project" architectural rename**, a massive internal refactoring effort to modernize the codebase's core terminology.

## 🚀 Features

* **Project Terminology Migration (Phase 2):** Completed a comprehensive "Tier 1" refactor (internal identifiers and comments) across the core backend and CLI. This ensures consistent use of "Project" instead of "Grove" in function parameters, local variables, and internal documentation.
    * **Broad Package Coverage:** Systematic updates were applied to `pkg/hub`, `pkg/api`, `pkg/config`, `pkg/agent`, `pkg/runtime`, `pkg/broker`, `pkg/logging`, `pkg/hubsync`, `extras/scion-telegram`, and `cmd/`.
    * **Internal Logic Modernization:** Renamed thousands of identifiers, including critical manager state variables (e.g., `projectsToScan`, `projectSlug`, `projectScionDir`) and internal struct fields.
    * **Robust Backward Compatibility:** To ensure zero disruption, all external-facing components—including API endpoint paths (`/api/v1/groves/`), JSON tags, NATS topic prefixes (`scion.grove.`), and environment variables (`SCION_GROVE_ID`)—remain unchanged in this phase.
    * **Test Integrity:** Thousands of lines of test fixtures, assertions, and mock data were migrated to the new terminology while verifying that no regressions occurred in the underlying logic.
    * **Completion Strategy:** Introduced a new "Grove Rename Survey" and updated package-specific project logs to track the transition towards Phase 3.
