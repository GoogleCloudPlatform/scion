# M2 bounded authorization-audit merge checkpoint

Date: 2026-10-06 UTC. Workstream: ptone/scion#2379.

This checkpoint integrates the immutable selected-main baseline and resolves
the accepted authorization merge conflicts. Amendments 6–12 to the protected
default-off brief authorize this five-path compatibility slice and its durable
merge. It does not implement or activate the subsequent default-off router,
registry, admission, freshness, drain, health, or production cutover surfaces.

## Baseline and path accounting

- First parent: `dd44eed7b1c4d085765b0b3f42eafbbf5f90a0d5` (M2).
- Second parent: `48c3666ac5c39682f066b3473f0c3b17c12f73de` (selected-main).
- Merge base: `64a549c402fe941a9ea7702a453ecf60b0b70d94`.
- Selected-main contains #2502 merge
  `8767f43aeb8ada3fe2b2e05d78da14b8f3b4ba5b`.
- Provisional conflict merge tree:
  `a0b47f623b800ff0fe91f1ee7574a11978af0e0f`.
- The raw inherited manifest contains 2,492 rows / 100,577 bytes, SHA-256
  `a1d088a488d4071529201462fcccccffe60fdf9c72f6dfe2e8eb0c7b365be6c3`.
  It is preserved outside Git at the protected workstream reviews path as
  `m2-defaultoff-inherited-baseline-manifest.txt`. These are inherited baseline
  integration changes, not manual implementation scope. Moving-main drift was
  explicitly dispositioned without fetching or integrating the moving tip.

The five accepted manual paths are `pkg/hub/authz.go`,
`pkg/hub/authz_candelegate.go`, `pkg/hub/authz_bearer.go`,
`pkg/hub/authz_merge_conflict_test.go`, and
`pkg/hub/authz_audit_reason_test.go`. This project log is the sixth manual path.
All other merged changes remain the accepted provisional baseline content.

## Conflict decisions and compatibility repair

- H1 retains M2's operation-free evaluation request while carrying selected-main
  target evidence and bearer-run memo state. Ordinary Decide retains one audit
  emission; bearer introspection remains non-emitting. The internal bearer
  evaluation literal uses the operation-free request type.
- H2 preserves the selected-main constraint/relationship fault condition,
  authorization outcome, denial text, and resolution-error deny cause, and adds
  dependency-unavailable audit metadata.
- H3 preserves selected-main bearer boundary, permission ceiling, live project
  admission, target evidence, and memo behavior. Denied helper and direct
  stage-1 returns classify audit metadata using typed DenyCause: resolution
  errors are dependency-unavailable; other denials are policy-denied.
- H4 retains selected-main delegation signatures, guard order, target
  construction, project-admission arguments, denial text, and permission
  ceiling logic. Local denials use policy metadata. Typed project-access lookup
  faults use dependency metadata; every other error or refusal still denies.
- Amendment 9 corrected one existing audit-reason test call by supplying
  `context.Background()` to the context-first delegation signature. Assertions
  and fixtures were unchanged. This fifth path was separately diff-reviewed.

Stage A intentionally withheld H2–H4 final audit metadata to prove a meaningful
RED. Stage B changed only metadata in authz.go and authz_candelegate.go after
manager acceptance. No reason-string parsing or error-to-allow path was added.
The #2502 writer and metrics files remain byte-identical to selected-main.

## Focused RED and GREEN

The exact accepted RED command was:

```sh
HEAVY_BUILD_MAX_WAIT=2700 /scion-volumes/scratchpad/tools/heavy-build.sh sh -c '
available=$(free -g | awk '\''/^Mem:/ {print $7}'\'')
free -g
if [ -z "$available" ] || [ "$available" -lt 30 ]; then
  echo "MEMORY_GATE_BLOCKED_AT_ACQUIRE available=${available:-unknown}"
  exit 75
fi
ulimit -v 12000000
exec timeout 15m env GOMEMLIMIT=6GiB GOGC=40 GOFLAGS=-gcflags=-c=1 GOCACHE=/scion-volumes/gocache go test -timeout 14m -count=1 -p 1 -v ./pkg/hub -run '\''^(TestAuthzConflictResolution_OperationFreeBearerInputs|TestAuthzConflictResolution_FaultAuditReasons|TestAuthzConflictResolution_UATDelegationBoundaryAndFault)$'\''
'
```

