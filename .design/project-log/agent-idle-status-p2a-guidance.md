# Agent status guidance: idle vs blocked vs task_completed

**Date:** 2026-11-14
**Branch:** scion/agent-idle-status-p2a
**Issue:** ptone/scion#7942 (phase 2a of ptone/scion#7826)

## Change

Rewrote the agent-facing status guidance around one rule:

- **blocked**: a specific expected event (a child agent, a reply, a scheduled message, your own
  background job). Signal it before ending a turn while your own background job or process runs,
  because suspending would kill it.
- **idle** (new): nothing is coming until new work arrives. A schedule hours away also permits idle.
- **task_completed**: the task the agent was created for is done. Ongoing roles (lead, owner,
  coordinator, reviewer pool) never complete; they go idle.
- Idle or completed agents may be suspended. If Scion suspends an agent this way, any message,
  mention, notification or scheduled message wakes it, so agents should not say blocked just to stay up.
- Dropped "to avoid being marked as stalled" as the reason to signal.

## Files

| File | Change |
|---|---|
| `resources/mandatory_boilerplate/agent-instructions-preamble.md` | Blocked rewritten, new Idle and Suspension sections, completed scoped by role, end-of-turn line |
| `pkg/config/embeds/templates/default/agents.md` | Same rule in the template's style |
| `resources/platform_skills/scion-scheduler/SKILL.md` | idle for "no further work" and for a far-off schedule; idle-suspended agents wake on any schedule without `--wake` |
| `resources/platform_skills/scion-messaging/SKILL.md` | idle after sending or answering with no further work |

## Notes

- No golden tests pin these texts. `resources/catalog_test.go` only checks that the preamble is non-empty.
- `sciontool status idle` comes in phase 1 (ptone/scion#7944). This PR must land after it.
- Left for phase 4 (docs): `docs-site/.../local/agent-lifecycle.md`, `hosted/user/scheduling.md`
  and `hosted/user/messaging.md` still describe blocked mainly as a way to avoid the stall detector.
- Not changed: the pi harness bridge (`harnesses/pi/`) matches `sciontool status ask_user|blocked`
  in shell text to suppress its automatic task_completed. It may need `idle` added, which is a code change
  that belongs with phase 1's status handling.
