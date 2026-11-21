---
experiment: web.decisions
---

# Asking users to decide: decision prompts

A **decision prompt** (`decision-prompt`) is a message that carries one or more
keyed decisions. Each decision has a question and 1 to 10 choices. In web chat
the user answers with a click, and the answer comes back to you as a
`decision-response` message. Other surfaces show a Markdown copy of the same
questions and options.

## When to use one

Use a decision prompt when the user should pick between a few concrete options
— or confirm/approve a single action — and you can act on the answer directly:
approve or hold a step, pick one of two designs, choose a target. Keep prose
for open questions, for options you cannot list in a short label, or when you
need a discussion rather than a pick.

- **Single-choice confirmation:** a decision with **1 choice** (such as
  `"Approved"` or `"Confirmed"`) renders as a single primary button and records
  the sign-off in the decision log. Use it when you need a person's explicit
  confirmation or approval before proceeding (for example approving a plan,
  deploy, or destructive action) and there is no meaningful second button — the
  user can click to approve, dismiss it, or reply in words if they want
  changes. Use plain prose instead for FYI updates or open-ended feedback that
  does not need recorded sign-off.
- Send it to a user or a conversation: `@<email>`, `user:<email>`, `conv:<id>` or
  `#<thread>`. Decision prompts cannot go to an agent or a `group[...]`.
- Only an agent can send one (agent mode, inside an agent container).
- The message body is optional: it becomes the intro shown above the decisions.
  Put context and your recommendation there.

## One decision or a set

- **One decision:** the user answers it on its own.
- **A set:** one prompt with 2 to 10 decisions. The user picks an answer for
  every open decision and then submits them together; there is no partial
  submit. Use a set for questions that belong together and that you will act on
  as a group. Send separate prompts for decisions that are independent.

## Keep questions and choices short

Keep decision cards scannable so the user can answer at a glance:

- **Question:** one short sentence. Do not pack build SHAs, multi-item scope
  lists, links, or caveats into `question`.
- **Choice labels:** a few words (for example `"Approve window"` / `"Hold"`, or
  `"Staging"` / `"Canary"`). Use `description` only for a brief one-line
  trade-off when the label alone is not enough.
- **Message body:** put the briefing — build details, scope lists, links,
  caveats, and your recommendation — in the message body (the intro above the
  decisions), where it renders as full Markdown.

The CLI emits a soft warning on stderr (without blocking the send) when a
`question` exceeds **200 characters** or a choice `label` exceeds **40
characters**.

**Bad — briefing crammed into the question and choice labels:**

```json
[
  {
    "id": "deploy-window",
    "question": "Release build abc1234 (PR #410 auth migration, PR #412 cache TTL fix, PR #415 metrics cleanup; see https://example.com/run/987) is green on staging, though rollback requires re-seeding the cache. Should we open the 14:00 UTC production deploy window now or hold until tomorrow?",
    "choices": [
      "Approve the 14:00 UTC production deploy window for build abc1234 now",
      "Hold the release until tomorrow's window after the cache-warming check"
    ]
  }
]
```

**Good — context in the message body, short question and choice labels:**

```json
[
  {
    "id": "deploy-window",
    "question": "Open the 14:00 UTC production deploy window?",
    "choices": [
      {"id": "approve", "label": "Approve window", "description": "Deploy build abc1234 at 14:00 UTC"},
      {"id": "hold", "label": "Hold", "description": "Wait for tomorrow's window"}
    ],
    "default": "approve"
  }
]
```

```bash
scion message "conv:<conversation-id>" --decisions-file <file> - <<'EOF_MESSAGE'
Build `abc1234` passed staging ([CI run](https://example.com/run/987)) and includes:
- Auth migration (PR #410)
- Cache TTL fix (PR #412)
- Metrics cleanup (PR #415)

**Caveat:** rollback requires re-seeding the cache. I recommend approving the 14:00 UTC window.
EOF_MESSAGE
```

## Send it from a file

Write the decisions to a JSON file and pass `--decisions-file`. This avoids
shell quoting problems; `--decisions '<json>'` takes the same JSON inline.

```json
[
  {
    "id": "deploy-target",
    "question": "Where should the **release candidate** go first?",
    "choices": [
      {"id": "staging", "label": "Staging", "description": "Full test suite, about 20 minutes"},
      {"id": "canary", "label": "Canary", "description": "5% of production traffic"}
    ],
    "default": "staging"
  },
  {
    "question": "Notify the release channel?",
    "choices": ["Yes", "No"]
  }
]
```

```bash
scion message "conv:<conversation-id>" --decisions-file <file> - <<'EOF_MESSAGE'
The release candidate passed CI. I recommend **staging** first.
EOF_MESSAGE
```

`--decisions-file -` reads the JSON from stdin; then the body cannot also come
from stdin, so use a positional body or `--body-file <path>`.

The JSON is an array of decisions:

