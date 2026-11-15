# Conduit designs

Design records for Conduit, the agent and broker connection layer, that are kept in the repository. The main Conduit design (v2.21, including the §3.7 control-handler rules) is still kept outside the repo, in the project scratchpad archive. Amendments that ship code are committed here.

| File | What | Status |
|---|---|---|
| `v2.22-deliver-message.md` | The `deliver-message` control route: message delivery over the agent's Conduit session (ptone/scion#7353). Amends §3.7 for keyed idempotency. | FROZEN 2026-11-14 |
| `exec-control-routes.md` | The `exec-start` and `exec-status` control routes for exec-only schedules (ptone/scion#7882, design D9 = b'): the one bounded exception to "no generic exec over conduit". | DRAFT for owner review (Q5) |
