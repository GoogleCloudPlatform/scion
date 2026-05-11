# Phase 6: Web Frontend Rename

Renamed all occurrences of "grove" to "project" in the web frontend.

## Changes

### 1. Types & State
- Updated `web/src/shared/types.ts`:
    - Renamed `Grove` interface to `Project`.
    - Renamed `GroveStatus` to `ProjectStatus`.
    - Renamed `GroveType` to `ProjectType`.
    - Renamed `GitHubAppGroveStatus` to `GitHubAppProjectStatus`.
    - Renamed `groveId` to `projectId` in `Agent`, `Message`, `Notification`, `Subscription`, `GCPServiceAccount`.
    - Updated `ResourceScope` and `SubscriptionScope` enums.
    - Updated `GroupType` (`grove_agents` -> `project_agents`).
- Updated `web/src/client/state.ts`:
    - Renamed `ViewScope` types and properties.
    - Renamed state maps and sets (`groves` -> `projects`, `deletedGroveIds` -> `deletedProjectIds`).
    - Updated SSE event handling and subscription subjects (`grove.>` -> `project.>`).
    - Renamed and updated methods (`handleGroveEvent` -> `handleProjectEvent`, `seedGroves` -> `seedProjects`).

### 2. Routes & Navigation
- Updated `web/src/client/main.ts`:
    - Renamed `/groves` routes to `/projects`.
    - Updated component tags and dynamic imports.
    - Updated hydration logic.
- Updated `web/src/components/app-shell.ts`:
    - Updated `PAGE_TITLES` and `getPageTitle` to use Project and `/projects`.
- Updated `web/src/components/shared/nav.ts`:
    - Renamed "Groves" to "Projects" in sidebar navigation.
- Updated `web/src/components/shared/breadcrumb.ts`:
    - Renamed "Groves" to "Projects" in breadcrumb labels.

### 3. Page Components (Renamed Files)
- `web/src/components/pages/groves.ts` -> `projects.ts`
- `web/src/components/pages/grove-detail.ts` -> `project-detail.ts`
- `web/src/components/pages/grove-settings.ts` -> `project-settings.ts`
- `web/src/components/pages/grove-create.ts` -> `project-create.ts`
- `web/src/components/pages/grove-schedules.ts` -> `project-schedules.ts`

### 4. Page Content & Logic
- Updated all page components to use `projectId`, `project` state, and the new `/api/v1/projects` endpoints.
- Updated UI text, labels, placeholders, and descriptions to use "Project" instead of "Grove".
- Updated CSS classes and selectors (`.grove-section` -> `.project-section`, etc.).
- Fixed issues with `project_agents` group type and related icons/labels in `admin-groups.ts` and `admin-group-detail.ts`.
- Updated `admin-scheduler.ts` to show project usage and usage quotas.
- Updated `github-app-setup.ts` to manage GitHub App installations for projects.
- Updated `home.ts` dashboard stats and quick actions.
- Updated `terminal.ts` toolbar back links.

### 5. Shared Components
- Updated `env-var-list.ts`, `secret-list.ts`, `shared-dir-list.ts`, `token-list.ts`, `gcp-service-account-list.ts` to use project-scoped APIs and terminology.
- Updated `subscription-manager.ts` and `scheduled-event-list.ts`.
- Updated `git-remote-display.ts` property name and labels.
- Updated `agent-message-viewer.ts` comments and UI text.
- Updated `debug-panel.ts` to track project state.
- Updated `resource-styles.ts` comments and documentation.

### 6. Tests & Documentation
- Updated `web/package.json` description.
- Updated `web/AGENTS.md` and `web/README.md`.
- Updated test scripts in `web/test-scripts/`:
    - `realtime-lifecycle-test.js`
    - `sse-curl-test.sh`
    - `screenshot-debug.js`

## Verification Results
- Successfully built the web frontend using `npm run build`.
- No remaining occurrences of "grove" or "groveId" (case-insensitive) in `web/src/` or `web/test-scripts/`.
- Verified component property bindings (e.g. `.project=${this.project}`) are consistent.