Amendment-9 RED acquired slot-2 after 225 seconds; acquire-time available memory
was 45 GB. Exit 1, wall duration 832 seconds, package test duration 1.992 seconds.
H1 passed. All 15 failed assertions across 13 failing subtests were intended
H2–H4 AuditReason comparisons; three delegation pass cases passed. No unrelated
outcome/reason/cause, compilation, setup, resource, or timeout failure occurred.
Manager independently accepted this evidence.

The exact accepted GREEN command was:

```sh
HEAVY_BUILD_MAX_WAIT=2700 /scion-volumes/scratchpad/tools/heavy-build.sh sh -c '
available=$(free -g | awk '\''/^Mem:/ {print $7}'\'')
free -g
if [ -z "$available" ] || [ "$available" -lt 30 ]; then
  echo "MEMORY_GATE_BLOCKED_AT_ACQUIRE available=${available:-unknown}"
  exit 75
fi
ulimit -v 12000000
exec timeout 15m env GOMEMLIMIT=6GiB GOGC=40 GOFLAGS=-gcflags=-c=1 GOCACHE=/scion-volumes/gocache go test -timeout 14m -count=1 -p 1 -v ./pkg/hub -run '\''^(TestAuthzConflictResolution_OperationFreeBearerInputs|TestAuthzConflictResolution_FaultAuditReasons|TestAuthzConflictResolution_UATDelegationBoundaryAndFault|TestEvaluateBearerCeiling_MatchesUATRequestDecision|TestEvaluateBearerCeiling_EmitsNoDecisionAudit|TestEvaluateBearerCeiling_StoreFaultDenies|TestUATGate_HubBoundaryReachesAccessibleProjects|TestUATGate_ProjectTargetRequiresCurrentAccess|TestUATGate_RequestTargetEvidence|TestEvaluateBearerCeiling_EvidenceMustNameEvaluatedPermission|TestRelationshipProjectAccess_DecideRefusalIdentical|TestCanDelegate_UATCannotDelegateOutsideProject|TestCanDelegate_UATCannotCreateSystemGrants|TestAuthz_IsIndeterminate_AccessConstraintLoadError|TestExplainAPI_EffectivePermissionsUsesNonEmittingIntrospection|TestDecisionAudit_Sampling)$'\''
'
```

Amendment-11 GREEN acquired slot-1 immediately; acquire-time available memory
was 36 GB. Exit 0, wall duration 580 seconds, package result
`ok github.com/GoogleCloudPlatform/scion/pkg/hub 6.519s`. All 16 named tests and
27 subtests passed, with no failure or skip. Amendment 12 independently accepts
this GREEN. No additional Go/build/vet/lint/race/full-suite execution is allowed.

Protected evidence lives under
`/scion-volumes/scratchpad/projects/audit-update/reviews/`:

| Evidence file | Result | SHA-256 |
| --- | --- | --- |
| m2-defaultoff-conflict-stage-a-red.txt | Initial memory 27 GB; no heavy command | e5812f73b8c8f30d9b7f6c17b5327d13a7401e07aab830dcde7e5d07b5084ec6 |
| m2-defaultoff-conflict-stage-a-red-callback-1740.txt | Memory 46 GB; queue-only timeout 124 / 900s; no Go | 069c20e577d2261f22f960e2176f9e9e86d1b2eba205adf7a286f7555a61edc0 |
| m2-defaultoff-conflict-stage-a-red-amendment-8.txt | Memory 46 GB; compile failure before subtests, exit 1 / 736s | 44254b4cb2427dd5158d3aba1c14b245897a0b053538aaa328572452215c0e97 |
| m2-defaultoff-conflict-stage-a-red-amendment-9.txt | Accepted intended RED; 608 lines / 53,616 bytes | 36c3b9de511925f8bdbb90cc063fd5a75c3db98bde1b02dc3ad57c5f136fb34d |
| m2-defaultoff-conflict-stage-b-green-amendment-11.txt | Accepted GREEN; 1,166 lines / 134,439 bytes | 3dcb830f0069676d2cfd65c9c78ce9e36b86030dd31964f3bdfb605c4113aa15 |

The compile failure was the single context-first signature mismatch corrected
under Amendment 9. Every reattempt required explicit coordinator/manager
authority; no automatic retry or resource-cap enlargement occurred. Reports
1–3 and all evidence remain sealed outside Git.

## Remaining gates and non-activation

The fresh significant-conflict review is a separate manager-controlled gate
after durable push. No reviewer was started and explain round 7 was not used.
The subsequent registry/router/admission slice and its implementation map are
not authorized by this closeout. No production census or ratified positive
handler was supplied; no T1–T25 entry, SCC grounding, production acceptance,
clean-build/live-profile/trust/activation gate, monotonic freshness bridge,
bounded NEW drain, or decision-health projection is approved by these tests.
Production admission/cutover remains rejected pending those independent gates.
The finite fixtures prove merge compatibility only, not production trust.

