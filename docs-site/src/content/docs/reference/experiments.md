---
title: Experiments
description: How hub-wide experiments (feature flags) work, for admins who toggle them and for developers who launch, change, and retire one.
---

An **experiment** is an unfinished or risky feature that ships in the binary, is off or on per hub, and is controlled by admins from **Admin → Server Config → Experiments**. Each experiment is declared once in code with a name, a description, a default, and the layers it gates (the web UI, the hub server, or both). From that tab, admins can enable or disable any registered experiment for every user of the hub; the change takes effect on the next page load.

## For admins

The Experiments tab lists every experiment this hub version knows about: its title, description, stage badge, layer badges (UI, Server, or both), its default, and a switch for the current effective value. Managing experiments needs **two** permissions: `hub.config.read` to open the Server Config page at all, and `hub.experiments.update` to view and change the list on the Experiments tab itself. An admin who holds only `hub.config.read` sees the tab but gets a message asking for `hub.experiments.update`; granting roles should account for both.

- **Toggling an experiment** stores an explicit override for that experiment and applies it immediately — there is no separate save step. The row then shows "· overridden", even if the new value matches the default. "Reset to default" (shown only when an override is set) removes the override, so the experiment follows the registry default again.
- **Default** means the value used when no admin has set an override for that experiment.
- **Next-page-load semantics.** Changes apply to all users of this hub. Users see them the next time they load or refresh the page — there is no live push to already-open tabs.
- **Overrides for experiments unknown to this hub version are kept.** The hub stores overrides by name, and different hub versions may know different experiments. If this hub doesn't recognize a stored name, it leaves that override untouched (so a rolling deploy or a rollback never silently discards a kill switch) and lists it on the tab as a one-line note naming each one. Remove a stale entry on its own, without resetting every experiment, with `PUT /api/v1/admin/experiments` and body `{"overrides":{"<name>":null},"expected_revision":<revision>}`, using the `revision` from `GET /api/v1/admin/experiments`; a stale revision returns 409, so re-read and retry.
- **The admin value applies on every page load where the experiments request succeeds.** If that request fails for a given page load (a transient error, a mixed-version replica during a rollout, an expired session), the browser falls back to its localStorage override, then to the compiled default, for that page load only; the next successful load restores the admin value.
- **If the stored experiment settings become unreadable:** every server-layer experiment turns off (regardless of its default or any override), every web-only experiment returns to its registry default (so a disabled default-on UI experiment can reappear until the row is reset), an error is logged on the hub, and the tab shows a banner with a **"Reset all to defaults"** button that clears every stored override, including overrides for experiments not known to this hub version.

## For developers: launching an experiment

1. Open or link a tracking issue.
2. Add an entry to `pkg/experiments/registry.go` with every field set. `Default: false` unless there is a deliberate decision otherwise. Set `ReviewBy` about 90 days out.
3. Web gates use `isFeatureEnabled(CONST)`, with the constant exported from `feature-flags.ts`. Server gates use `requireExperiment` or `s.experimentEnabled`.
4. **The hub decides hub behaviour.** Browser-side values can be edited by the user, so a web gate is presentation only. If the experiment changes what the hub does, register it with `LayerServer` and check it in the hub with `requireExperiment` or `s.experimentEnabled`; the UI gate only mirrors that check. PR reviewers check this, because no test can detect it automatically. `GET /api/v1/experiments` carries only experiments with `LayerWeb`, so a UI gate that mirrors a hub check only works if the experiment is registered with **both** `LayerWeb` and `LayerServer`.
5. Keep both paths working and tested (unit tests for flag on and flag off). E2E tests pin the flag through `window.__SCION_FEATURES__`.
6. If `Default: true` and the experiment has `LayerWeb`, add the name to `DEFAULT_ON_FLAGS` (server-only names must not be listed). The consistency test enforces this.

## Changing the default

Changing an experiment's default needs a code change to the registry, in a PR that references the tracking issue, and, for an experiment with `LayerWeb`, an update to `DEFAULT_ON_FLAGS` to match (add the name when the default becomes true, remove it when it becomes false); the consistency test enforces this. A server-only (`LayerServer`-only) experiment is never listed in `DEFAULT_ON_FLAGS`. Admin overrides are kept across the change — an admin who already set an explicit value for that experiment is unaffected by a default flip.

## Retiring an experiment

Retiring an experiment is either a **graduation** (the feature becomes permanent) or an **abandonment**:

- **Graduate:** delete the flag-off code path and the `isFeatureEnabled` calls, delete the registry entry, add the name to `compiledRetired`, and remove it from `DEFAULT_ON_FLAGS`.
- **Abandon:** delete the feature code, delete the registry entry, add the name to `compiledRetired`, and remove it from `DEFAULT_ON_FLAGS` if it was default-on.

In both cases, add a web test asserting that the retired name resolves OFF by default. Stored overrides for a retired name are ignored on read and pruned automatically on the next admin write. Retired names are never reused. Mention the retirement in release notes; if the experiment had `Default: false`, note which hubs may have had it enabled.

## Precedence and localStorage

The web client resolves a flag in this order, highest first:

| # | Source | Present in production? | Purpose |
|---|---|---|---|
| 1 | Values in `window.__SCION_FEATURES__` before server flags are first applied ("pinned") | Only `setFeatureFlag('web.native_chat*')`, which is never an experiment name; otherwise E2E init scripts | Deterministic tests |
| 2 | Server value from `GET /api/v1/experiments` (admin override, else the registry default) | Yes, signed-in users only | Hub-wide control |
| 3 | localStorage `scion:feature:<name>` | Yes, for unregistered names, failed fetches, and signed-out loads (set only through devtools) | Unregistered or in-development flags; fallback when the fetch fails |
| 4 | Compiled `DEFAULT_ON_FLAGS` | Yes | Fetch failure |

The map of web-layer experiments is only served to signed-in users, from `GET /api/v1/experiments`, with compiled defaults applying when that request fails. **Devtools localStorage overrides no longer apply to registered experiments** — for example, an existing `web.terminal_workspace` opt-out set through devtools — except on page loads where the experiments request fails. The server value wins so that an admin's hub-wide choice is a dependable control, not something a single browser can silently override.

## Review cadence

Each registry entry has a `ReviewBy` date. Once it passes, the Experiments tab shows a "Review overdue" warning on that row. The owner either extends `ReviewBy` with a reason in a PR, or retires the experiment. There is no CI check that fails a build after the date passes — review is a documented process, not a time bomb.
