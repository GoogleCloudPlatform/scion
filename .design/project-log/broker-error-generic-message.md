# Broker error answers: generic client text, detail in the hub log

**Date:** 2026-11-10
**Branch:** scion/broker-error-generic-message

Broker error answers carry the HTTP status and a safe code; the broker's own text goes to the hub log only.

## New behaviour

- `brokerStatusError.Error()` names only the status and, when well formed,
  the broker's error code: "runtime broker returned error 500 (runtime_error)".
  Status codes and error codes in responses stay the same.
- `newBrokerStatusError` (used by the HTTP transport, the control-channel
  client and the workspace tunnel) logs each broker error answer once:
  broker id, status, code, full body length, and the body rendered by
  `brokerBodyForLog` (at most 2 KiB).
  A 404 is logged at debug level, any other status at warn. Request headers
  are not logged. `LogValue` renders the error's text, status, code and body
  length, without the body.
- `renderBrokerText` replaces invalid UTF-8, control characters and other
  non-printable runes with U+FFFD and caps its OUTPUT at the given size, on a
  rune boundary. It backs both the log rendering and `brokerErrorMessage()`.
- `brokerErrorMessage()` returns the JSON error message rendered at most 512
  bytes, or the fixed text "the runtime broker returned an error". The
  512-byte cap keeps the project storage refusal texts the hub allowlists
  whole; other relayed broker messages (for example git_config_refused,
  validation and conflict answers) are cut at 512 bytes.
- The decode-failure path of `decodeResponseWithSnippet` and the
  control-channel `ImageStatus` error give errors without the body; it goes
  to the log.
- Hash-mismatch repair (`isHashMismatchError`, `repairHashMismatch`) matches
  on the broker body explicitly (`brokerErrorMatchText`), so its behaviour
  stays the same.
- PTY close reasons from the broker (`handleStreamClose`) and from a conduit
  relay or target (`ptyLeafCloseCode`) are passed on only when they are one
  of the fixed protocol tokens (`relayableStreamCloseReasons`), or, for a
  reason of the form "<token>: <detail>" with an allowlisted token,
  as the token alone (the detail is logged). Any other reason becomes ""
  (the close code stays the same) and is logged. The conduit session's
  drain-deadline reason is `conduit.CloseReasonDrainDeadline` and is one of
  the tokens.
- The legacy port-forward tunnel answers a failure with the fixed message
  "Port-forward tunnel failed"; the tunnel's text is logged through
  `brokerBodyForLog`.
- The workspace tunnel (sync-from upload, sync-to apply) builds its error
  with `newBrokerStatusError`; its "timeout" check still matches a broker
  body that names a timeout (`brokerErrorMatchText`) and answers 504.
- The generic dispatch 502 relays, as structured error details, only
  `brokerFailureDetails`: a runtime_error's allowlisted cause; a
  template_error's cause "file_not_found" and the missing file when it is a
  plain base name of at most 255 bytes; a local_worktree_refused's reason as
  one printable line of at most 1024 runes, dropped if it holds a URL with
  userinfo; a run_mismatch's `currentRunId` only as a canonical lower-case
  UUID. Nothing else from the broker is relayed.
- The CLI (`cmd/broker_failure.go`) reads the current text, "runtime broker
  returned error <status> (<code>)", and the hub's structured details first;
  it still reads an older hub's "<status>: <body>" text.
- The broker's input-overflow close reason is
  `wsprotocol.CloseReasonInputOverflow` and is one of the relayed
  close-reason tokens.

## Recorded and start texts

A body-free `Error()` dropped the broker's well-formed message from every
place that took its text from `Error()`: a reincarnation record, the failed
dispatch row written by `reconcile.go` (which also lost the broker's current
run id) and the start API's generic 502.

- `brokerRecordText` (`broker_failure_details.go`) builds that text:
  `Error()`, then the capped, rendered message, then the allowlisted current
  run. A message holding a URL with userinfo, a path separator or a token
  shape (a 20+ run of token characters with a digit) is replaced by the
  generic text, as is a missing one; a message equal to the code is left out.