No PR, main merge, deployment, experiment activation, live-setting change,
legacy retirement, logging/sink behavior change, or #2392 timing claim is part
of this checkpoint. Retained legacy #2502 lifecycle remains separate from the
unimplemented NEW 2-second drain. Human-directed questions sent by this worker:
none; manager acceptance exchanges are agent-directed. After delivery the worker
remains blocked pending durable receipt and explicit lifecycle disposition.

## Default-off vertical slice — accepted finite mechanics, 2026-10-06

Implementation base: `591e24c340fa9b0f68102a11c0c649a98749c58f`.
Amendments 13–23 separately authorize this slice after the compatibility
checkpoint described above. That earlier merge/conflict history is inherited
compatibility work, not this slice's implementation scope. The eleven final
implementation/test paths are:

```text
pkg/experiments/registry.go
pkg/experiments/registry_test.go
pkg/hub/audit_authz_test.go
pkg/hub/decision_audit_admission.go
pkg/hub/decision_audit_admission_test.go
pkg/hub/handlers_health.go
pkg/hub/handlers_health_summary_test.go
pkg/hub/handlers_health_test.go
pkg/hub/operational_settings.go
pkg/hub/operational_settings_test.go
pkg/hub/server.go
```

This log, `.design/project-log/m2-defaultoff-decision-audit-slice.md`, is the
single additional path. The slice preserves ordinary authorization results,
sampling, H1–H4, and the retained #2502 writer/metrics/legacy lifecycle. It adds
only the server-layer alpha experiment `hub.authorization_decision_audit_v2`,
registered default false, and bounded fail-closed local routing mechanics.

Finite admission validates the whole canonical manifest and exact live
root/legacy/handler/clock/timer/settings/caller graph; missing, extra, drifted,
unknown or over-cap facts reject wholesale. Expected positive constructor facts
are test-only. Successful authoritative read observations alone establish the
q0-based 74-second lease with one-second read slack in the approved 75-second
budget. Source/attachment/sequence/generation/revision/epoch and matching copied
snapshots must still agree at handoff; failed, canceled, overlapping, obsolete,
stale or unproved reads stay off. Wall time, events and cached true are not proof.

One stable router provides K=1 synchronous NEW ownership, with no queue or
hidden goroutine. Ineligible decisions choose legacy exactly once. An admitted
finite NEW failure/panic/nonacceptance/caller cancellation latches fault once;
the triggering record may be lost, with no same-record fallback/retry. The next
record chooses legacy. The admitted finite caller is live-bound before methods;
unproved/hostile or pre-canceled callers reject before NEW. Caller cancellation
during NEW is accounted on synchronous return, without parenting or a watcher.
Independent cooperative mechanics cancel at h+1 and complete/release by h+2;
this is separate from retained legacy's 4s+1s shutdown and HTTP drain behavior.

The locally readable NEW health fault is a critical audit/logging warning:
possible triggering-record loss, NEW off, subsequent legacy routing. It remains
latched independently of the failed sink. Historical legacy drops/closed state
have a separate fixed check. Only its Hub availability effect is noncritical:
health degrades while serving/readiness remain available, and existing truly
availability-critical failures still win. No broader dashboard delivery claim.

Accepted Stage-A RED: protected `reviews/m2-defaultoff-stage-a-seam-red-amendment-15.txt`,
42 lines / 2,278 bytes / SHA-256
`cee01061bfbae74761ca36072f07e592973f9b2cd6b8c451dc1a65158d0abbcb`.
Its three intended failures were the absent compiled registry entry, nil exact
finite admission, and NEW count 0 versus required 1 (legacy count 1). Prior
one-owner/result checks passed; no unrelated failure. It was not rerun.

