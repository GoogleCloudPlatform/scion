# Optional count limits: the agent Edit page

**Date:** 2026-10-31
**Branch:** scion/dev-optval
**Design:** optional typed values for count and resource limits, section 6 (Edit page)

## Problem

A count limit (`max_turns`, `max_model_calls`) of `0` is now stored and means Unlimited, but the Edit page still treated a stored `0` as unset. It also said Unlimited on a count limit takes effect only at the next reincarnation. That was wrong once brokers advertise `unlimitedCountLimits`, because the next start applies it. Two hub editability flags had no web consumer: `unlimitedUnsupported` (count limits) and `subfieldClearUnsupported` (on `config.resources`).

## Solution

All changes are in `web/src/components/shared/agent-config-form.ts`, plus the two flags in `web/src/shared/types.ts`:

- **Stored 0 shows as Unlimited.** `storedUnlimited` covers `limit-count`. A stored `0` shows the Unlimited box ticked with Clear offered. Clear, or unticking, sends `null`; an untouched field sends nothing.
- **`unlimitedUnsupported`.** The Unlimited box is disabled and a fixed reason is shown (`UNLIMITED_UNSUPPORTED_REASON`). The reason covers both cases the hub flags: the broker lacks the capability, or the agent has no broker. A stored `0` stays shown as Unlimited, and Clear stays available.
  - The box stays disabled even when ticked, as the design specifies. Unticking would only be a clear, which Clear already does.
- **Reincarnation notes.** `keptByStart` is a clear only, so neither the default nor the held status says Unlimited waits for a reincarnation.
- **Resources removal note.** It shows only when `subfieldClearUnsupported === true`, with the held wording kept for a held edit.

## Review fix: held reason

A held count limit flagged `unlimitedUnsupported` (for example `max_turns` on a running agent) had a disabled Unlimited box with no reason, because only the default status showed the note. The note is now one helper, `unlimitedUnsupportedNote`, used by both the held and default statuses. Tests: a held `unlimitedUnsupported` field shows the disabled box and the reason. A held resources removal without `subfieldClearUnsupported` shows no note, which guards the flag check.

## Flag contract

Agreed with the hub side:
- Both flags are bare bools, `omitempty`, with no hub-sent reason; the web shows fixed text.
- `subfieldClearUnsupported` is carried only on `config.resources`. It is also set when the agent has no broker.
- The Edit page does not read the project-settings inherited hint (`agentEditPlaceholders` is static). A project `0` reaches it as the agent's own stored `0`, stamped at create.

## Notes

- An absent `subfieldClearUnsupported` means "capable broker" on a current hub and "unknown" on an older one. The web bundle ships in the hub binary, so this only mattered for merge order: this change merges after the hub change that adds the flag.
- Follow-up, not in this change: the agent detail page still shows "None" for a stored count limit of `0`.
