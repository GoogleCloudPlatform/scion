# Choices PR 2.T: containerless real-hub agent fixture and E2E suite (ptone/scion#7776)

Base: upstream `main` (`bfdf9f143`). Part of epic `ptone/scion#7514`; unblocks
real-hub E2E coverage in PR 2B (`ptone/scion#7798`) and Phase 3
(`ptone/scion#7753`). Test-only PR — no production Go or web component changes.

## What changed

- Added `web/e2e/harness/agent.ts`: a reusable, containerless real-hub
  agent-sender fixture (`createTestAgent`, `mintAgentToken`,
  `deriveAgentSigningKey`, `canonicalAgentUserDMKey`, and
  `classifyChoiceResponseDelivery`):
  - Provisions a real stored agent row on a running `startHub(...)` test hub
    using `POST /api/v1/projects/{projectId}/agents` with `provisionOnly: true`
    against the co-located runtime broker (`runtimeBroker: true`,
    `testIdentities: true`), requiring zero Docker daemon or live agent
    containers.
  - Derives `agent_signing_key` from `E2E_SESSION_SECRET` matching
    `deriveSharedSigningKey(secret, "agent_signing_key")` in `pkg/hub/server.go`
    and signs an HS256 `X-Scion-Agent-Token` JWT matching
    `AgentTokenService.SignAgentToken` in `pkg/hub/agenttoken.go`.
  - Transitions the agent to `phase: "running", activity: "idle"` via
    `POST /api/v1/agents/{id}/status`, validating the minted credential up
    front and ensuring chat v2 treats the agent as reachable.
  - Authenticates as `AgentIdentity` on both `RouteAgentSelf` endpoints
    (`/api/v1/agents/{id}/outbound-message`, `/api/v1/agents/{id}/status`) and
    `RouteAgentToken` endpoints (`/api/v1/agent/*`, e.g.
    `GET /api/v1/agent/gcp-token` and PR 2B's
    `POST /api/v1/agent/decisions/interpreted`).
  - Exposes `agent.inspectChoiceResponses(...)` and
    `agent.waitForChoiceResponseDelivery(...)` to classify `choice-response`
    rows into `'attempted_delivery'` (`attemptedAgentDelivery === true`, where
    the hub dispatched to the broker and containerless delivery reported
    `broker delivery failed: ...`), `'logged_without_delivery'`
    (`attemptedAgentDelivery === false`, where the hub persisted the row
    without calling broker delivery, such as a matching confirmation of an
    interpreted answer), and `'agent_unreachable'`.
- Added `web/e2e/chat-choices/` (`playwright.config.ts`, `global-setup.ts`,
  `global-teardown.ts`, `state.ts`, `tsconfig.json`, `choices.pw.ts`) and the
  `npm run test:e2e:chat-choices` script in `web/package.json` (plus shared
  ESLint project registration in `web/e2e/tsconfig.eslint.json` and
  `web/eslint.config.mjs`):
  1. Verifies the containerless agent fixture authenticates on both
     `RouteAgentSelf` and `RouteAgentToken` routes.
  2. Single-decision prompt in a user DM renders `<scion-chat-choices>` live
     over SSE with zero page reload, transitions to `decided` (`Answered by
     you`) on click, renders `<scion-chat-choice-response>`, and settles as
     `attempted_delivery`.
  3. Multi-user group conversation updates User B's open browser tab live over
     SSE when User A answers (`Answered by Alice Choices`, read-only disabled
     buttons, compact `<scion-chat-choice-response>` line, zero page reload).
  4. Multi-decision set card (`answerMode: "set"`) tracks per-row selections
     (`2 remaining` -> `1 remaining` -> `0 remaining`), submits atomically via
     `Submit answers`, flips all rows to `decided` (`Submitted by you`),
     renders expandable `details[data-kind="set"]`, and settles as
     `attempted_delivery`.
  5. Replacement (`choicesUpdate: true`) flips an open v1 card live over SSE to
     `replaced` (`Replaced by v2`, `Jump to v2`) and renders the pending v2
     card live.
- Added `web/e2e/harness-selftest/agent-fixture.pw.ts` so `npm run
  test:e2e-support` exercises JWT signing, `provisionOnly` setup, outbound
  addressing normalization, and confirm-vs-override delivery classification in
  the fast CI web job without Go or a browser.
- Documented `web/e2e/harness/agent.ts` and `npm run test:e2e:chat-choices` in
  `web/e2e/harness/README.md`.

## How it was verified

- Built `./cmd/scion` (`go build -buildvcs=false -o build/scion ./cmd/scion`)
  and `web` (`npm run build`), then ran
  `CHROMIUM_EXECUTABLE=/usr/bin/chromium E2E_PREBUILT=1 npm run test:e2e:chat-choices`
  twice consecutively in a container without Docker: 5 passed, 0 skipped, 0
  failed on both runs (~36s each).
- Ran `npm run typecheck`, `tsc --noEmit -p e2e/chat-choices/tsconfig.json`,
  `npm run lint`, `npm run lint:e2e`, `npm test`, `npm run test:e2e-support`
  (53 passed), and `make ci` after cleaning untracked build artifacts.

## Learned / follow-ups

- Built-in `project-member` no longer carries `agent.message` permission; use
  `project-admin` (or `project-owner`) for test users who should be able to
  answer a `messageMode: 'project'` agent's choices, and `project-member` when
  testing blocked answerability (`You do not have permission to message this
  agent.`).
- Chat SSE streams subscribe to `user.<userId>.chat.>` and
  `project.<projectId>.chat.>` (not `user.<userId>.>`).
- On cloud-hosted sandboxes without Docker, pass
  `SCION_HOST_NETWORK_BLOCK_POLICY: 'allow'` in `startHub({ env })` alongside
  `SCION_IMAGE_REGISTRY` so the co-located runtime broker never refuses startup
  on detected cloud metadata hosts.
