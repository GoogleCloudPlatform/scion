---
name: independent-review
description: >-
  Run an independent complementary review of another agent's work before acceptance.
  Use when an orchestrator needs a distinct reviewer to inspect work directly, challenge
  assumptions, verify requirements, or close material findings back into the work loop.
---

# Independent Review Protocol

Use this protocol when work should be checked by an agent other than the agent that produced it.
The goal is not another summary of the worker's reasoning. The reviewer independently examines
primary evidence and reports findings to the orchestrator before the work is accepted.

## Roles

A review cycle has three roles:

- **Producer** — the agent that performed the work being reviewed.
- **Reviewer** — a distinct agent created for the review. It must not be the producer.
- **Orchestrator** — the agent that requests the review, receives the findings, and decides what
  happens next.

The reviewer may use the same harness or model as the producer, but using a different model,
harness, or review specialization is useful when available. Organizational independence comes
from using a distinct agent identity and evidence path, not from model diversity alone.

## When to Review

The orchestrator should consider independent review before accepting work when any of these apply:

- the result changes important code, configuration, data, or external behavior;
- requirements are ambiguous or several interpretations are plausible;
- the producer reports uncertainty, a workaround, or an unresolved risk;
- multiple workers produced conflicting conclusions;
- correctness depends on details that are easy to miss in a normal status summary;
- the cost of accepting a hidden defect is materially higher than the cost of a review.

Do not create reviewers mechanically for every trivial task. The orchestrator owns the decision to
invoke review based on the work and its risk.

## Independence Requirements

A complementary review is independent only when all of the following hold:

1. **Distinct actor** — create a separate reviewer agent. The producer must not review its own work.
2. **Direct evidence** — give the reviewer a direct path to the artifact, branch/commit, test output,
   logs, or other primary evidence. Do not ask it to judge only from the producer's summary.
3. **No producer filtering** — the producer may identify useful evidence, but it must not decide
   what the reviewer is allowed to inspect or rewrite the evidence into a review packet that hides
   the original artifact.
4. **Independent judgment** — ask the reviewer to reach its own conclusion before seeing the
   producer's defense or proposed disposition of possible findings.
5. **Separate feedback path** — findings go to the orchestrator, not only back to the producer.

A reviewer can be a child of the same orchestrator as the producer. It does not need a separate
human owner; it needs a distinct agent identity and an evidence path that does not depend on the
producer's self-report.

## Supplying Evidence

Use the most direct evidence channel available for the project and workspace mode.

For code or repository work, prefer an immutable commit or otherwise stable branch/ref that the
reviewer can inspect directly. For generated artifacts, reports, datasets, or other files, place a
stable copy in a project shared directory when the reviewer's isolated workspace would not expose
the producer's copy. Scion project shared directories are mounted across agents even when their
normal workspaces are isolated.

Operational evidence may include Scion-visible agent state and logs when those are relevant to the
claim being reviewed.

If the reviewer cannot actually reach the claimed evidence — for example, a clone-per-agent worker
has an unpublished branch and no artifact was placed in a shared directory — the review is not
complete. Publish or otherwise expose the evidence first; do not substitute the producer's summary.

## Reviewer Brief

The review brief should state:

- **Subject** — exactly what result or claim is under review.
- **Acceptance criteria** — the requirements or invariants the result is expected to satisfy.
- **Evidence** — direct paths, refs, artifact locations, logs, or commands the reviewer can inspect.
- **Boundaries** — what is out of scope for this review.
- **Reporting** — the orchestrator that receives findings.

Do not tell the reviewer what conclusion to reach. Avoid leading language such as "confirm this is
correct". Prefer "determine whether this satisfies the acceptance criteria and report any findings."

## Reviewer Behavior

The reviewer should:

1. inspect the supplied primary evidence directly;
2. test or cross-check material claims when practical;
3. look for omissions and contradictions, not only obvious implementation defects;
4. distinguish verified findings from questions or insufficient evidence;
5. avoid modifying the reviewed artifact unless the orchestrator explicitly starts a separate
   repair task after review;
6. report findings to the orchestrator with enough evidence for an independent disposition.

A useful finding contains:

- the affected requirement or invariant;
- the concrete evidence observed;
- why the evidence matters;
- a severity or impact description when useful;
- a recommended next action, without silently performing that action.

If no material finding is found, say what evidence was inspected and what checks were performed.
"Looks good" without an evidence trail is not a completed independent review.

## Closing the Loop

The orchestrator owns the disposition of reviewer findings.

For each material finding, it should choose an explicit next action such as:

- send the finding back to the producer for repair;
- assign repair to a different worker;
- narrow or change the acceptance criteria when the original requirement was wrong or incomplete;
- accept the risk explicitly when the finding is understood and intentionally tolerated.

After a repair that changes the evidence relevant to a material finding, run a focused re-review
before acceptance. The re-review may use the same reviewer or another distinct reviewer.

The review cycle is complete only when:

- the reviewer has inspected direct evidence;
- findings have reached the orchestrator;
- every material finding has an explicit disposition; and
- any required repair has been re-checked before the orchestrator accepts the work.

## Anti-Patterns

These do **not** count as independent review:

- asking the producer to "double-check" its own result;
- asking a reviewer to critique only the producer's summary;
- letting the producer choose which original evidence the reviewer may see;
- having the reviewer silently fix the artifact and report success without surfacing findings;
- accepting work while material findings are still undisposed;
- using deterministic tests alone as a substitute for an independent reviewer when the review
  requires judgment about requirements, omissions, trade-offs, or conflicting evidence.
