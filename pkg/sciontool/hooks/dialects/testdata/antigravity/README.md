# Antigravity hook-payload fixture

`hook-payloads-1.2.12.jsonl` is a real, captured sequence of stdin payloads
the antigravity (`agy`) CLI sends to `sciontool hook --dialect=antigravity`,
captured from `agy` version `1.2.12` (the version
`harnesses/antigravity/Dockerfile` pins via `ARG AGY_VERSION`). It was
driven against a local, credential-free mock implementing the Gemini
Developer API's `generateContent`/`streamGenerateContent` shape via `agy`'s
own documented `GOOGLE_GEMINI_BASE_URL` override — no real Google account,
OAuth token, or API key was used or needed. Every record is the CLI's real,
unmodified hook payload with three scrub substitutions applied (see below);
nothing was hand-written or hand-edited.

## Contents (10 records, one continued conversation, in capture order)

1. `PreInvocation` (`invocationNum=0`)
2. `PreToolUse` (`run_command`, reading a file)
3. `PostToolUse` (`run_command`, no error)
4. `PostInvocation` (`invocationNum=0`)
5. `PreInvocation` (`invocationNum=1`)
6. `PostInvocation` (`invocationNum=1`)
7. `Stop` (`terminationReason=NO_TOOL_CALL`)
8. `PreInvocation` (`invocationNum=0`, new turn — a second, separate prompt)
9. `PostInvocation` (`invocationNum=0`)
10. `Stop` (`terminationReason=NO_TOOL_CALL`)

Records 1-7 are one turn that forces two real model requests (a tool call,
then a follow-up call with the tool result): this is the real-world
evidence that `PostInvocation` fires one per main-loop model request, not
once per turn — one `Stop` (one turn) pairs with two `PreInvocation`/
`PostInvocation` pairs (`invocationNum` 0 and 1). Records 8-10 are a second,
separate turn in the same conversation; `invocationNum` resets to 0,
confirming it is scoped per-turn.

**Known undercount.** The mock's raw request log (kept alongside the
capture, not part of this fixture) shows a fourth real model request beyond
these three: `agy`'s own conversation-title generation, against a
`*-flash-lite-*` model. It fires no Invocation hook at all, so it is
invisible to this mechanism entirely. Failed or retried main-loop attempts
were not captured either, so `PostInvocation`'s behavior on an error path
(and any `status`-equivalent field) is uncharacterized by this fixture.

## Usage (absent)

Behind the scenes, two of the three real model responses in this capture
carried a full Gemini `usageMetadata` block (distinct, non-zero
`promptTokenCount`/`candidatesTokenCount`/`cachedContentTokenCount`/
`thoughtsTokenCount`/`totalTokenCount` values) and one carried none at all.
**`PreInvocation` and `PostInvocation` are identical in shape across every
record here** — `conversationId`, `workspacePaths`, `transcriptPath`,
`artifactDirectoryPath`, `modelName`, `invocationNum`, `initialNumSteps`,
nothing else — regardless of whether the underlying model response that
invocation wrapped had usage data or not. Neither event carries any
usage/token field, matching `agy`'s own embedded hooks documentation
verbatim ("`PostInvocation` Input (stdin): Same as `PreInvocation` input").
This is why `harnesses/antigravity/dialect.yaml` maps no token fields for
either event: there is nothing to map. Antigravity ships calls-only; a
tokens follow-up is filed separately (design §3.7/§9, "if not, it ships
calls-only and tokens become a follow-up").

## Scrubs

Three substitutions, applied everywhere they occur in the raw capture:
the capture host's scratch home directory, the capture host's scratch
workspace directory, and the real per-session conversation UUID → a
placeholder UUID. Records are re-serialised with sorted keys; parsed values
are otherwise identical.
