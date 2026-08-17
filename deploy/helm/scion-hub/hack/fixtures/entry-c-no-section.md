<!--
FIXTURE, Entry C, the RENUMBERED / RESTRUCTURED arm. Not a real VALIDATION.md.

A plausible VALIDATION.md that has been reorganised so that section 7.2 no
longer exists under that heading. The evaluator must return CANNOT-EVALUATE
rather than DISCHARGED: the word it looks for is absent, but it is absent
because the evaluator is looking in a document that no longer has the shape it
assumes - not because anybody ran anything.

This is the arm that catches the most likely real-world silent failure, which
is not deletion but renumbering by a later phase.
-->

# Operator validation checklist

## Live checks

#### 8.1 Something a later phase renumbered this section into

The content here is irrelevant. What matters is that no heading matches the
extraction, so the evaluator has no corpus and must say so.
