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
- **Non-created phases.** The hub accepts `explicitTimezone` in any phase. So outside `created` the
  page shows a neutral notice ("only its timezone can be changed here. This page edits other
  settings only while an agent is in "created" phase.") and the Timezone row, with no other form
  fields and no Save or Start button, and a back link to the agent. The notice is scoped to this
  page because the hub also accepts config edits for stopped agents. In every non-created phase, or when the PATCH
  returns the hub's next-start warning, the row says "A timezone change applies on the agent's next
  start."
- **Entry point.** `agent-detail.ts` now shows the Configure button in every phase when the caller
  has the agent `update` capability, and still in `created` as before. This makes the row reachable
  for existing (often legacy-pinned) agents, which the task 16 release note relies on.
- **No overlapping PATCHes.** Pin…, Unpin, the picker and its Pin/Cancel actions are disabled while
  the main form is saving or starting. Save, Start, Back and Delete are disabled while a pin or unpin
  is in flight. `handleSave`, `handleStart` and the pin/unpin PATCH also return early if another
  PATCH is in flight, so the timezone PATCH and the config PATCH never race. Tests cover both
  directions; dropping either half, or the handler guards, fails 2 to 4 cases.
- **Env table.** `TZ` is filtered out on load, so an empty gathered `TZ` is never a "required" row.
  In `buildConfig` it is skipped on both the current and the loaded side, so a typed `TZ` row is
  never sent and a loaded `TZ` never counts as an env edit. The Environment tab says where `TZ` is managed.

## Source labels

explicit = "Pinned on this agent"; legacy = "Pinned (kept from an earlier TZ setting)"; user = "Your
TZ environment variable"; project/hub/broker = "Project/Hub/Broker TZ environment variable";
progeny = "Inherited TZ environment variable"; hub-default = "Hub default timezone"; none = "Not set
(container default)", with the value shown as UTC.

## Tests

`web/src/components/pages/agent-configure-timezone.test.ts`: pin, unpin, a pin-then-unpin
round-trip through a mocked PATCH, every source label from the PATCH response, invalid zone, PATCH
failure, Save response, a response without the resolved fields (pin and unpin), a running agent
(pin, resolved value and source), a legacy-pinned running agent (unpin), the next-start hint in
stopped/starting/suspended/error, and the TZ env filter on load and save. For running, stopped,
starting, suspended and error it also asserts the view has no form fields and no button other than
Pin…/Unpin (adding a Save button there fails 5 cases).
`agent-detail-header.test.ts` checks that Configure shows for running/stopped/suspended/error agents
with `update` capability and is hidden without it (reverting the condition fails 5 cases). The display
preference is set to a zone that differs from the browser zone. A mutation check (removing the
filters or changing the PATCH body) fails 8 cases. These ran under TZ=UTC, Asia/Tokyo and
Asia/Kathmandu with the neighbouring configure tests, the picker tests and the format scan.
`npm run typecheck` is clean. The configure page has no Playwright coverage, so none was added.

## Follow-ups

- A GET field for `resolvedTimezone`/`timezoneSource` would let the row show the resolved zone on
  load. Tracked as ptone/scion#2767.
