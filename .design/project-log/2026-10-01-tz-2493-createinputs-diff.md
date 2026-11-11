# CreateInputs Diff for PATCH /api/v1/agents/{id} (Option C)

**Date:** 2026-10-01
**Branch:** scion/tz-2493
**Fork PR:** ptone/scion#2527
**Design:** gs://scion-xproject-exchange/tz-refactor/2493/options.md §5 (Option C, recommended and chosen by ptone 2026-10-01 14:25Z)

## Problem

`applyAgentUpdate` (PATCH `/api/v1/agents/{id}`) writes `AppliedConfig.{Image,Model,ThinkingLevel,Task,HarnessAuth,Env,InlineConfig}` directly but never touches `AppliedConfig.CreateInputs`. `scion reincarnate` rebuilds a fresh config from `CreateInputs`, not from the live `AppliedConfig`, so any edit made on the configure page after create — change the model, add an env var, set a system prompt — was silently lost on the next reincarnate. The configure page always PATCHes the full, live, derived config back on every Save and every Start, so the PATCH body alone cannot distinguish "the user changed this" from "the page echoed what was already there."

## Solution: Option C (diff-gated write into CreateInputs)

`CreateInputs` now means "explicit inputs: the create request, plus any later PATCH edit that changed a field's live value" (invariant E). A new helper, `recordExplicitEdits` (`pkg/hub/applied_config_explicit_edits.go`), is called in `applyAgentUpdate` right before the live `agent.AppliedConfig.*` writes. It takes a snapshot of the live config (`old`) taken before any of those writes, and is a no-op when `AppliedConfig.CreateInputs` is nil (an agent that predates the field, or was never captured — the reincarnate fallback already reads the live config directly for those).

- **Image / Model:** compared against `old.Image`/`old.Model` in canonical (registry-qualified, alias-resolved) form, so an echo is never a diff. `""` means "unchanged", matching the live write's own semantics.
- **ThinkingLevel / HarnessAuth:** `ThinkingLevel` compares `nil` as a real value (the live write always applies it); `HarnessAuth` treats `""` as "unchanged", same as the live write. Both are written to their `AgentCreateInputs` top-level field AND mirrored into `CreateInputs.InlineConfig`, since `buildFreshAppliedConfig` reads the top-level fields, not the InlineConfig mirror, but the mirror keeps InlineConfig internally consistent.
- **Env, per key:** a key added or changed against the live `old.Env` is set in `CreateInputs.InlineConfig.Env`; a key the live env had that the request no longer has is deleted from it. Skipped entirely when the request's `env` key is absent (same guard as the live write).
- **Every other `ScionConfig` field** (system_prompt, agent_instructions, user, branch, max_turns, resources, ...): compared field-by-field against `old.InlineConfig` via reflection over the JSON struct tags, for **present keys only** — decoded from the raw request body's `config` object, not from the already-decoded `*api.ScionConfig` (every field there is `omitempty`, so an omitted key and an explicit zero value are otherwise indistinguishable). A present, changed value — empty included — is recorded; an absent key is never treated as cleared. **Excluded:** `task` (CreateInputs excludes it by design), and `harness`/`harness_config`/`default_harness_config` (an unvalidated harness switch must not take effect only at reincarnate).
- `CreateInputs.InlineConfig` is allocated lazily, only once at least one field actually changed, so an all-echo PATCH leaves a nil `CreateInputs.InlineConfig` nil rather than turning it into a non-nil empty struct.

A seam is marked at the call site and in `recordExplicitEdits`' doc comment for ptone/scion#2457 task #16: that task's future PATCH `config.env["TZ"]` strip (I2) must run between the `old` snapshot and the `recordExplicitEdits` call, never after it, or an ignored `TZ` key would be recorded here and replayed by I1(a) as an unrequested pin.

### Web change

`agent-configure.ts`'s `buildConfig` only sent a field when truthy, so clearing an owned field (one this page renders and is the only place that edits) back to empty omitted its key entirely — recorded by the hub as "never touched", not "cleared". `system_prompt`, `agent_instructions`, `user`, `branch`, `max_turns`, `max_model_calls`, and `max_duration` are now sent even when empty (still gated on harness-capability support, so an unsupported field stays omitted). `model`, `image`, `auth_selectedType` and `task` keep the truthy-only guard: they have hub-side "empty means unchanged" semantics independent of this feature, so sending an explicit empty would not clear them anyway, and `resources` keeps its truthy-only guard too (it is a pointer struct on the Go side; always sending an empty `ResourceSpec{}` object would make `reflect.DeepEqual` see a diff against a nil `old.InlineConfig.Resources` on every single save, breaking invariant E for every agent that has never set resources — see Adjacent findings below).