Accepted Stage-B GREEN: protected `reviews/m2-defaultoff-stage-b-green-amendment-22.txt`,
1,098 lines / 115,716 bytes / SHA-256
`0fa4809e4df2e386945de88c9ee717dd575c886d2c423309a82094f289f6f95c`.
The exact literal command is sealed in that evidence: heavy-build wrapper,
`HEAVY_BUILD_MAX_WAIT=2700` (45-minute queue maximum), acquire-time available
memory >=30 GB, independent `timeout 15m`, `ulimit -v 12000000`,
`GOMEMLIMIT=6GiB`, `GOGC=40`, `GOFLAGS=-gcflags=-c=1`,
`GOCACHE=/scion-volumes/gocache`; targeted `go test -timeout 14m -count=1 -p 1 -v`
on `./pkg/experiments ./pkg/hub`, exact 35-name selector, no full suite/race.
The single invocation acquired slot 2 immediately at 47 GB available, exited 0
in 586.857204 seconds, and produced experiments `ok` (0.005s) and Hub `ok`
(3.902s). All 35 named tests and 112 subtests passed (147 matching RUN/PASS
outcomes), with no missing/unexpected/duplicate/skip/fail/timeout/setup/resource
result. This includes the live-caller mutation, hostile/unproved and pre-canceled
caller rejection, distinct post-handoff caller-cancel mode, and retained invariants.
No repair, retry, extra Go command or resource-cap enlargement followed.

Production NEW remains structurally unadmitted even with a fresh true override:
no ratified production census/manifest, exact live bindings, clean-build/live
profile, elapsed clock/timer, cooperative store/source, accepting handler,
originating caller or complete-return/cleanup contract is supplied. No T1–T25
entry, profile, imported target or trust boundary is approved by finite fixtures.
Independent critical warning/alert/dashboard-delivery proof remains a hard
pre-activation gate. These tests do not prove production persistence, no-loss,
freshness source, drain timing or sink health/alert delivery.

No activation, deployment, live-setting change, cutover, legacy retirement,
PR/main merge, reviewer launch or explain round 7 is authorized. Production
trust/census/live-profile/clock/store/handler/caller/complete-return/alert-delivery
and activation gates remain rejecting. At this log-review stage implementation
is staged, log unstaged, and commit/push remain held for manager disposition.
Earlier compatibility evidence and all sealed RED/GREEN/blocker artifacts are
preserved. Human-directed messages/questions from this worker: none. Lifecycle
completion/deletion remains held pending durable receipt and explicit release.

## Quality-gate corrections — finite mechanics only, 2026-10-06

Correction base: `5ea96416729ccd739d3a4bbd9809026d9498001a`.
Amendments 25–32 authorize this bounded correction checkpoint. The independent
input verdicts were code REQUEST CHANGES, test REQUEST CHANGES, and security
APPROVE exclusively for the bounded default-off posture. Security approval
waived none of the code or test findings and granted no production admission.
Protected reports under the workstream reviews directory are:

| Input | SHA-256 |
| --- | --- |
| m2-defaultoff-code-review-1.md | bf58f8ab7103e584360f25c6bd88c14a648a12a1bdca68a52e81741479cea439 |
| m2-defaultoff-test-review-1.md | ec7dc3877701106332db85b784cfca5c9dc8dae2cc3b501bb8298eb972270414 |
| m2-defaultoff-security-review-1.md | 8bbc91c19e718ba250692c75d24dd4e039b8c797416dd531ca7b6d79be3b2262 |

Code H1 required persistent propagation-lifecycle rejection; M1/M2 required real
lower-authoritative-revision and otherwise-eligible live ownership fixtures.
Test F1–F5 required causal K=1/concurrent handoff, independent h+1 cancellation
and h+2/+1ns completion, occupied NEW close/cancel/wait/reference cleanup and
HTTP drain, authoritative revision regression, and isolated safety boundaries.
A27 adds these finite oracles only in the two existing test paths. Manifest
cardinality coverage truthfully proves seven accepted roles/eight rejected; the
16-entry ceiling is redundant/unreachable in this schema, not tested at 16/17.
Virtual-clock boundaries do not prove real scheduler or arbitrary-I/O bounds.

A28's single accepted expanded RED produced five failing subcases and ten
assertion errors: stop, channel-close, recovered-loop and stop-during-read each
failed two lifecycle assertions; sequence exhaustion failed both the in-read
ownership assertion and persistent poisoned-observation assertion. The tenth
assertion was independently classified as the second manifestation of the same
sequence defect. No RED rerun or automatic repair followed. Protected raw:
`m2-defaultoff-quality-gate-red-amendment-28.txt`, 1,205 lines / 125,624 bytes,
SHA-256 `fab127694a414e678650b59fb16d87076bbeacdfece14f0298a0d435b7eb1628`.

