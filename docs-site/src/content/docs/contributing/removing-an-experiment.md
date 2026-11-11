---
title: Removing an Experiment
description: Checklist for retiring a hub-wide experiment, whether it graduates or is abandoned.
---

This checklist covers retiring a registered experiment from `pkg/experiments/registry.go`. Retiring happens in one of two ways:

- **Graduation:** the feature becomes permanent.
- **Abandonment:** the feature is deleted.

For how experiments work, see the [Experiments reference](/scion/reference/experiments/). This checklist was written from the first removal, the terminal workspace experiment (ptone/scion#4917). Use that PR as a worked example.

## 1. Decide and record

- [ ] Record the decision (graduate or abandon) and its evidence on the experiment's tracking issue.
- [ ] Open a removal issue that links the tracking issue.

## 2. Find every read site

- [ ] Search for the name and its exported constant: `git grep -n '<name>'` and `git grep -n '<CONST>'`. Web gates go through `isFeatureEnabled(<CONST>)`, with the constant exported from `web/src/utils/feature-flags.ts`.
- [ ] Search the hub for `requireExperiment` and `experimentEnabled` calls naming the experiment or its Go constant.
- [ ] Look for code whose only purpose is the other state: legacy routes and page adapters, fallbacks (popups, alternate links), and visibility branches that can no longer be reached. For example, a "neither mode is on" branch can't run once one mode is always on.
- [ ] Look for rollout notes next to the tests, such as a `ROLLOUT.md`.

## 3. Delete the other state

- [ ] **Graduation:**
  - Web: delete the flag-off path, the `isFeatureEnabled` calls and the exported constant, and make the flag-on path unconditional.
  - Hub: unwrap the `requireExperiment` routes and `experimentEnabled` branches so the flag-on behaviour always runs, and delete the experiment's Go constant in `pkg/experiments/registry.go`.
  - Delete the tests that exist only for the off state, such as a hub test that a gated route answers 404 while the experiment is off.
- [ ] **Abandonment:** delete the feature code, its UI, and its gated hub routes and handlers, together with their route metadata and authorization manifest entries.
- [ ] Keep public URLs working. If the flag-off state had its own URL, turn that URL into a redirect rather than a 404. Build the target from a fixed same-origin prefix (as `web/src/client/main.ts` does with `/terminals/`), never from a user-supplied URL. Make the redirect match any value the old route matched. A value that the new route then doesn't recognise can land on the 404 page; that is acceptable.
- [ ] If a deleted file or route used an icon that nothing else uses, remove that icon from `USED_ICONS` in `web/scripts/copy-shoelace-icons.mjs`. Keep icons that are still used.

## 4. Update the registry

- [ ] Delete the entry from `compiled` in `pkg/experiments/registry.go`.
- [ ] Add the name to `compiledRetired`, with a one-line comment that names the removal issue. Retired names are never reused.
- [ ] Remove the name from `DEFAULT_ON_FLAGS` in `feature-flags.ts` if it is there. The Go consistency test in `pkg/experiments/registry_test.go` fails if `DEFAULT_ON_FLAGS` and the registry disagree. A server-only (`LayerServer`-only) experiment is never in `DEFAULT_ON_FLAGS`.
- [ ] Update registry comments that used the name as an example.

## 5. Stored values: nothing to migrate

Don't write a migration:

- A stored admin override for a retired name is ignored on every read. It never appears in `GET /api/v1/experiments` or in the admin tab's unknown overrides.
- The next admin write prunes it.
- `TestRetiredExperimentOverrides_StoredAtStartup` in `pkg/hub` starts a hub with every name in `Registry.Retired()` stored, then checks that the stored names are ignored and then pruned. It covers each new retirement without changes.
- A stale `scion:feature:<name>` localStorage key in a browser is never read again, so it needs no cleanup.

## 6. Update the tests

- [ ] Delete unit and E2E tests that exist only for the other state. Also delete the "flag off" cases inside table-driven tests, and fields such as `hrefDisabled`.
- [ ] Remove the name from E2E init scripts that pin it through `window.__SCION_FEATURES__`, and from setup parameters such as `enabled` or `terminalWorkspace`. The suite for a graduated feature must pass with no experiment setup.
- [ ] Some tests reach a shared component only through a deleted adapter (for example a pane tested through a legacy page). Port the cases that test shared behaviour so they drive the component directly. Delete the cases that test only the adapter.
- [ ] Re-target tests that used the name as a sample experiment. They should use fixtures, never a production experiment:
  - **Hub:** the `testRegistry` fixtures, or `productionRegistryWithFixture` with `fixtureWebDefaultOn`.
  - **Web unit tests:** a fixture name such as `web.fixture_flag`.
  - **`pkg/config/opsettings`:** any pattern-valid sample name.
  - **E2E experiments spec (`web/e2e/experiments.spec.ts`):** another registered experiment.
- [ ] Remove the name from the perf-budget fixture, `web/e2e-perf/budgets/fixture.mjs` and `fixture-schema.json`. `pkg/hub/perf_budget_test.go` checks that schema against the hub's real `GET /api/v1/experiments` response.
- [ ] In `feature-flags.test.ts`, add a type-level test that the constant is no longer exported, and one test that the retired name resolves OFF. That test literal is the only allowed reference to the name outside `compiledRetired`.

## 7. Update the docs

- [ ] Remove the name from the [Experiments reference](/scion/reference/experiments/) (examples included) and from the flag table in [Web Configuration](/scion/reference/web-config/).
- [ ] Remove "enabled by the `<name>` flag" wording from feature pages.
- [ ] Add a status note to the experiment's design doc in `.design/`, without the experiment name. The existing design-doc text stays as written: it is design history.
- [ ] Leave history alone: `.design/` (design docs and `.design/project-log/`), `changelog/` and the published release notes keep their references.

## 8. Write the release note

The release note goes in the PR description; there is no release-note file. Say:

- Which experiment was retired, and whether it graduated or was abandoned.
- What changes for users. For a graduation, note that admins who had turned the experiment off get the feature back. For a default-off experiment, note which hubs may have had it on.
- That stored overrides are ignored and pruned, so no action is needed.
- Any URL that now redirects.

## 9. Verify

- [ ] This command, which leaves out the history paths, lists exactly two lines: the `compiledRetired` entry and the resolves-OFF web test.

  ```sh
  git grep -n '<name>' -- . ':!.design/' ':!.design/project-log/' ':!changelog/' ':!docs-site/src/content/docs/release-notes/'
  ```
- [ ] Go: `pkg/experiments`, `pkg/config/opsettings`, and the `pkg/hub` shards that hold the experiments tests (`hack/hubshard/hubshard.sh -locate <TestName>`).
- [ ] Web, in `web/`: the touched unit tests, `npm run typecheck`, `npm run lint`, `npm run format:check`, `npm run build` and `npm run test:e2e-perf`. `typecheck` and `lint` don't cover the E2E sources, so also run `npm run lint:e2e` and `npm run lint:e2e-perf`.
- [ ] Playwright: the suites whose files changed, and `web/e2e/experiments.spec.ts` (main config) when the experiments tab or its test changed.
- [ ] Docs: `npm run build` in `docs-site/`, which runs the link validator.
