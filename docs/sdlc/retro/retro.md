# Retrospective — tempconv CLI

**Date:** 2026-06-09
**Branch:** feat/sdlc-stage1-test
**PR:** #1 (elukewalker/scion)

## Pipeline Metrics

| Metric | Value |
|--------|-------|
| Total commits | 7 |
| Pipeline duration | ~43 minutes (17:19–18:02 UTC) |
| Agents involved | 5 (planner-2, coder-1, coder-2, reviewer-1, reviewer-2, shipper-1) |
| Files changed | 12 (3 source + 1 test + 8 docs/config) |
| Lines added | 600 |
| Tests | 22 pass, 0 fail |
| Coverage | 84% |
| Review iterations | 2 (1 LOOP_BACK + 1 PR_READY) |
| P0/P1 defects shipped | 0 |
| Informational findings | 6 (all non-blocking) |

## What Went Well

1. **Planner produced implementation-ready code.** The planner (planner-2) delivered working Go code with tests in a single commit. The coder (coder-1) confirmed the implementation needed zero changes during self-review — an ideal outcome.

2. **Review caught real defects.** Reviewer-1 identified 4 genuine issues (negative CLI parsing, NaN/Inf validation, missing AC-008 test, no CLI integration tests). All were legitimate gaps that would have caused user-facing failures.

3. **Fast defect resolution.** Coder-2 fixed all 4 defects in a single commit (ff9fbcc), and reviewer-2 confirmed all fixes in one pass — no second loop-back needed.

4. **Hub-and-spoke conversion pattern.** Routing all conversions through Celsius reduced the implementation from 6 bidirectional formulas to 4 unidirectional ones, making the code easier to verify.

5. **Structured SDLC artifacts.** The plan/code/review doc trail made each pipeline stage's input/output clear, reducing ambiguity for downstream agents.

## What Caused the Loop-Back

The single LOOP_BACK (reviewer-1 → coder-2) was caused by 4 defects:

| # | Defect | Root Cause |
|---|--------|------------|
| 1 | Negative values parsed as flags (`-40` → unknown flag) | Go's `flag` package treats `-` prefix as flag marker; planner didn't account for this |
| 2 | NaN/Inf accepted as valid input | `strconv.ParseFloat` succeeds on "NaN"/"Inf"; no post-parse validation |
| 3 | No AC-008 test (missing value error) | Coder self-review didn't trace AC coverage exhaustively |
| 4 | No CLI integration tests | Unit tests covered logic but not the argument-parsing layer |

**Pattern:** Defects 1–2 are edge cases in Go's stdlib behavior. Defect 3–4 are coverage gaps the coder could have caught with an AC-to-test traceability check.

## Patterns to Reinforce

- **AC traceability table in review verdicts.** The reviewer's AC-to-test mapping (verdict.md lines 20–31) made verification unambiguous. Every future review should include this.
- **Separate unit + CLI integration test layers.** The fix added `runCLI` helper tests that exercise the full `run()` path — this caught the `preprocessArgs` bug that unit tests missed.
- **Single-commit defect fixes.** Bundling all 4 fixes in one commit kept the loop-back to exactly one iteration.

## Patterns to Avoid

- **Skipping CLI-level tests for CLI tools.** The planner's initial implementation had only unit-level conversion tests. CLI argument parsing is a distinct failure surface that needs its own test layer.
- **Assuming stdlib handles all edge cases.** Go's `flag` package and `strconv.ParseFloat` have well-known footguns (negative numbers as flags, NaN/Inf parsing). These should be in a "Go CLI gotchas" checklist.

## Efficiency Assessment

- **1 loop-back out of 1 review cycle** is acceptable for a first-run pipeline. The defects were genuine, not stylistic.
- **43-minute wall clock** for plan → code → review → fix → re-review → ship → docs is efficient for 600 lines of production-quality Go.
- **Zero wasted iterations:** no spurious loop-backs, no false-positive findings escalated to blocking severity.
