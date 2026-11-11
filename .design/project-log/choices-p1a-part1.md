# Choices Phase 1, PR A part 1: experiment, decision schema, storage (ptone/scion#7514)

Base: `17ed440f`. Design: native-chat choices v4 (signed off), section 10
items 1 to 3. Branch `nc-choices-p1a-wip`. Part 2 (hub send and answer
paths, SSE, history, CLI) and PR B (web) build on it.

## What was built

- **Experiment and glossary.** `web.chat_choices` in
  `pkg/experiments/registry.go` (LayerWeb and LayerServer, alpha, default
  off, owner native-chat, issue `ptone/scion#7514`, review by 2027-02-05),
  with a pinning test, a bullet in the experiments reference, and the
  decision, question, choice, answer, choice prompt and decision log terms
  in `GLOSSARY.md` (Messaging).
- **Message types and schema** (`pkg/messages/types.go`, `choices.go`).
  `choice-prompt` and `choice-response` join the closed enum.
  `IsHubMintedType` names them for the client-path refusal part 2 wires in.
  `DecisionInput`/`ChoiceInput` decode the `--choices` JSON (a choice is a
  string or an object). Unknown and reserved fields (`multiple`,
  `allow_other`, `expires_in`) do not fail decoding: they are kept and
  reported with their field path. `NormalizeDecisions` returns the canonical
  `Decision` form or a `ChoiceErrors` list (field path plus code) covering
  every 4.2 and 4.3 rule: key shape and the non-digit rule (all-digit keys
  pass only in the shape of a hub number; issuance is the store's check),
  case-insensitive key uniqueness, choice id shape and positional default
  ids, 2 to 10 choices, label (at most 100 runes, one line of plain text,
  unique case-insensitively), description (300), question (2000), default by
  id then label, at most 10 decisions, and the 32 KB cap on the JSON of the
  normalized decisions.
- **Markdown fallback.** `RenderChoiceFallback(intro, decisions, answerURL)`
  returns the prompt's `msg` and the intro length (UTF-8 bytes up to the
  fallback block). Keys, choice ids and labels are written as plain text;
  questions and descriptions pass through as markdown. `@everyone`, `@here`,
  `@channel`, `<!...>` and `<@...>` are broken with a zero-width space (an
  email address such as `ops@here.example` is left alone). The intro (the
  agent's body) is not changed. One difference from the 4.6 mock: only the
  key is bold (`**go-live.** question`), because a markdown question inside a
  bold span breaks the span. `ChoiceIntro(msg, introLen)` recovers the intro
  and returns "" for an offset that does not fit.
- **Storage.** Ent schemas `decision` (`decisions`), `decision_answer`
  (`decision_answers`) and `decision_key_counter` (`decision_key_counters`)
  as in 4.5, additive (no `DowngradeFloor` change), no foreign keys, so
  answers and decisions survive a purged message. `store.DecisionStore` is
  embedded in `store.Store` and implemented on `entadapter.CompositeStore`
  (each method writes a message and decision rows together):
  `CreateChoicePrompt`, `GetDecision`, `GetDecisionsByMessageIDs`,
  `RecordAnswer`. The Phase 2/3 list methods are not here.

## Schema field decisions and their reasons

These fields are not named in the design; the design's architect confirmed
each one.

- `decision.intro_length` (int, default 0, position 0 row only, UTF-8
  bytes): the byte offset in `msg` where the fallback block starts, which is
  always a character boundary because the hub builds `msg` by
  concatenation. It refines 4.6: the web never slices `msg` by this offset
  (JavaScript strings are UTF-16, so a byte offset would cut a non-ASCII
  intro in the wrong place); the hub slices it (`messages.ChoiceIntro`) and
  part 2 serves the intro text in the history and SSE view (for example
  `choiceIntro`). A test with emoji, CJK and accented intros pins it.
- `decision_answer.idempotency_key` (nillable, at most 128, unique
  `(decision_row_id, idempotency_key)`; NULLs never collide, so keyless
  answers do not either). A replay needs the same key, choice and answerer,
  so one user's result is never handed to another under a reused key; the
  same key with another choice or another user is a conflict.
- RecordAnswer sets `decided` and `decided_at` only on the pending to
  decided transition, so a later direct answer after an interpreted one
  leaves `decided_at` alone (the effective answer is derived); a replaced row
  answers `ErrDecisionReplaced`; the store enforces only unique
  `(decision_row_id, provenance)`, and part 2 owns the confirm and override
  rules.
- The 50-pending cap counts after the prompt's own replacements, so an
  update of a pending key counts once; it is a soft cap (two concurrent sends
  may overshoot by a few), so it takes no extra lock.

## Store behaviour

- `CreateChoicePrompt` runs in its own transaction (or joins the caller's,
  without retry). Hub numbers come from `decision_key_counters.next`: the
  counter row update reserves them, so concurrent prompts of one agent take
  disjoint numbers, and a refused prompt rolls its reservation back. An
  all-digit key at or above the numbers reserved for this prompt is refused
  (`DecisionKeyNotIssuedError`). Without `Update` any existing key refuses the
  whole prompt (`ChoiceKeyExistsError`, hint text
  `go-live already exists (v1, decided by alice: yes); ...; pass --update to
  replace it, or use a new id`). With `Update` the active version is marked
  replaced (conditionally on not being replaced already) and version n+1 is
  inserted; a decided version keeps its answer row. A lost race (a unique
  violation or a version replaced under us) retries once.
- `RecordAnswer` copies key, version, question and label from the row,
  inserts the `choice-response` message and the answer in one transaction,
  and decides a pending row. A replay is checked before the replaced check,
  so a retry after a repost still gets its answer. The loser of two
  concurrent answers gets `ErrDecisionAnswerConflict`.

## Tests

- `pkg/messages/choices_test.go`: every validation rule (field and code),
  limits at the boundary, the 32 KB cap, unknown and reserved fields,
  decode errors, the design example in canonical form, the fallback output,
  mention neutralizing and label escaping, multi-byte intro offsets.
- `pkg/store/entadapter/decision_store_test.go`: key assignment, the counter
  never reusing numbers (replace, other agent, refused prompt), unissued
  numbers, the rejection hint, replace of pending and decided versions,
  the pending cap, concurrent `--update` reposts (consecutive versions, no
  error), concurrent hub numbers, concurrent answers (one winner),
  idempotent replay, rows surviving deletion of the prompt and response
  messages, joining a caller transaction.
- `pkg/experiments`: `TestChatChoices_RegisteredDefaultOff`.
- Runs (compile windows on the community host, private Go cache pair):
  window 1A: `go generate ./pkg/ent` and `go build -buildvcs=false ./...`, both
  exit 0; window 1B: `go vet` on ./pkg/messages, ./pkg/experiments,
  ./pkg/store, ./pkg/store/entadapter and ./pkg/ent/..., exit 0, and one
  `go test` over ./pkg/messages, ./pkg/experiments and
  ./pkg/store/entadapter, all ok. No pkg/hub file is touched, so no hub
  shard was run. fmt-check, ent-check, the ./pkg/store and ./pkg/ent tests
  and the scoped golangci-lint (windows 2A and 2B) follow in a later entry.

## Left for part 2

- Wire `messages.IsHubMintedType` into every client send path. Part 1 adds
  the two types to `validTypes`, so until part 2 lands a client could set
  them on an ordinary send; PR A must not land without part 2.
- Map the two types in `pkg/messaging/envelope_compat.go` (`MapLegacyType`
  falls back to text/inform with a warning).
- `CreateMessage` inside the transaction has no LISTEN/NOTIFY publisher: the
  send path must publish the stored prompt and response after commit (the
  `StoredMessageID` route).
- Clear `decision_answers.answered_by_user_id` on user deletion (name and
  email snapshots stay, 5.8), and name the retained data in the user
  deletion doc.
- Project deletion does not touch the decision tables; decide whether it
  should.
