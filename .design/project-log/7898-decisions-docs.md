# Docs: `scion decisions` on the docs site (ptone/scion#7898)

**Date:** 2026-11-14
**Branch:** scion/choices-skills-docs
**Context:** phase 2 of the ptone/scion#7830 design (§3.6 "Docs-site gap", §7). Prose only.

## What changed

- `docs-site/src/content/docs/reference/cli.md`: new `scion decisions` section after
  `scion conversation group`: `list [--pending|--decided|--all]`, `show <key>`,
  `record <key> <choice-id> --from <message-id>` and the batch `<key>=<choice-id>...` form,
  `--json`, agent-only, gated on `web.chat_choices`, the Hub's refusal codes for `record`.
- `docs-site/src/content/docs/hosted/user/messaging.md`: two bullets in "Multiple-Choice
  Decisions (Experimental)": how agents review decisions, and how they record an interpreted
  answer from a free-text reply (provenance, confirm/override, batch atomicity).

## Sources checked

- `cmd/decisions.go`, `cmd/decisions_list.go`, `cmd/decisions_show.go`,
  `cmd/decisions_record.go` (flags, help text, output, error mapping).
- `cmd/cli_mode.go` (`agentAllowed`) and `cmd/platform_gate.go` (Windows) for availability.
- `pkg/hub/choices_agent.go` (`handleAgentDecisionInterpreted`: source-message checks, error
  codes, the posted `choice-response` line, no delivery back to the agent) and
  `pkg/store/entadapter/decision_store.go` (exact choice-ID match, confirm/override outcomes).

## Findings worth knowing

- `record` must use the choice **ID**; labels are not matched (`400 invalid_choice`).
- The CLI sends a fresh idempotency key per call, so re-running a successful `record` fails
  with `409 already_answered` rather than replaying.
- `show` marks the decision's `default` choice `(default)`. The docs stay silent on how the
  UI preselects a recommended choice, since ptone/scion#7842 is changing that.
- The docs build needs `d2` (v0.9.0, as CI pins); it is not in the agent image. A release
  tarball in `/tmp` was enough for `npm run build`.