A29 production corrections are confined to decision_audit_admission.go and
operational_settings.go. A gate-owned per-attachment propagation-loss latch
rejects new reads, publication and both candidate/final NEW handoff. Propagation
start, polling, reconnect, subscription exit/recovery and stop carry a captured
router/source/attachment identity; callbacks from an old attachment cannot
clear a current proof. Only explicit source attachment handoff clears the
attachment's lifecycle loss. Refresh poison, clock poison and fault stay latched.
Sequence exhaustion atomically sets poison and empties the router observation;
no successful read can revive it. Refresh completion publishes or clears the
copied Ops proof only for its current attachment. Lock acquisition stays
OperationalSettings.mu -> router.gate, with no reverse acquisition.

A30 corrects only the frozen sequence-exhaustion fixture predicate: it requires
an already-empty router observation and uses the copied Ops proof as the
otherwise-eligible stale baseline before the poisoned emission. Ownership and
persistent-poison assertions are unchanged. This tests the combined atomic
clearing/poison invariant; it does not isolate removal of the poison guard alone.
Lifecycle slot cancellation executes outside both locks. Refresh completion
cancellation executes outside router.gate but still under OperationalSettings.mu,
preserving the preexisting refresh completion behavior; error completion now
uses the same mu -> gate publication ordering. No parent watcher, new goroutine,
queue, exported positive constructor or production trust adapter was added.

A31's single literal 35-name focused GREEN acquired heavy-build slot 4 after
168 seconds with 43 GB available and exited 0. Start 22:24:38.417435Z; end
22:37:54.276329Z; total queue-inclusive wall 795.858956 seconds. Package results:
experiments PASS 0.005s, hub PASS 4.055s. All 35 named tests and 155 emitted
subtests passed: 190 matching RUN/PASS outcomes, with zero missing, unexpected,
duplicate, failed or skipped outcomes and no setup/VCS/compile/resource/timeout/
deadlock/watchdog/unrecovered-panic failure. The caught recovered-loop fixture
panic log is expected in a passing test. Protected raw:
`m2-defaultoff-quality-gate-green-amendment-31.txt`, 1,186 lines / 123,945 bytes,
SHA-256 `c63ed820d42bb8b1378bc4c9e21e9da48ef62a3099a048b37992529f7b8c4724`.

The exact command is preserved in the protected A30 review and A31 raw evidence:
normal heavy-build wrapper, HEAVY_BUILD_MAX_WAIT=2700 (45m queue), acquire-time
available memory >=30 GB, ulimit -v 12000000, GOMEMLIMIT=6GiB, GOGC=40,
GOFLAGS=-gcflags=-c=1, GOCACHE=/scion-volumes/gocache, independent timeout 15m,
go test -timeout 14m -count=1 -p 1 -v ./pkg/experiments ./pkg/hub with the unchanged
literal 35-name selector. No outer timeout, second invocation, retry, race,
full suite, build, vet, lint, generator or resource-cap enlargement followed.

Production NEW remains structurally unadmitted and the registered experiment
remains default false, including under a fresh true production override. Every
hard pre-activation gate remains rejecting: ratified production census and exact
whole manifest; T1–T25/SCC/provenance/constructor authority; clean source/build
and exact live profile/capture/bindings for root, legacy, handler, clock, timer,
settings and originating caller; monotonic elapsed clock/suspension/epoch and
timer scheduler; bounded cooperative authoritative store/source; accepting
handler; complete synchronous cancellation/return/reference cleanup graph;
independent critical audit-warning, alert and dashboard delivery; production
freshness/drain/persistence/no-loss evidence and explicit activation approval.
Finite fixture GREEN supplies none of those production contracts or approvals.
The retained #2502 legacy lifecycle and ordinary authorization/sampling/results
remain separate from finite NEW mechanics.

Fresh post-fix independent re-review is still pending. No final quality-gate
approval, reviewer launch, activation, deployment, live-setting change, cutover,
legacy retirement, main merge, PR or explain round 7 is claimed. This checkpoint
contains exactly four correction paths plus this appended project log. Earlier
history is preserved byte-for-byte, and all sealed reports/evidence stay outside
Git unchanged. Human-directed messages/questions from this worker: none.
Completion/deletion remains held for durable receipt, independent re-review and
explicit lifecycle disposition.

## Fresh test-review closure — bounded fixture correction, 2026-10-07

Checkpoint base: `2d11b29bbdefdff419b76d7d245ca7c4419759fb`.
Fresh independent code review APPROVE and security review APPROVE apply only
to the bounded default-off correction. Fresh test review REQUEST CHANGES for
R1 High (recovered-loop masking by stop cleanup) and R2 Medium (missing actual
captured-attachment publication coverage); neither approval waived those items.
Protected fresh report SHA-256 identities:

