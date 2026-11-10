# Choices Phase 1, PR A part 2: hub send and answer paths, CLI (ptone/scion#7514)

Base: part 1 and part 2 rebased onto main at `19f7fc4b`, then upstream
main merged in for the open pull request. Design: native-chat choices v4
(signed off), section 10 items 4 to 6. PR B (web) builds on it. This
entry covers part 2 and the changes that followed its four reviews.

## What was built

- **Send path (item 4).** `OutboundMessageRequest` gains `choices` and
  `choices_update`. A send with choices needs `web.chat_choices` (400
  `experiment_disabled` when off; choices are never dropped), goes only to a
  user or a conversation (an `agent:` or broadcast recipient, or a
  conversation that routing maps to an agent, is 400), and carries no
  attachments. The hub normalizes the decisions with
  `messages.NormalizeDecisions` (field errors in the 400 details) and stores
  the prompt with `CreateChoicePrompt` in one transaction, which renders the
  markdown fallback as the body. The intro is the body after the agent
  mention rewrite, like every stored body. The response lists each decision
  as `{key, version, ref, row_id, replaced}`; a replaced decided version
  names its answer and who gave it. A reused key without `choices_update` is
  409 `choice_key_exists`; an all-digit key never issued to the agent is 400
  `decision_key_not_issued`; the pending cap is 409
  `too_many_pending_decisions`. A choice prompt counts against the outbound
  message rate limit, and it wakes no default agent and fans out no agent
  mention.
- **Hub-minted types.** `choice-prompt` and `choice-response` are refused
  (400 `hub_minted_type`) on every client path that takes a caller type.
- **Answer and get endpoints (item 5).** `POST /api/v1/decisions/{rowId}/answer`
  and `GET /api/v1/decisions/{rowId}`, behind `requireExperiment`, in the
  route metadata, the route authorization manifest and the operation
  catalog (`chat.decision.answer`, `chat.decision.read`). Answering needs
  the right to send in the prompt's conversation (for a DM, only its
  addressee); reading needs read access; a missing row, prompt or refused
  caller is 404; agents, visitors and user access tokens are 403. A true
  replay (the answer recorded under the same idempotency key, by the same
  user, with the same choice) is answered 200 first; the same key for any
  other answer is 409. Every other answer gets the state checks: a replaced
  version is 409 `replaced`, a directly answered one is 409
  `already_answered`, and over an interpreted answer only the recorded
  answerer may confirm or override (403 `not_recorded_answerer`). The label
  always comes from the stored row.
- **Store rules (`RecordAnswer`).** The store enforces the answer rules for
  any caller: one answer per provenance; a direct answer over an
  interpreted one only from the recorded user, as a confirm (same choice)
  or an override (another choice); every other answer is outcome `answer`.
  On Postgres the decision row is locked for the answer, and a repost of
  the key locks the key's rows, so an answer and a repost serialize; the
  pending-to-decided update must change exactly one row. A lost repost race
  is retried up to five times.
- **choice-response.** Stored from the answering user to the asking agent
  in the prompt's conversation, delivered with the hub line and the
  `choice_key`, `choice_version`, `choice_id`, `choice_provenance`
  metadata (reserved keys, stripped from client input). A confirmation is
  stored but not delivered; a gone asker leaves the row failed and the
  answer stands.
- **Events and history.** `choice.updated` goes to the project subject for
  a thread and to each DM user's subject for a DM. Chat history pages gain
  `messageDecisions`, `messageChoiceIntros` and `messageChoiceResponses`,
  read in batches (the decisions of the page's prompts, the versions that
  replaced them, the answers of its responses and their prompts). The chat
  data of a prompt or response is passed to the publisher explicitly, as
  a publish option, through the member fan-out.
- **Deletes.** A project delete removes its decisions, answers and the key
  counters of the agents deleted with it, in its transaction. A user delete
  clears the user id on their answers and deletes the user in one
  transaction; name and email snapshots stay.
- **Schema.** `decision_answers` gains an index on `answered_by_user_id`
  for the user delete.
- **CLI (item 6).** `scion message --choices`, `--choices-file` and
  `--update`, with the client-side refusals (agent and group recipients,
  `--in`/`--at`, `--attach`, `--artifact`, `--wake`, `--cc`, `--channel`,
  `--thread-id`). One predicate, agent mode and the agent name, both
  refuses a non-agent send and gates the ask_user side effect. Text output
  lists one line per decision whatever the send status; `--format json`
  prints the full send result. The ask_user summary strips markdown only
  where it marks.
- **agentstatus.** The status update behind `sciontool status` lives in
  `pkg/sciontool/agentstatus` so both binaries share it. Its local update is
  unix-only (`agentstatus_unix.go`); on other platforms it refuses and logs
  an unsupported-platform error and never fails the send, so `./cmd/scion`
  builds for Windows.
