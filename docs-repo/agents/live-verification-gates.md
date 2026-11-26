# Live verification and hosted-mode hardening gates

For running or evaluating live hosted or workstation verification suites, use the `live-verification-testing` skill (`skills/live-verification-testing/SKILL.md` in `scion-frontiers/scion-repo-contrib`, once provisioned). When a design specifies a numbered real-hub gate or acceptance checklist, aim to cover every numbered scenario on the initial gate run (and compare gate output against the design's acceptance criteria during review) to avoid a second pass over omitted scenarios.

When a PR hardens hosted or production-like modes by refusing convenience behavior that remains legitimate in workstation mode, apply these seven gate checks before merge:

1. **Consumer inventory before merge:** grep flags and environment variables across test harnesses, scripts (such as `hack/workstation-smoke.sh`), and CI workflows to list every consumer that depends on the removed convenience.
2. **One designated test-hub mode:** start test hubs through a single explicitly gated path (class marker, pinned hub ID, enabled only via CLI flag—never settings or environment variables), with table-driven tests proving it is allowed only when all conditions hold and refused when any single condition fails.
3. **Failing-before, passing-after test per finding:** each fix commit carries a test that fails on the previous head and passes on the fix.
4. **Whole-surface phase and consolidation gates:** avoid per-phase skip lists that allow regressions in adjacent packages (for example `./cmd`) to slip through, and re-run the full gate set on the consolidated head to catch interaction failures.
5. **Explicit local-versus-CI gap disclosure:** list every suite not run locally and require it green in CI before merge; give scripts outside CI a smoke run or name them explicitly as unverified.
6. **Adversarial fix verification for name and normalization checks:** after a second failure on an incremental denylist or patch approach against case folding, Unicode, or filesystem quirks, switch to an allowlist-style rule and have a fresh verifier attack it.
7. **Blind review brief consumer check:** ask the blind reviewer to enumerate consumers of every removed capability independently.