| Report | SHA-256 |
| --- | --- |
| m2-defaultoff-fix-code-review-1.md | f85f6f8fa225e696babe3cd54b7b8614ade6f649b64594714eaff7aa9ec88216 |
| m2-defaultoff-fix-test-review-1.md | 5eeec2c0116a58ce2c4dc06d0abc60244760ddf88fdc8bc489c88ad9fbe333f2 |
| m2-defaultoff-fix-security-review-1.md | 7fa71c8528a59278e13d989a6808d6c4c844f03b53bc709478b8ad3ae6912206 |

A33 changes only operational_settings_test.go. R1 now asserts the real recovered
subscription path's persistent loss after unsubscribe completion and BEFORE the
independent stop cleanup closure: loss already latched, both copied/router proofs
wholly empty, healthy same-attachment Refresh unable to revive either proof,
and exactly one legacy/zero NEW owner. Only then stop/join the poll goroutine.
The obsolete cleanup comment is corrected. Existing stop/channel-close/read-stop
assertions and the later explicit handoff recovery remain separate and intact.

R2 adds four finite actual OperationalSettings captured-A callbacks after explicit
same-source handoff to fresh B: loss, successful read completion, store-error
completion, and rejected/untracked read completion. Each requires full copied
Ops/router B proof equality and exactly one NEW/zero legacy owner. Current B loss
then invalidates both proofs and healthy same-attachment reads remain legacy-only;
only explicit C handoff and fresh captured C read recover finite NEW ownership.
Tracked A success/error setup releases only A's old pending router bookkeeping
before real B publication, then resumes the ORIGINAL A Ops completion. That
preparatory router call is not the publication oracle and proves no availability
with an outstanding tracked-old read. Same-valued generic cache ingestion keeps
private publication ownership separate from snapshot mismatch. No production
source, top-level test name, selector, arbitrary callback or timing bound changed.

Protected A33/A34 artifacts under the workstream reviews directory:

| Artifact | Lines | Bytes | SHA-256 |
| --- | ---: | ---: | --- |
| m2-defaultoff-fix-test-closure-amendment-33.patch | 243 | 10721 | ad21f91a96adf238fa418cab80cbba80f3b83daa48aa806c2c2fcc53bc62e1c1 |
| m2-defaultoff-fix-test-closure-review-amendment-33.md | 186 | 12682 | a9e5ba80141761fd24f52455e388be171d66764291e23bf5ff584e5d2b47c8eb |
| m2-defaultoff-fix-test-closure-green-amendment-34.txt | 1200 | 125196 | 812069d5403bec24bae2034baecae0a4c4bc940786973704ca0f862d8668d34a |

A34's single literal focused GREEN began 2026-10-06 23:42:18.740425Z and ended
23:58:05.282848Z, wall 946.542528 seconds including queue. Slot 4 acquired after
396 seconds at 32 GB available; the independent execution cap was respected.
Exit 0: experiments PASS 0.006s, Hub PASS 8.897s. Exact stable census: 35 named
plus 159 subtests = 194 matching RUN/PASS outcomes, no FAIL/SKIP/missing/
unexpected/duplicate outcome. All four captured-A subcases and recovered-loop
pre-cleanup case PASS. The caught subscription panic is expected fixture output.
No setup/compile/VCS/resource/timeout/deadlock/watchdog/unrecovered-panic failure.

Literal command remains sealed in A34 raw evidence: HEAVY_BUILD_MAX_WAIT=2700
normal wrapper (45m queue), acquire-time available >=30 GB, ulimit -v 12000000,
GOMEMLIMIT=6GiB, GOGC=40, GOFLAGS=-gcflags=-c=1,
GOCACHE=/scion-volumes/gocache, independent timeout 15m, go test -timeout 14m
-count=1 -p 1 -v ./pkg/experiments ./pkg/hub with unchanged literal 35-name
selector. No outer timeout, retry, resource bypass or additional Go invocation.
It began before the broker pause and continued alone under the explicit
running-work exception; no new create/build/test was started after the pause.

A34 accepted one disclosed nonsemantic reading-order exception for A33 only:
all four required input identities matched before edits, security contained no
corrective direction, its complete body was read before delivery, and the exact
patch was unchanged afterward. The late security-body read was disclosed and
recorded in the validation evidence; this exception is not generalized or repeated.

