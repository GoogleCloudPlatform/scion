# tz-refactor task 7: agent.log in UTC, tzdata in agent images, dashboard "(UTC)" labels

**Date:** 2026-10-02
**Branch:** scion/tz-t7
**Issue:** ptone/scion#2500 (part of ptone/scion#2457, design Option A, §2.3, §2.1 item 8, §7, AC13)

## What changed

- **agent.log timestamps.** sciontool wrote `agent.log` lines (and the
  per-service `<name>.lifecycle.log` lines) with the zoneless layout
  `"2006-01-02 15:04:05"` in the agent's `TZ`. All three writers
  (`log.write`, the `/tmp/agent.log` fallback notice in `getLogFileLocked`,
  and `managedService.writeLifecycle`) now call one helper,
  `log.Timestamp(t)`, which returns `t.UTC().Format(time.RFC3339Nano)`. The
  agent process keeps its own `TZ`. No other sciontool writer stamps
  agent.log (grepped `pkg/sciontool`, `cmd/sciontool`, `pkg/harness`); the
  slog bridge goes through `log.write`.
- **tzdata in agent images.** `tzdata` is now in `COMMON_PACKAGES` in
  `image-build/lib/install-core-toolchain.sh` (used by core-base and
  thick-prep). It was already present transitively on Debian trixie
  (`python3` -> `libpython3.13-stdlib` -> `tzdata`); now it is explicit.
  `image-build/lib/verify-base-contract.sh` gains a network-free check that
  `/usr/share/zoneinfo/Asia/Tokyo` exists, that `TZ=Asia/Tokyo date +%z` is
  `+0900`, and that `TZ` is unset in the build environment (an `ENV TZ`
  fails the build; when not building, a set `TZ` is only a note, because a
  running agent container legitimately has one).
- **Dashboard.** The metrics dashboard charts get a "Day (UTC)" x-axis title
  and "(UTC)" in the chart headings. Buckets are UTC days: `seriesIncreases`
  takes interval ends from `timestamppb.AsTime()`, which returns UTC, and the
  bucket key is `Format("2006-01-02")` on that value. A new Go test pins that
  with `time.Local` set to Tokyo.

## Test evidence

- `go test -p 2 ./pkg/sciontool/log/ ./pkg/sciontool/services/` and the
  pkg/hub metrics-dashboard tests pass under `TZ=UTC`, `TZ=Asia/Tokyo` and
  `TZ=Asia/Kathmandu`, with the leaked `SCION_*` env stripped.
- Revert check: without the `.UTC()` in `log.Timestamp`, both new log tests
  fail (`"...T09:30:15.123456789+09:00"`, want `"...T00:30:15.123456789Z"`).
- Web: `metrics-dashboard.test.ts` passes under `TZ=Asia/Tokyo` and
  `TZ=Asia/Kathmandu`, and fails when the labels are reverted;
  `npm run typecheck` and `npm run build` pass.
- The docker smoke check is left for the maintainer (no docker here):
  `docker run --rm <image> ls /usr/share/zoneinfo` and
  `docker run --rm -e TZ=Asia/Tokyo <image> date`.

## Follow-ups

- `harnesses/claude/Dockerfile` has `ARG TZ` / `ENV TZ="$TZ"` (a devcontainer
  leftover). No build passes the arg, so the image ships `TZ=""`, which glibc
  and Go treat as UTC, and a runtime `-e TZ` overrides it. It is a harness
  image, not a core image, so it is out of scope here. It is relevant to
  tz-refactor task 15 (hub authority for agent `TZ`) and should be removed
  there or separately. The new contract check runs only on core-base and
  thick-prep, so it does not see harness images.
