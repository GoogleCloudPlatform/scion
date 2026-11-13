# Choices web cards, phases 4 and 5: answerability and response lines (ptone/scion#7673)

Base: `scion/chat-choices` at `100708a` (phase 1). Design: `design-7673.md`
r4 (approved by ptone, msg `6f8bd390-e2c5-4578-9e30-314d5a846bf7`), §4.3,
§4.5, §4.6, §4.7, AC 6 and 7. Branch `scion/chat-choices-p45`, which
choices-em integrates. Everything stays behind `web.chat_choices`.

## What was built

- **Answerability (phase 4).** `client/chat-choices.ts` defines
  `AskerAnswerability {state: ok|blocked|gone|unknown, reason, askerName,
  readOnly}`.
  - `fetchAskerAnswerability` reads `GET /api/v1/agents/{id}`. If
    `_messageability.canMessage=false`, the result is blocked with the
    `getDenialMessage` copy. 404 means gone. Any other failure is
    `unknown`, which stays answerable.
  - `answerabilityFromDenial` maps a 403: `message_denied` uses
    `details.reason`, `chat_post_denied` means read-only, and any other 403
    gives "You can't answer this decision." `answerDecision` now returns
    `details.reason`.
- **Thread.** The thread reads each distinct `askerAgentId` once per thread
  load. The cache is guarded by a generation counter and reset on
  conversation switch. In a read-only thread (`canPost=false`, not a DM)
  every card is blocked and no agent read is made. When a card emits
  `choice-answer-denied`:
  - `chat_post_denied` sets `canPost=false`, and the composer gives way to
    the read-only notice.
  - `message_denied` blocks every prompt from that asker.
  - A generic 403 stays on that one card.
  - A lookup result never overrides a 403 that already blocked the asker.
- **Card.** `<scion-chat-choices>` has a new `answerability` input.
  - Blocked: every choice is disabled (`aria-disabled`), the Enter hint is
    hidden, and the reason shows inline with a `lock` icon (`eye` when
    read-only). The card group's `aria-describedby` points at the reason.
  - A 403 on click is authoritative: it blocks the card, shows a toast and
    emits `choice-answer-denied`.
  - Gone asker: the card stays answerable and shows the muted note with
    `info-circle`.
  - Hooks for set mode (phase 2): `answerBlocked` (disable Submit),
    `renderAnswerBlocked()` (place the reason once under the rows),
    `renderAskerGoneNote()`, `effectiveAnswerability`. Single mode puts the
    reason in the row in place of "Awaiting your answer".
- **Response lines (phase 5).** New `<scion-chat-choice-response>`, a
  sibling file. `scion-chat-message` renders it instead of the bubble for a
  `choice-response` that carries `choiceResponse`/`choiceResponseSet`, with
  the browser flag on. The line sits in the answerer's lane: right with
  "you" for the viewer, left with the sender's name otherwise.
  - Single: check, "X chose L for KEY question · time · Jump".
    `agent_interpreted` uses `robot` and "recorded from X's reply by
    <agent>".
  - Set: native `<details>`, "X answered N decisions · time · Show answers
    ▸". It expands to `KEY label (recommended)` (from `matchedDefault`) in
    position order, then "Jump to question".
  - `dispatchState=failed` adds "not delivered (agent gone)".
  - Jump emits `scroll-to-message`, which the thread already sends to
    `scrollToMessageById`.
- **Touch fix.** The thread's tap-to-open-menu handler on devices without
  hover now skips `summary`. Before, expanding a set line on a phone opened
  the message action sheet.

## Verification

- `npm run typecheck`, `lint` (0 errors), `test` (519 files, 12726 passed
  before the touch fix; the chat suites were re-run after it), `build`,
  and `make ci`.
- New Vitest files:
  - `client/chat-choices-answerability.test.ts`: lookup states and 403
    mapping.
  - `chat-choices-answerability.test.ts`: blocked, read-only, each 403
    code, gone.
  - `chat-choice-response.test.ts`: single, interpreted, failed, set,
    jump.
  - `chat-thread-choices-answerability.test.ts`: one read per asker, a
    blocked or gone asker, a read-only thread with no read, a 403 spreading
    to other cards and the composer, lane rendering, jump, flag off, touch
    tap.
- I removed the thread wiring, and the thread tests failed as expected.
- Ad-hoc screenshots (light and dark, desktop 1440 and 390px) from the
  mocked chat page were compared with `mocks/png/10-disabled.png` and
  `12-responses.png`. Evidence:
  `/scion-volumes/scratchpad/projects/chat-choices/evidence/phase4-5/`.

## Deviations and left open

- The blocked reason follows the design text ("You can't answer
  **agent-x**: <denial copy>"), not the mock's "**You can't answer these
  decisions.** <copy>". The two differ. A set card can switch the lead
  wording in `renderAnswerBlocked()` if choices-em prefers the mock.
- "not delivered (agent gone)" is muted, as the design says. The mock
  shows it in danger red.
- Interpreted responses: `ChoiceResponseView` does not name the
  interpreting agent, so the line uses the response's agent recipient
  (`routedTo`), which is the asker. No live route produces interpreted
  answers yet (design §2).
- Response lines drop the message's star/actions affordances (no bubble).
  A choice-response flag-off bubble still offers Edit/Delete as before;
  this is not new.
- The committed mocked-API Playwright screenshots are phase 6. The scratch
  spec used here is under the evidence `scripts/` directory.