Production files remain unchanged. NEW remains structurally unadmitted and the
registered experiment remains default false, including fresh true override.
All earlier hard pre-activation gates remain rejecting: ratified census/manifest,
T1–T25/SCC/provenance/constructor authority, clean build/live profile and exact
binding/capture graph, elapsed clock/timer/scheduler, cooperative store/handler/
originating caller, complete stop/return/cleanup, independent critical warning/
alert/dashboard delivery, production freshness/drain/persistence/no-loss and
explicit activation/cutover approval. Finite GREEN grants no trust, activation,
deployment, cutover, persistence, production timing, alert delivery, legacy
retirement, PR/main merge or explain round 7 claim. Existing #2502 legacy
lifecycle, authorization outcomes and sampling remain separate and unchanged.

Commit/push and fresh independent test re-review remain pending. This log append
is unstaged; the accepted operational_settings_test.go remains the sole staged
path. No all-quality-gates-closed or independent post-closure approval is claimed.
All earlier history is preserved byte-for-byte; protected artifacts stay sealed
outside Git. Human-directed messages/questions from this worker: none. Retain
and block pending exact log acceptance, durability and explicit lifecycle release.

## 2026-10-07 — isolated publication integration closeout (append acceptance pending)

Worker audit-update-m2-defaultoff-publish-dev-1; manager audit-update-em.
This section records the isolated publication preparation checkpoint. All prior
419 lines / 27,053 bytes remain an identical historical prefix, SHA-256
9178af154ba4a32ba8f82e9a35a7135beaea63a7b61b56c66547ace5c5e152e7.
Historical staging/push/re-review wording above describes its original phases;
the current state is recorded below without rewriting those checkpoints.

Immutable main integration base: d8b2b692b1ff9c69812838ae67c1d052ab180b22.
Selected-main #2502 writer/emitter substrate:
48c3666ac5c39682f066b3473f0c3b17c12f73de.
Accepted finite slice/correction provenance chain:
591e24c340fa9b0f68102a11c0c649a98749c58f ->
5ea96416729ccd739d3a4bbd9809026d9498001a ->
2d11b29bbdefdff419b76d7d245ca7c4419759fb ->
6d6e4de4598c82f0579fb0c566ea422c7e997b29.
The local publication branch is scion/audit-update-m2-defaultoff-publish.

Only the approved eleven code/test deltas and exact final historical log were
extracted onto pinned main, exactly twelve paths. The whole M2 branch, its
34-path historical merge payload and 448-path whole-branch delta are excluded.
Seven historical operationId additions in audit_authz_test.go were excluded;
only the two approved router tests were added. The three absent historical
TestAuthzConflictResolution_OperationFreeBearerInputs,
TestAuthzConflictResolution_FaultAuditReasons and
TestAuthzConflictResolution_UATDelegationBoundaryAndFault tests were not imported
or selected, and their historical outcomes are not claimed on current main.

Current-main health response shape, stall_config omission, plugin broker
exclusion/pagination and server context are retained. The exact approved summary
warning test was inserted before main's plugin tests, relocating only a blank
separator around its unchanged body. Approved health/server hooks were applied
with context offsets; no newly authored production adaptation or API bridge.
Selected #2502 legacy writer/emitter ownership, sampling/authorization results,
HTTP drain and separate legacy close/deferred-close behavior remain intact.

Stage-A protected packet, under
/scion-volumes/scratchpad/projects/audit-update/reviews/:
- m2-defaultoff-publication-integration-stage-a.patch: 3,437 lines / 152,715 bytes;
  SHA-256 d6c11d0d91e0b5ef5b1992f3664779c7eb1175024ac8d91d372295983b4200bf.
- m2-defaultoff-publication-integration-stage-a-review.md: 358 lines / 24,597 bytes;
  SHA-256 75d24b3f79a4c67c1cba393e5518cfd9d093c839428779ade3ec0df8cae6f52e.
Stage A accounted for twelve postimages, 3,136 additions / 8 deletions. Eight
postimages, including the historical log, equal exact final approved blobs;
four retain main context. Exact approved-hunk reversal restores main bytes;
all 32 other historical paths remain unchanged or absent on both sides.
Scoped gofmt changed zero bytes; diff-check and protected-patch checks passed.
The Stage-A packet remains sealed as the pre-append checkpoint, not this append.

The private shallow clone initially lacked five literal provenance objects.
Amendment 1 authorized exactly five one-shot depth-1 SHA fetches, in order:
48c3666, 591e24c, 5ea9641, 2d11b29, 6d6e4de (full identities above).
All succeeded; only object storage, FETCH_HEAD and inherent shallow-boundary
bookkeeping changed. Main/task refs, origin/config, index and worktree stayed
unchanged during provisioning. Literal commit identities and raw ordered/sole
parents were verified. Main remained shallow, so local ancestry checking was
unavailable (exit 1); no deepen, retry or shallow repair occurred. Amendment 2
accepted the manager's successful immutable ancestry proof sealed in
m2-defaultoff-publication-footprint.md, SHA-256
6ab094d2d748de0b44851d6bf29daf84ef9e6efed6a84f97778a66cd7270b0a6:
selected-main is an ancestor of pinned main and their merge base is selected-main.
The narrow reviewer accepted that sealed provenance; it fetched no historical
objects and did not claim independent local historical-object availability.