## Files Changed

| File | Change |
|------|--------|
| `pkg/hub/applied_config_explicit_edits.go` (new) | `recordExplicitEdits`, `recordOtherInlineFieldEdits`, `diffExplicitEnvKeys`, `thinkingLevelEqual` |
| `pkg/hub/applied_config_explicit_edits_test.go` (new) | All 8 hub-side test-plan items from options.md §5 |
| `pkg/hub/handlers_agents_core.go` | `applyAgentUpdate` reads the request body into a buffer (for the raw presence map), snapshots `old` before the config writes, resolves the model alias once up front, and calls `recordExplicitEdits` before the live writes |
| `pkg/store/models.go` | Doc comments on `AgentAppliedConfig.CreateInputs` and `AgentCreateInputs` updated for the new "plus later explicit edits" meaning |
| `pkg/hub/reincarnate_config.go` | `buildFreshAppliedConfig`'s doc comment updated to note CreateInputs can now include PATCH-recorded edits |
| `web/src/components/pages/agent-configure.ts` | `buildConfig` sends explicit empty values for cleared owned fields |
| `web/src/components/pages/agent-configure-build-config.test.ts` (new) | Test-plan item 9 |

## Test Evidence

Hub (`pkg/hub`, `//go:build !no_sqlite` — excluded from `make ci`'s `test-fast`, which runs `-tags no_sqlite`; run separately via plain `go test ./pkg/hub/...`):
- `TestApplyAgentUpdate_ExplicitEditsSurviveReincarnate` (item 1), `TestApplyAgentUpdate_EchoPatchLeavesCreateInputsByteIdentical` (item 2), `TestApplyAgentUpdate_TemplateEnvRefreshesAfterEchoPatch` (item 3), `TestApplyAgentUpdate_DeletesEnvKeyAndThinkingLevel` (item 4), `TestApplyAgentUpdate_AbsentVolumesKeptPresentEmptySystemPromptCleared` (item 5), `TestApplyAgentUpdate_SecretNamedEnvKeyStrippedByCleanup` (item 6), `TestApplyAgentUpdate_NilCreateInputsStaysNil` (item 7), `TestApplyAgentUpdate_HarnessConfigNeverReachesCreateInputs` (item 8) — all pass under default TZ and `TZ=Asia/Tokyo`.
- Under `TZ=Asia/Kathmandu`, all 8 fail with the same pre-existing SQLite test-store migration error every `createTestStore`-backed `pkg/hub` test hits on `main` under that TZ (`empty agent role backfill: ... Scan error on column create_time`, confirmed against a clean detached `upstream/main` worktree) — tz-refactor task #2's scope, not introduced here. No test introduced a *different* Kathmandu failure.

Web (Vitest): `agent-configure-build-config.test.ts`, 7 tests (item 9) — pass. Full web suite (`npm run test -- --run`): 3158 tests across 111 files pass. `npm run typecheck`: clean.

Go: `go build -buildvcs=false -p 2 ./...` and `golangci-lint run --new-from-rev=upstream/main --concurrency=1 ./pkg/hub/...` (0 issues) both clean. `GOFLAGS="-buildvcs=false -p=2" make ci` is green **only with the sandbox's leaked `SCION_*`/`CLAUDE_CODE_*`/`ANTHROPIC_*` env vars stripped** — with them present, `pkg/harness`'s `TestNativeTelemetryProvisionedChildEnv` fails on a native-telemetry policy conflict seeded by the leaked `CLAUDE_CODE_ENABLE_TELEMETRY=1`/`SCION_TELEMETRY_*` vars; this is an unrelated, pre-existing environment leak (confirmed by re-running that one test with only the telemetry-related vars stripped), not something this change introduced or fixed.

## Follow-ups (raised, not fixed — options.md §7)

1. `messageMode` is silently dropped by the PATCH body struct (`handlers_agents_core.go`'s `updates` struct has no `messageMode` field), so the configure page's "Default" clear is a no-op.
2. The PATCH replaces `InlineConfig` wholesale; fields the configure page never sends (`volumes`, `mcp_servers`, `services`, `skills`, `secrets`, `hub`, `kubernetes`) are likely dropped from the next start. Not traced through to the broker.
3. The reincarnate dry-run plan does not show inline-only field changes, so a revert can be silent under any CreateInputs option.
4. (New, found while implementing the web change) `agent-configure.ts`'s `resources` tab has the same "cleared owned field is silently dropped" gap as the fields this change fixes, but fixing it safely needs a tombstone or presence-aware comparison on the Go side first (see "Web change" above) — left as its own follow-up rather than widening this change's scope.
