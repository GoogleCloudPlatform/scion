# DEF-12 Volume Exercise (AC-12-6-LOCAL)

**Date:** 2026-08-27
**Author:** dev-def12-volume
**Branch:** `scion/ca-msg-em6-def12`

## Summary

Ran the conversation backfill pipeline against 50,000 in-memory SQLite messages
to validate correctness and measure wall-clock performance at realistic scale.

## Message Distribution

| Category | Count | Pct | Description |
|---|---|---|---|
| DM ThreadID | 30,000 | 60% | Canonical `dm:<kind>:<uuid>:<kind>:<uuid>` |
| Non-DM ThreadID | 7,500 | 15% | Topic/thread style (`topic-N`) |
| Malformed ThreadID | 5,000 | 10% | Parse failures (`dm:invalid:bad:format`, etc.) |
| Empty ThreadID | 5,000 | 10% | Legacy — key derived from sender/recipient |
| Pre-backfilled | 2,500 | 5% | ConversationID already set |
| **Total** | **50,000** | | 20 distinct sender/recipient pairs |

## Timing Results

| Phase | Wall Time | Detail |
|---|---|---|
| Seed | 2.98s | 50,000 message inserts |
| Phase 1 (dry-run) | 46.67s | 50,000 processed, 70 conversations would be created |
| Phase 2 (execute) | 48.24s | 70 conversations created, 42,500 attributed |
| Phase 3 (re-execute) | 47.75s | 45,000 skipped, 5,000 errors (malformed) |
| **Total backfill** | **2m 22.7s** | |

## Key Observations

1. **Correctness verified** — all three phases pass assertions:
   - Dry-run produces no mutations (verified by sampling)
   - Execute correctly stamps 42,500 messages and creates 70 conversations
   - Re-execute is fully idempotent: 0 attributed, 0 new conversations
2. **Error handling** — all 5,000 malformed messages produce errors without crashing;
   they are excluded from the attribution count and skipped on re-run
3. **Idempotency invariant** — `Attributed + Inferred + Skipped + len(Errors) == TotalProcessed`
   holds across all phases
4. **Performance** — ~47s per phase at batch size 1000 on in-memory SQLite.
   Each phase scans all 50k messages; the dominant cost is ListMessages pagination.

## Test File

`cmd/server_backfill_volume_test.go` — guarded by `//go:build volume_test && !no_sqlite`.

Run with:
```bash
go test ./cmd/... -run TestBackfillVolume -tags volume_test -v -count=1 -timeout 300s
```