Current-main Amendment-3 focused GREEN ran exactly once, without retry/repair.
Raw m2-defaultoff-publication-integration-green-amendment-3.txt:
1,180 lines / 112,895 bytes; SHA-256
6b828a445384dd2947be91c4e2e2f9acd8dc0f81d7c6237a813541c21e4748ab.
Literal command: 2,152 bytes including final newline; SHA-256
e14a4284292e1d089eb64fad144bfa5a48021d83390f477fc44026b4e518a438.
It selected 32 extant historical finite/#2502 names plus current-main
TestHandleHealthSummary_ResponseShape,
TestHealthStats_ConnectedBrokersExcludesPlugins and
TestHealthStats_ConnectedBrokersMultiPage. Exactly 35 named + 143 subtests =
178 unique RUNs and 178 matching PASS outcomes; both packages succeeded:
experiments 0.005s, hub 3.397s. No FAIL/SKIP/missing/unexpected/duplicate outcome
or compile/setup/VCS/resource/timeout/deadlock/unrecovered-panic failure.
Expected malformed JSON and recovered finite subscription panic logs remain
passing negative-fixture output. Dependency downloads occurred inside this run.

UTC start 2026-10-07T00:38:16.046056+00:00;
end 2026-10-07T00:48:49.351200+00:00; wall 633.305144533 seconds; exit 0.
Normal heavy-build slot 4 acquired after 0 seconds, available memory 45 GB.
Controls: maximum queue 2,700s, acquire-time available >=30 GB, virtual-memory
limit 12,000,000 KiB, GOMEMLIMIT=6GiB, GOGC=40, GOFLAGS=-gcflags=-c=1,
shared GOCACHE, independent 15m process / 14m test caps, count 1 / p 1 / verbose,
only pkg/experiments and pkg/hub. No second gate or full CI/race execution.
Historical A34 above remains separate evidence: 35 named + 159 subtests =194
outcomes, not the current-main integration invocation or its selector.

Fresh finite test re-review APPROVE:
m2-defaultoff-fix-test-review-2.md, 379 lines / 23,574 bytes, SHA-256
124d9f55fb74e939b6cfe89ef77d8460ba011253718c3db152ebd05d9fdd66e0.
Its scope is finite R1/R2 causal test-closure adequacy at the approved checkpoint.
Narrow conflict review APPROVE:
m2-defaultoff-publication-conflict-review-1.md, 455 lines / 29,528 bytes, SHA-256
752dd98fb8055fdd63cf162ea85a2c9dc55e932874a016846341f35e533abf48.
Its scope is retention of this exact isolated Stage A and truthful log closeout;
no actionable Critical/High/Medium/Low finding. Finite fixture shortcuts and
failure-path cleanup limits remain disclosed; no code/test edit was required.
Neither approval supplies a new full-M2/code/security/full-CI/race approval or
production scheduling, trust, persistence or activation evidence.

Current closeout state: the accepted twelve-path Stage-A packet remains staged;
only this log append is unstaged, pending exact manager acceptance. HEAD/task
branch/local main/tracking/live main remain the immutable integration base.
No publication commit or push has occurred; commit and push remain pending.
No future SHA, remote durability, compare link, PR, merge queue or publication
is claimed. Protected evidence is retained outside Git; no task build remains.

Production NEW remains registered default false and structurally unadmitted,
including fresh true override. All production trust/census/manifest and
T1-T25/SCC/provenance/constructor, clean live build/profile/binding/capture,
elapsed clock/epoch/timer/scheduler, cooperative store/handler/originating caller,
complete stop/return/reference cleanup, independent warning/alert/dashboard,
freshness/drain/persistence/no-loss and explicit activation gates remain open
and rejecting. No deploy, activation, cutover, legacy retirement or explain
round 7. Finite fixture positives approve none of those production contracts.
Human-directed questions/messages from this publication worker: NONE; manager
exchanges are agent-directed, with no inference about other workers' inventories.
Retain and BLOCK for exact append acceptance and durability disposition; no
completion or deletion authorized at this checkpoint.