- **Decision text.** Choice descriptions are one line without control or
  format characters; questions may hold line breaks and tabs but no other
  control character.

## Decisions and their reasons

- Choices only on the outbound-message request: every agent send to a user
  or conversation goes through it.
- Who may answer: for a DM only the addressee; for a group conversation
  anyone the chat send check allows (first answer wins); agents never
  answer directly.
- A true replay is decided before the state checks, so a retry after the
  version was replaced or answered still gets its answer; any other use of
  a key is checked like a fresh answer.
- The store repeats the handler's answer rules so no caller can skip them.
  An agent_interpreted answer needs a version with no answer of either
  kind. A direct answer whose outcome does not fit the recorded answers
  gets its own error, which the endpoint maps to 409 with the current
  decision: it means the state changed after the endpoint read it. An
  interpreted answer with a wrong outcome stays invalid input.
- The decision routes are session-only in the catalog. Their refusal of
  any non-session user credential (a user access token, a federated
  user) is the session-only 403 with the recorded reason, so clients and
  the bearer disposition matrix can tell it from an authority denial.
  The handler checks the credential class itself, because the chat route
  gate also admits federated users.
- Orphan key counters of agents hard-deleted before a project delete are
  accepted (one small row per agent; agent IDs are not reused).
- The agentstatus refusal logs rather than returns an error, because a
  status update never fails its caller.
- The sender check needs both agent mode and the agent name: agent mode is
  what the side effect is for, and the name is the identity the prompt is
  sent as.

## Tests

- `pkg/hub/choices_test.go`: the send, refusal, key, rate limit and
  hub-minted cases; answer authorization (outsider 404, agent 403, token
  and visitor 403, reader refused, DM addressee only); recording and
  delivery; replay, key reuse 409, and a fresh key keeping every check;
  concurrent answers; replaced 409; `choice.updated` for a thread and for a
  DM (user subject only); a gone asker; a group prompt waking no agent; the
  intro mention rewrite; the replaced answer naming who decided; an
  interpreted answer landing after the state check (409 with the current
  decision, through a store wrapper that records it just before the
  write); history views.
- `pkg/store/entadapter`: concurrent reposts at two and three; the answer
  rules (outcome needs an interpreted answer; only the recorded answerer
  over it; no interpreted answer after a direct one); a failed user delete keeping the answer's user id; batch row
  reads; project and user deletes. Postgres-only tests hold an answer or a
  repost open in a transaction and check that the other side waits for the
  row lock and then sees the committed state; they run in the Postgres
  store job.
- `pkg/sciontool/agentstatus`: the ask_user update writes
  `waiting_for_input` and a following tool-end keeps it (unix).
- `pkg/messages`: control characters in descriptions and questions.
- `pkg/hubclient`: the choice fields on the wire.
- `cmd`: flag rules, choices file loading, the summary, the decision lines for a
  non-sent status, and the sends.

## Check results

- The rebase onto `19f7fc4b` had no conflicts; each file carries the same
  added and removed line counts on both bases. `go generate ./pkg/ent`
  changed nothing.
- On the rebased head: Windows builds of `./cmd/scion` (amd64, arm64),
  `go vet`, the scoped `golangci-lint` and `gofmt -l .` clean; tests of
  `pkg/messages`, `pkg/hubclient`, `pkg/sciontool/agentstatus`,
  `pkg/store`, `pkg/store/entadapter`, `pkg/hub/authzop` and `cmd`: ok.
- A hub shard run found the bearer disposition matrix probing the
  decision routes while their experiment is off; that led to the
  session-only refusal above and two pinned matrix exclusions.
- The merge of main had two conflicts: the experiments reference (main's
  entry kept, the choices entry after it) and the generated catalog
  report (regenerated by its staleness test). `go vet`, lint and
  `gofmt` over the packages the merge touched: clean.
- The final head runs `go generate`, vet and lint, the tests of every
  touched package and the located hub shards before landing.
- Gates outside this environment: `hack/winsec-guard.sh`; the Postgres
  store job (the race tests); the hub shards holding no touched test file;
  the sudo and setuid tests in `cmd/sciontool/commands`, which need a
  root-owned temp chain.

## Process notes

- Run the public-text checks (wording, tier-1, issue keywords) and
  `gofmt -l` on every commit before committing; one round had to be
  re-committed and one formatting change needed an extra commit.
- Guard tests that enumerate publish sites must list a new publish site;
  run a changed package's whole hub shard, not a filtered set.
- Path-set diffs that decide anything use `--no-renames`.
- Run every hub shard that holds a touched test file before review ends:
  a catalog-wide test (the bearer disposition matrix) can fail on a new
  route that no diff review shows.
- A generated report is regenerated by its own test, never edited by
  hand during a merge.

## Left open

- Phase 2 (agent decision endpoints, interpreted answers) and Phase 3.
- PR B (web cards, SSE patching).
