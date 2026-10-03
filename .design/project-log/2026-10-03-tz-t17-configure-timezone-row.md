# tz-refactor task 17: configure-page Timezone row (pin and unpin)

**Date:** 2026-10-03
**Branch:** scion/tz-t17
**Issue:** ptone/scion#2510 (part of ptone/scion#2457; design Option A, §3 A "Configure page")

## What changed (web only)

- `web/src/components/pages/agent-configure.ts` gains a **Timezone row** on the General tab. It shows
  the agent's container timezone and its source, with **Pin…** (the shared
  `scion-timezone-picker`, no "Auto" entry, validated with `isValidTimeZone`) and **Unpin**.
  Each writes the agent PATCH's top-level `explicitTimezone` (a zone name pins, `""` unpins) in its
  own request, separate from Save. The row then shows `resolvedTimezone` and `timezoneSource`
  from that response. A Save response updates the row the same way.
- **On load** the agent GET carries only `appliedConfig.explicitTimezone`/`explicitTimezoneLegacy`,
  so the row shows the pin (source explicit or legacy) or "Not pinned" with the resolution order.
  The resolved zone of an unpinned agent appears only after a PATCH.
- **Non-created phases.** The page still says the agent cannot be configured, but it renders the
  Timezone row under that notice, because the hub accepts `explicitTimezone` in any phase. In
  cloning/starting/running (the hub's live-container phases), or when the PATCH returns the hub's
  next-start warning, the row says the change applies on next start. agent-detail still links
  Configure only for created agents.
- **Env table.** `TZ` is filtered out on load, so an empty gathered `TZ` is never a "required" row.
  In `buildConfig` it is skipped on both the current and the loaded side, so a typed `TZ` row is
  never sent and a loaded `TZ` never counts as an env edit. The Environment tab says where `TZ` is managed.

## Source labels

explicit = "Pinned on this agent"; legacy = "Pinned (kept from an earlier TZ setting)"; user = "Your
TZ environment variable"; project/hub/broker = "Project/Hub/Broker TZ environment variable";
progeny = "Inherited TZ environment variable"; hub-default = "Hub default timezone"; none = "Not set
(container default)", with the value shown as UTC.

## Tests

`web/src/components/pages/agent-configure-timezone.test.ts` (21 cases): pin, unpin, a pin-then-unpin
round-trip through a mocked PATCH, every source label from the PATCH response, invalid zone, PATCH
failure, Save response, running/stopped agents, and the TZ env filter on load and save. The display
preference is set to a zone that differs from the browser zone. A mutation check (removing the
filters or changing the PATCH body) fails 8 cases. These ran under TZ=UTC, Asia/Tokyo and
Asia/Kathmandu with the neighbouring configure tests, the picker tests and the format scan.
`npm run typecheck` is clean. The configure page has no Playwright coverage, so none was added.

## Follow-ups

- A GET field for `resolvedTimezone`/`timezoneSource` would let the row show the resolved zone on
  load (requested as a hub follow-up).
- No Configure entry point for non-created agents. The row is reachable there only by URL.
