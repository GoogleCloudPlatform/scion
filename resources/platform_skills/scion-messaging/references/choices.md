---
experiment: web.chat_choices
---

# Asking users to decide: choice prompts

A **choice prompt** is a message that carries one or more keyed decisions. Each
decision has a question and 2 to 10 choices. In web chat the user answers with a
click, and the answer comes back to you as a `choice-response` message. Other
surfaces show a Markdown copy of the same questions and options.

## When to use one

Use a choice prompt when the user should pick between a few concrete options and
you can act on the answer directly: approve or hold a step, pick one of two
designs, choose a target. Keep prose for open questions, for options you cannot
list in a short label, or when you need a discussion rather than a pick.

- Send it to a user or a conversation: `@<email>`, `user:<email>`, `conv:<id>` or
  `#<thread>`. Choice prompts cannot go to an agent or a `group[...]`.
- Only an agent can send one (agent mode, inside an agent container).
- The message body is optional: it becomes the intro shown above the decisions.
  Put context and your recommendation there.

## One decision or a set

- **One decision:** the user answers it on its own.
- **A set:** one prompt with 2 to 10 decisions. The user picks an answer for
  every open decision and then submits them together; there is no partial
  submit. Use a set for questions that belong together and that you will act on
  as a group. Send separate prompts for decisions that are independent.

## Send it from a file

Write the decisions to a JSON file and pass `--choices-file`. This avoids shell
quoting problems; `--choices '<json>'` takes the same JSON inline.

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
scion message "conv:<conversation-id>" --choices-file <file> - <<'EOF_MESSAGE'
The release candidate passed CI. I recommend **staging** first.
EOF_MESSAGE
```

`--choices-file -` reads the JSON from stdin; then the body cannot also come
from stdin, so use a positional body or `--body-file <path>`.

The JSON is an array of decisions:

| Field | Meaning |
|---|---|
| `id` | Optional decision key. Use a short key you will recognize, such as `deploy-target`: `[A-Za-z0-9_-]`, at most 32 characters, with at least one non-digit. Leave it out and the Hub assigns your next number (`1`, `2`, ...). Keys compare case-insensitively. |
| `question` | Required. Markdown, at most 2000 characters. |
| `choices` | Required, 2 to 10. Each is a string label, or an object with `label` (one line, at most 100 characters), optional `id` (`[A-Za-z0-9_-]`, at most 32 characters) and optional `description` (one line, at most 300 characters). A choice without an `id` gets its position (`1`, `2`, ...). Choice ids and labels must each be unique within the decision, compared case-insensitively. |
| `default` | Optional. Marks your recommended choice, by its `id` or `label`. |

Other fields are refused. The whole set is at most 32 KiB of JSON.

On success the CLI prints one line per decision, for example
`deploy-target v1 (new)` and `1 v1 (new)`. Keep the keys: you need them to read
or replace the decisions. With `--format json` you get the Hub's full send
result.

After a successful send the CLI marks you as waiting for input, the same as
`sciontool status ask_user`, with the first question as the summary.

You cannot combine choices with `--attach`, `--artifact`, `--wake`, `--in`,
`--at`, `--cc`, `--channel` or `--thread-id`.

## Revise a decision: `--update`

Reusing a key you already sent is refused (`choice_key_exists`) unless you pass
`--update`. With `--update`, each decision whose key exists gets a new version,
and the old version is marked replaced and can no longer be answered. This works
for answered decisions too.

**Revising an unanswered set: resend every still-pending key of that set**, not
only the one you changed, in one prompt with `--update`. The new prompt then
holds the whole set and the user answers it with one submit. If you resend only
the changed key, the set is split: the replaced decision moves to the new prompt
and the others stay on the old one.

```bash
scion message "conv:<conversation-id>" --choices-file <file> --update - <<'EOF_MESSAGE'
Updated: canary is no longer an option this week.
EOF_MESSAGE
```

The output shows what each new version replaced, for example
`deploy-target v2 (replaced v1, pending)`.

## Reading the answer

The answer arrives as a message of type `choice-response`, from the user who
answered, in the conversation of the prompt.

- One decision:
  `[choice-response] deploy-target v1 ("Where should ...") answer=Staging (choice staging) by <user>`
- A set: a first line
  `[choice-response] 2 decisions answered by <user> (prompt <message-id>)`,
  then one line per decision:
  `deploy-target v1 ("Where should ...") answer=Staging (choice staging, recommended)`.

`recommended` means the answer matched your `default`. Act on the answer, and
reply in the conversation when the user expects a result.

## Inspect your decisions

```bash
scion decisions list
scion decisions list --decided
scion decisions list --all
scion decisions show <key>
```

`list` shows pending decisions by default; `--decided` shows answered ones and
`--all` adds replaced versions. `show <key>` prints every version of a key with
its choices and every recorded answer. Both take `--json`.

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

- At most 10 decisions per prompt and 2 to 10 choices per decision.
- At most 50 pending decisions per agent. A prompt that would exceed it is
  refused (`too_many_pending_decisions`): wait for answers, or replace pending
  decisions with `--update` instead of adding new ones.
- An all-digit key must be a number the Hub assigned to you.

## When choices are off

Choice prompts need the `web.chat_choices` experiment on the Hub. If a send is
refused with `experiment_disabled`, or `scion decisions` reports that decisions
need the `web.chat_choices` experiment, send the question as ordinary Markdown
instead: options in a list with their trade-offs, your recommendation, and the
question on its own line.