| Field | Meaning |
|---|---|
| `id` | Optional decision key. Use a short key you will recognize, such as `deploy-target`: `[A-Za-z0-9_-]`, at most 32 characters, with at least one non-digit. Leave it out and the Hub assigns your next number (`1`, `2`, ...). Keys compare case-insensitively. |
| `question` | Required. Markdown, at most 2000 characters (the CLI warns above 200 characters; keep it to one short sentence and put context in the message body). |
| `choices` | Required, 1 to 10. Each is a string label, or an object with `label` (one line, at most 100 characters; the CLI warns above 40 characters — keep labels to a few words), optional `id` (`[A-Za-z0-9_-]`, at most 32 characters) and optional `description` (one line, at most 300 characters). A choice without an `id` gets its position (`1`, `2`, ...). Choice ids and labels must each be unique within the decision, compared case-insensitively. |
| `default` | Optional. Marks your recommended choice, by its `id` or `label` (cannot name Other). |
| `allow_other` | Optional. Lets the user type a short free-form answer; use sparingly (see [Let the user answer in their own words](#let-the-user-answer-in-their-own-words-allow_other)). |

Other fields are refused. The whole set is at most 32 KiB of JSON.

On success the CLI prints one line per decision, for example
`deploy-target v1 (new)` and `1 v1 (new)`. Keep the keys: you need them to read
or replace the decisions. With `--format json` you get the Hub's full send
result.

After a successful send the CLI marks you as waiting for input, the same as
`sciontool status ask_user`, with the first question as the summary.

You cannot combine decisions with `--attach`, `--artifact`, `--wake`, `--in`,
`--at`, `--cc`, `--channel` or `--thread-id`.

## Let the user answer in their own words: `allow_other`

**Use `allow_other` sparingly.** The point of a decision prompt is to steer the
user to concrete, suggested choices you can act on directly. Offer Other only
when a plausible answer genuinely falls outside the list you can write (such as
a region, a name, or a version) — not by default, and not on every question of
a set. If you find yourself adding `allow_other` everywhere, write better
choices or ask in prose instead.

- **Good:** `"Which region for the canary?"` with `choices: ["us-central1", "europe-west1"]`
  and `"allow_other": {"label": "Another region", "placeholder": "e.g. europe-west4"}` —
  the user may want a valid region you did not list.
- **Bad:** `"Deploy to staging or hold?"` with `choices: ["Deploy", "Hold"]`
  and `"allow_other": true` — a closed binary decision; adding Other weakens
  the prompt without a genuine open set of answers.

Accepted forms on a decision object (`allow_other` remains reserved on
individual choice objects):

```json
"allow_other": true
"allow_other": "Something else"
"allow_other": {
  "label": "Another region",
  "description": "Any GCP region code",
  "placeholder": "e.g. europe-west4"
}
```

- `false` is the same as omitting `allow_other`. In the object form, `label`
  defaults to `"Other"`, and `description` and `placeholder` are optional.
- **Limits:** `label` is one line of plain text, 1 to 100 characters, and must
  differ (case-insensitively) from every fixed choice label; `description` is
  one line, at most 300 characters; `placeholder` is one line of plain text, at
  most 100 characters. The user's typed answer is one line of plain text (no
  newlines, tabs, control, or bidi/format characters), 1 to 200 characters
  after trimming.
- Even with `allow_other` enabled, **1 to 10** fixed choices are required (0 is
  refused; use a plain message for free-form-only questions). `default` cannot
  name Other (`$other` or its label).
- **What comes back:** the choice ID is `$other`, the label is the decision's
  Other label, and the typed text is Go-quoted (`text="..."`) right after the
  parenthesis:
  - Single: `[decision-response] region v1 ("Which region for the canary?") answer=Another region (choice $other) text="europe-west4 canary" by <user>`,
    with envelope metadata `choice_id=$other` and `decision_other_text` (the
    unquoted typed text).
  - Set: each Other row carries `answer=Another region (choice $other) text="europe-west4 canary"`
    in the body; envelope metadata `decision_answers` carries `$other`
    (`region@1=$other,proceed@1=1`) and no free text.
- **Recording a prose reply:** when the user replies in prose instead of
  clicking Other, record it with `--other` (repeatable, and combinable with
  `<key>=<choice-id>` pairs in one batch; do not pass `$other` positionally or
  mix with the two-argument `<key> <choice-id>` form):

  ```bash
  scion decisions record --from <message-id> --other region="europe-west4 canary"
  ```

  If the user answers directly afterwards, submitting `$other` with the exact
  same trimmed text confirms the interpreted answer (not delivered back to you);
  a different choice or different text overrides it and delivers a
  `decision-response` naming the previous answer (for example
  `, overriding interpreted answer Another region text="europe-west4"`).
- **Do not ask for secrets:** Other answers are stored permanently in the
  decision log, visible to everyone who can read the conversation, and
  searchable. Never use `allow_other` to ask for credentials, tokens, keys, or
  other secrets.

## Revise a decision: `--update`

Reusing a key you already sent is refused (`decision_key_exists`) unless you
pass `--update`. With `--update`, each decision whose key exists gets a new
version, and the old version is marked replaced and can no longer be answered.
This works for answered, withdrawn, and dismissed decisions too. To retire a decision
without a replacement, use `scion decisions withdraw`.

**Revising an unanswered set: resend every still-pending key of that set**, not
only the one you changed, in one prompt with `--update`. The new prompt then
holds the whole set and the user answers it with one submit. If you resend only
the changed key, the set is split: the replaced decision moves to the new prompt
and the others stay on the old one.

```bash
scion message "conv:<conversation-id>" --decisions-file <file> --update - <<'EOF_MESSAGE'
Updated: canary is no longer an option this week.
EOF_MESSAGE
```

The output shows what each new version replaced, for example
`deploy-target v2 (replaced v1, pending)`.

## Reading the answer

The answer arrives as a message of type `decision-response`, from the user who
answered, in the conversation of the prompt.

- One decision:
  `[decision-response] deploy-target v1 ("Where should ...") answer=Staging (choice staging) by <user>`
  with envelope metadata `decision_key`, `decision_version`, `choice_id` and
  `decision_provenance`.
- A set: a first line
  `[decision-response] 2 decisions answered by <user> (prompt <message-id>)`,
  then one line per decision:
  `deploy-target v1 ("Where should ...") answer=Staging (choice staging, recommended)`,
  with envelope metadata `decision_batch` and `decision_answers`
  (`key@version=choiceId,...`).

`recommended` means the answer matched your `default`. Act on the answer, and
reply in the conversation when the user expects a result.

A user may also **dismiss** one or more open decisions instead of answering
(once the version is at least 5 minutes old). You receive one
`decision-response` per dismiss call (for example
`alice dismissed D234 ("Ship v0.9 today?"). Reason: ...`, adding
`(your reading: <label>)` when the row held your interpreted answer) with
envelope metadata `decision_batch`, `decision_answers` as JSON entries
(`[{"key":"D234","version":1,"choice_id":"$dismissed","provenance":"direct","outcome":"dismiss"}]`
— unlike set answers, which encode `decision_answers` as the compact
`key@version=choiceId` list), optional `decision_dismiss_reason`, and, when one
decision was dismissed, `decision_key`, `decision_version`,
`choice_id` (`$dismissed`) and `decision_provenance`. Treat a dismissal as a
closed question: do not ask again unless the situation changed, and if you must
re-ask, pass `--update` (reusing a dismissed key without `--update` is refused
with `decision_key_exists`).

## Withdraw an obsolete decision

When a pending decision stops mattering (you found the answer yourself, the
plan changed, or the user answered in prose without matching a choice), retire
it so it leaves **Waiting on you**, stops counting towards your pending cap,
and refuses late answers:

```bash
scion decisions withdraw D234 --reason "CI found a regression"
scion decisions withdraw D234 --set --reason "Moving release to Monday"
```

`--set` also withdraws every other pending decision of the prompt each key
belongs to, leaving answered siblings (including interpreted answers awaiting
confirmation) untouched. Repeating `withdraw` on an already-withdrawn decision
is a no-op; no message is posted in the conversation, so send an ordinary
message if bridged readers should know.

## Inspect your decisions

```bash
scion decisions list
scion decisions list --search staging
scion decisions list --decided
scion decisions list --withdrawn
scion decisions list --dismissed
scion decisions list --all
scion decisions show <key>
```

`list` shows pending decisions by default; `--decided` shows answered ones,
`--withdrawn` shows withdrawn ones, `--dismissed` shows dismissed ones, `--all`
adds replaced, withdrawn and dismissed versions, and `-q`/`--search <text>` (2
to 256 characters) searches the question, key, effective answer label, Other
text, and dismissal reason. `show <key>` prints every version of a key with its choices,
withdrawal or dismissal details, and every recorded answer. Both take `--json`.

## A user answers in words instead

Sometimes the user replies in prose ("go with staging") instead of answering the
prompt. When the reply clearly answers a pending decision, record it so the
decision log and the prompt show the answer:

```bash
scion decisions record <key> <choice-id> --from <message-id>
scion decisions record --from <message-id> <key>=<choice-id> <key>=<choice-id>
```

- `<message-id>` is the user's reply. Find it with
  `scion conversation messages "conv:<conversation-id>" --limit 10 --json`
  (each message's `id`).
- The reply must be written by a user (not an agent) who may post in the
  prompt's conversation, and be later than the prompt. All pairs recorded at
  once must be from prompts in that same conversation.
- Record up to 10 pairs at once with the `key=choice-id` form. Do not mix the
  two forms.
- This records an *interpreted* answer, shown apart from answers the user gave
  directly. The user can still confirm or override it. If the reply is
  ambiguous, ask instead of recording.

## Limits

- At most 10 decisions per prompt and 1 to 10 choices per decision.
- At most 50 pending decisions per agent. A prompt that would exceed it is
  refused (`too_many_pending_decisions`): wait for answers, withdraw obsolete
  pending decisions with `scion decisions withdraw`, or replace pending
  decisions with `--update` instead of adding new ones.
- An all-digit key must be a number the Hub assigned to you.

## When decisions are off

Decision prompts need the `web.decisions` experiment on the Hub. If a send is
refused with `experiment_disabled`, or `scion decisions` reports that decisions
need the `web.decisions` experiment, send the question as ordinary Markdown
instead: options in a list with their trade-offs, your recommendation, and the
question on its own line.
