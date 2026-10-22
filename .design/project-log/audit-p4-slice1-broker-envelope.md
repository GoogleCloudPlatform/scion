# Remaining-audit P4 slice 1: ingress operation context and broker envelopes

Date: 2026-10-22 UTC. Workstream: ptone/scion#2401 (tracker #2369).
Base: `fce92df9f2d2b888912645b679beda6a3c147f3b`. Design of record: the EM's
P4 slice-1 design receipt (architect handoff S1.1-S1.9, EM questions Q1-Q10,
rulings R12, R13 and R14). This is a deliberately **partial** P4: it is not full
P4 and not P3 completion, and audit activation stays held.

## What changed

- **Ingress operation context** (`pkg/hub/audit_operation_middleware.go`).
  `applyMiddleware` installs `auditOperationMiddleware` directly inside
  RequestLog and outside UnifiedAuth. Rule: keep an operation already on the
  context (conduit relay re-entry), else adopt the server-minted request ID
  when `auditevent.NewOperationContext` accepts it, else store nothing. There
  is no `StartOperation` fallback, so decision dispositions are unchanged.
- **Broker family catalog** (`pkg/hub/auditevent/catalog.go`, `broker.go`).
  Five literal entries, resource kind `runtime_broker`: `auth_failure`
  (decision/deny), `register`, `rotate`, `deregister` (commit/succeeded),
  `join` (commit/succeeded, failure/failed per R13). Closed `failure_code`
  enum of 13 values. Typed builder `BuildBrokerEvent`: no Details and no
  free-form field; it canonicalizes `source_ip` with netip, sanitizes and
  truncates `user_agent` (auth_failure only), applies the claimed broker ID
  bound, and sets `request.id` only when it equals the correlation.
- **Validator** (`auditevent/validate.go`): `resource` is required only when
  the entry lists it in `RequiredEnvelopeLeaves` (Q1). Both earlier entries
  list it; pinned by tests.
- **Conversion** (`pkg/hub/audit.go`, `audit_broker_envelope.go`).
  `LogAuditLogger.LogBrokerAuthEvent` routes auth_failure, register, join,
  rotate and deregister to envelopes emitted through a SlogSink over the
  shared async `audit` writer (`Server.auditWriterHandler`, wired in `New`
  after `initDecisionAuditLog`). Always on; never gated on the decision
  experiment. One envelope replaces each former legacy line. auth_success,
  link, unlink, update_settings and the dead revoke constant are unchanged.
- **Typed fields** on `BrokerAuthEvent`: `FailureCode`, `ProjectID`,
  `RegisterOperation`. The converted branch never reads `FailReason` or
  `Details`.
- **brokerauth.go**: unexported sentinels wrapped with `%w`; `Error()` text
  byte-identical (pinned on the HTTP bodies).
- **Call sites**: `LogRegistrationEvent` takes the operation;
  `LogEmbeddedRegistrationEvent` (new) carries the project scope;
  `LogJoinEvent` takes the failure code. The
  `AuditLogger` interface and every test double are unchanged.

- **Classification** (R14): a secret lookup the store rejects with
  `ErrInvalidInput` (a non-UUID claimed broker ID) is `unknown_broker`, not
  `internal`, so an unauthenticated caller cannot fill the `internal` bucket.
- **Principal** (R14): every empty actor, embedded register included, is
  dropped with the warning; nothing is attributed to the hub.

## Disclosures

- Re-entry also changes decision and boundary record content (outer
  correlation, no `request.id`); dispositions are unchanged.
- auth_failure is unauthenticated and shares the audit writer; under a flood
  other audit records can be dropped. Drops are counted
  (`scion.logging.write.failures{writer=audit}`); the design follow-up is
  ptone/scion#5649. `source_ip` ignores TrustedProxies (pre-existing;
  ptone/scion#5650).

- Re-entry: correlation is the outer operation; `request.id` is omitted
  because `auditevent/validate.go:185` (at fce92df9) rejects
  `request.id != correlation_id`.
- Missing correlation, no sink, missing actor or a failed administrative
  action: no event, plus one rate-limited warning (action, reason,
  failure_code only). A non-parity case; production always has a request ID.
- auth_failure is pre-authentication and normally carries no credential, as
  the legacy line had none.
- `source_ip` and `user_agent` keep data the legacy lines already logged in
  the same sink class; nothing new is collected. `user_agent` is on
  auth_failure only. A sanitized value is flagged `user_agent_sanitized`.

## Deferred

The no-request-ID fallback, every other audit family (fan-out), boundary
CREATE async (oversize analysis), actor/delegation leaves, #4740 and #4741.

## Verification notes

Dev-time only, never evidence: `gofmt`, `go test ./pkg/hub/auditevent/...`,
one masked non-test `go build ./pkg/hub/`, and `hack/hubshard -parity`. The
pkg/hub tests run only in the gate windows (round 1 added dev-time compiles
of ./cmd and the affected hubshards). New and affected tests live in
hubshards 1, 2, 3 and 6.