- The message is read once (`brokerErrorRawMessage`), and
  `relayableBrokerMessage` judges both that raw text and the exact rendered
  text that is appended. The userinfo check stays as defence in depth,
  although the separator check already refuses any URL.
- `dispatchFailureText` uses it for any broker HTTP error and keeps the
  hub's wrapping text; the dispatch row and the start 502 go through it. When
  an error wraps a broker answer but writes its own text
  (`DeleteRunMismatchError`), nothing is replaced and the result is that
  error's own body-free text; the dispatch row in `reconcile.go` reaches this
  through a run-scoped delete the broker refuses.

## Behaviour notes

- Agent delete: a placement refusal answers every caller with the
  refusal's message. For any other failure, callers who are not admins see
  the generic delete message. Admin callers see a broker's JSON
  error.message on a 409 conflict, capped and sanitised; the hub's own text
  for runtime_unavailable and for failures that are not broker answers; and
  the status and code for any other broker error answer.
- A failed reincarnation record holds the broker's status and code and,
  when it passes the filter, its rendered message.
- Structured relays that already pick fixed fields stay as they were:
  allowlisted `details.cause`, flat runtime-target refusal codes and detail
  keys, skill / workspace-storage / project-storage refusals.
- Not done: the code and details accessors still decode the body on their
  own (the message has one shared read); the bodies are small. A digit-free
  token-shaped run passes the filter; the create path's 404 arm and default
  502, and the suspend fallback, still relay or drop the message outside it;
  `brokerCurrentRunID` writes the run id without the UUID check.

## Tests

- `broker_status_error_test.go`:
  - `Error()` and `brokerErrorMessage()` carry no body text;
  - the log line has broker id, status, code and body_len, at debug for a
    404; `LogValue` has no body;
  - a marker past the 2 KiB cap does not reach the log (for a JSON and a
    plain body), control characters are replaced, and output built from
    replaced bytes stays within both caps;
  - hash-mismatch matching and repair routing;
  - the close-reason allowlist (unit, conduit mapping, broker StreamClose
    end to end);
  - the create/start/restart dispatch error switch; the decode-failure
    error;
  - the JSON error message cap and rune filter, in a response too; every
    allowlisted project storage text passing byte-identical;
  - `brokerFailureDetails` (the accepted file, reason and run id, and each
    rejected shape) and the dispatch 502 carrying them;
  - `brokerRecordText`: the table, one read feeding both the filter and the
    renderer, the filter passing the emitted text for inputs the renderer
    changes, a token past the cap, and the `dispatchFailureText` fallback.
- `broker_error_body_handlers_test.go`: agent logs, agent delete (admin
  caller) and legacy port-forward handlers (the tunnel's error answer and a
  failed tunnel request) keep status and code and carry no body text; the
  body reaches the hub log on the logs path. A routed image status query
  with a broker error answer gives an error without the body. Workspace
  sync-from and apply keep their 502 and code without the body, and a
  broker body naming a timeout still answers 504.
- `cmd/broker_failure_test.go` feeds the CLI the current hub text and
  details.
- New: the start 502 (`status_launch_writes_test.go`) and the provision-only
  warning (`identity_mapping_error_test.go`) with a token-shaped broker
  message carry the generic text in its place.
- Updated: `msg_sanitize_1841_test.go`, the reincarnate 409 E2E test (status,
  code and rendered message, not the raw body), `workspace_handlers_test.go` (the removed
  `errRuntimeBrokerError`), `dispatch_typed_error_test.go` (the row's text
  through `dispatchFailureText`), `httpdispatcher_credential_revoke_test.go`
  (the status, not the broker's text) and `deferred_delete_rowless_test.go` (the dispatch
  row's full text).

## Lessons

- A test that pads a body for a parser that reads the whole body keeps the
  padding inside the JSON.
- Expected handler texts follow the exact error chain the fixture builds.
- Making a shared text body-free changes every reader of that text; search
  the callers of `Error()` for records and responses before the change, and
  compile the new tests early so fixture errors surface during review.
- After a change to a shared text, run every pkg/hub shard, not only the
  targeted tests: an older assertion on the same record (the reincarnate
  409 test) failed only in a full shard run.
