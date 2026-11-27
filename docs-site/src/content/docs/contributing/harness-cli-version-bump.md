---
title: Bumping Usage-Verified Harness CLI Versions
description: How to upgrade agy, grok, and muse CLI versions while keeping usage decoding, golden fixtures, and harness images aligned.
---

Three harness CLIs — **Antigravity (`agy`)**, **Grok Build (`grok`)**, and **Muse Code (`muse`)** — have usage decoders or normalizers whose wire schema and token semantics are verified against live captures of specific CLI releases. Upgrading one of these CLIs without refreshing its fixtures and re-verifying decoding can silently corrupt or drop token telemetry.

## Verified Versions and Source-of-Truth Records

| Harness | CLI | Verified Version | Go Verified-Version Record | Fixtures & Tests | Image Recipe |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `antigravity` | `agy` | `1.5.18` | `pkg/trajectory/agydecode/version.go` (`VerifiedVersions`) | `pkg/trajectory/agydecode/testdata/` (`decoder_test.go`, `fixture_test.go`) | `harnesses/antigravity/Dockerfile` |
| `grok-build` | `grok` | `1.0.92` | `pkg/sciontool/telemetry/usage.go` (`GrokBuildVerifiedVersions`) | `pkg/sciontool/telemetry/testdata/usage/grok-build-1.0.92.pb.json` (`usage_grok_test.go`) | `harnesses/grok-build/Dockerfile` (`ARG GROK_VERSION`) |
| `muse-code` | `muse` | `1.4.25` (`1.4.25-R9361.1`) | `pkg/sciontool/hooks/handlers/muse_turn_delta.go` (`MuseCodeVerifiedVersions`) | `pkg/sciontool/hooks/dialects/testdata/muse-code/session-usage-1.4.25.jsonl` (`muse_dialect_test.go`) | `harnesses/muse-code/Dockerfile` |

## What Degradation Looks Like If a Bump Is Skipped

Each harness fails differently when a newer CLI diverges from the verified version:

- **Antigravity (`agy`):** `agydecode.DecodeFile` checks the detected `agy --version` against `agydecode.VerifiedVersions`. Any unlisted version is marked `StatusUnverified`, which suppresses token attribution and degrades Hub usage reporting for those agents to `calls_only` (only `PreInvocation`/`PostInvocation` hook call counts remain). If the SQLite schema (`gen_metadata`, `steps`, `trajectory_metadata_blob`) or protobuf field tags change, decoding may also report `StatusMalformed`.
- **Grok Build (`grok`):** `grokBuildUsageRule` expects `grok_code.api_request` / `grok_code.api_error` OTLP log events where `input_tokens` is inclusive of `cache_read_tokens` (and assumed inclusive of `cache_creation_tokens`, unverified live on `1.0.92`) and `output_tokens` is exclusive of `reasoning_tokens`. If a newer `grok` release renames event names, changes attribute keys, or flips inclusivity (for example making `output_tokens` inclusive of `reasoning_tokens` or `input_tokens` exclusive of `cache_read_tokens`), token counts silently double-count, under-count, or drop to zero.
- **Muse Code (`muse`):** `normalizeMuseTurnUsage` subtracts a monotonic per-`(session_id, turn_id)` high-water mark because `muse 1.4.25` emits turn-cumulative totals in `PostLLMCall.usage`. If a newer `muse` release switches `PostLLMCall.usage` to per-step counts, high-water subtraction silently under-counts steps after step 0 (dropping any step whose token counts do not exceed the prior step).

## Bump Process

Follow these steps whenever moving one of these CLIs to a newer release:

### 1. Build or Install the Candidate CLI Version

- **Grok Build:** pass `--build-arg GROK_VERSION=<new-version>` (or `=latest`) when building `harnesses/grok-build/Dockerfile`, or install directly in a throwaway environment via `https://x.ai/cli/install.sh` with `<new-version>` as the positional argument.
- **Antigravity:** build `harnesses/antigravity/Dockerfile` (with `--build-arg AGY_VERSION=<new-version>` if pinned, or via `https://antigravity.google/cli/install.sh`) and record the exact `agy --version` output.
- **Muse Code:** build `harnesses/muse-code/Dockerfile` (with `--build-arg MUSE_VERSION=<new-version>` if pinned, or via `https://dev.meta.ai/install.sh`) and record the exact `muse --version` output.

### 2. Capture and Scrub Fresh Usage Fixtures

Capture live outputs against a local mock or test endpoint (scrubbing host paths, UUIDs, timestamps, and any credentials):

- **Antigravity (`agy`):** inspect a real conversation SQLite database (`gen_metadata`, `steps`, `trajectory_metadata_blob`) from the new `agy` release and confirm the protobuf wire layout matches `pkg/trajectory/agydecode/testdata/README.md`. Update `pkg/trajectory/agydecode/fixture_test.go` if wire fields changed, then regenerate goldens:
  ```bash
  UPDATE_GOLDEN=1 go test -buildvcs=false ./pkg/trajectory/agydecode/...
  ```
- **Grok Build (`grok`):** run a multi-step tool-calling turn plus a terminal error turn (e.g. invalid model HTTP 404) with `GROK_EXTERNAL_OTEL=true` exporting to a local OTLP/gRPC receiver. Verify `grok_code.api_request` and `grok_code.api_error` event names, `input_tokens` / `cache_read_tokens` / `cache_creation_tokens` inclusivity, and `output_tokens` / `reasoning_tokens` exclusivity. Save the scrubbed capture as `pkg/sciontool/telemetry/testdata/usage/grok-build-<version>.pb.json` and update `grokBuildUsageFixturePath` in `pkg/sciontool/telemetry/usage_grok_test.go`.
- **Muse Code (`muse`):** run the four scenarios documented in `pkg/sciontool/hooks/dialects/testdata/muse-code/README.md` (single call with cache+reasoning, multi-step tool loop on one `turn_id`, failed step 1 after successful step 0, and subagent call) against a local mock endpoint. Confirm whether `PostLLMCall.usage` is still turn-cumulative or has switched to per-step. Save the scrubbed capture as `pkg/sciontool/hooks/dialects/testdata/muse-code/session-usage-<version>.jsonl`, update `README.md`, and update `loadMuseUsagePayloads` in `pkg/sciontool/hooks/handlers/muse_dialect_test.go`.

### 3. Update the Verified-Version Record and Image Pin

1. Add (or update) the new version in the corresponding Go slice:
   - `pkg/trajectory/agydecode/version.go` (`VerifiedVersions`)
   - `pkg/sciontool/telemetry/usage.go` (`GrokBuildVerifiedVersions`)
   - `pkg/sciontool/hooks/handlers/muse_turn_delta.go` (`MuseCodeVerifiedVersions`)
2. Update the matching `ARG` default in `harnesses/<name>/Dockerfile` (when pinned) and any version notes in `harnesses/<name>/usage.yaml` and `harnesses/<name>/README.md`.

### 4. Re-Verify Tests and Contract Guards

Run the decoder tests and Dockerfile contract guards:

```bash
go test -buildvcs=false ./pkg/trajectory/agydecode/...
go test -buildvcs=false ./pkg/sciontool/telemetry/...
go test -buildvcs=false ./pkg/sciontool/hooks/handlers/...
go test -buildvcs=false -tags no_sqlite -run 'Test.*Dockerfile' ./cmd
```

If decoder logic changed in `sciontool`, rebuild `scion-base` first and then the affected harness images (`image-build/README.md`, "Build provenance and stale sciontool").
