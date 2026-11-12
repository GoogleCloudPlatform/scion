# Choices web cards, phase 1: single-decision vertical slice (ptone/scion#7673)

Base: upstream main `e8160a1`. Design: `design-7673.md` r4 (approved by
ptone, msg `6f8bd390-e2c5-4578-9e30-314d5a846bf7`), §8 item 1. Branch
`scion/chat-choices`, the one branch for the issue ("PR B").

## What was built

- **B0 (hub).** `choice.updated` is published on
  `project.<id>.chat.choice.updated` (threads) and
  `user.<uid>.chat.choice.updated` (each user of a DM), so the web's existing
  `project.<id>.chat.>` and every-scope `user.<uid>.chat.>` subscriptions
  receive it with no new subjects and unchanged authorization. The set-answer
  200 body is camelCase (`responseMessageId`, `batchId`). Docs pinning the old
  names updated. No shim (alpha). Own commit at the bottom of the branch.
- **Types.** `web/src/shared/types.ts` mirrors `pkg/hub/choices_view.go`
  (`DecisionView`, `AnswerView`, `ChoiceResponseView`,
  `ChoiceResponseSetView`, `ChoiceUpdatedEvent`); `Message` gains the five
  optional choice fields.
- **Routing.** `state.ts` maps both `…chat.choice.updated` subjects to a
  `choice-updated` event. Before this, an unknown `user.<uid>.chat.*` subject
  fell through to `chat-message-received` and scheduled a history backfill.
- **Thread.** `client/chat-choices.ts` holds the gate
  (`chatChoicesEnabled()`), the single answer call (`answerDecision`) and the
  merge rules. The thread merges the five history side maps on all three
  history readers (page, backfill, history window) and the live
  `chat.message` inline fields, and patches rows from `choice-updated` and
  from the card's own `choice-answered`. Guard: a terminal view is never
  replaced by a pending one. It is applied in the patch and in
  `mergeMessages`, so a history page fetched before the answer (for example a
  reconnect refetch) cannot reopen an answered card.
- **Card.** `<scion-chat-choices>` single mode: pending (recommended =
  `variant=primary outline`, screen-reader "(recommended)"), submitting,
  answered ("Answered by you|Name · time"), 409 `already_answered`/`replaced`
  (apply the returned view + toast), other errors (toast, card re-enabled).
  Enter on the focused card picks the recommended choice; Enter on a button
  is left to the button. Choices with descriptions stack as rows (§4.2).
  `scion-chat-message` renders it only for a `choice-prompt` with decisions
  and the browser flag on, and then shows `choiceIntro` alone and widens the
  bubble.

## Verification

- `npm run typecheck`, `lint`, `test` (12685 passed), `build`; `make ci`;
  hub shard 6.
- New Vitest: `client/chat-choices.test.ts`, `client/state.test.ts` (2 routes),
  `chat-choices.test.ts` (card states, 409s, error, Enter),
  `chat-thread-choices.test.ts` (double gate, history and live merge, SSE
  patch, out-of-order and stale-refetch guards). The stale-refetch test was
  checked to fail with the guard removed.
- End to end on a dev-auth workstation server: a real agent identity sent the
  prompt with `scion message --choices`, two tabs, click in one, the other
  updated without reload; flag off rendered the fallback. Evidence and exact
  commands: `/scion-volumes/scratchpad/projects/chat-choices/evidence/phase1/`.

## Deviations and left open

- Set prompts (2+ decisions) keep the fallback markdown until phase 2.
- Replaced rows show "Replaced by vN" without the jump link (phase 3);
  interpreted answers render as a plain answer (phase 3).
- 409 `batch_required` on the single endpoint shows a toast; the design's
  "re-fetch history" for that can't-happen case is not wired.
- Times use the app's 24-hour `formatInstant` style ("03:29"), not the mock's
  "3:42 PM".

## Process notes

- **Agent identity without a container runtime.** The sandbox has no docker,
  so agents cannot run. What worked: a docker stub on the test server's PATH
  that runs nothing, records `run`'s labels and reports the broker's
  `scion.bind_sources` binds as mounts on `inspect`. Add
  `SCION_HOST_NETWORK_BLOCK_POLICY=allow` on the broker, a `provisionOnly`
  agent and then `start`: the broker writes the hub-issued token to
  `<agent home>/.scion/scion-token`, and `scion message` runs as the agent
  with `HOME=<agent home> SCION_CLI_MODE=agent`. A failed start revokes the
  token. Run the test server under `env -i`, since an agent sandbox carries
  its own `SCION_*`. `hack/e2e/fakeagent` was found afterwards and may be
  shorter.
- Dev-auth browser sign-in is `POST /auth/dev-launch` plus the returned link,
  not automatic. A fresh workstation also needs `server.auth.display_name` or
  every page redirects to `/onboarding`.
- `beforeEach(() => mock.mockReset())` returns the mock, which Vitest then
  runs as teardown: use a block body.
- `pkill -f "<pattern>"` matches the invoking shell's own command line and
  kills it.
