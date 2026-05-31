# Fix: scion resume creates new agents instead of resuming stopped ones

**Date:** 2026-05-31
**Issue:** #61
**PR:** #85
**Agent:** dev-issue-61

## Problem

When using `scion resume <agent-name>` on agents stopped with `scion stop`, the Hub server destroyed the existing agent record and created a brand new one. This caused:
- New agent ID assigned (old one deleted)
- Template association lost (blank template column)
- Fresh container uptime (newly created container)

## Root Cause

The Hub server's `CreateAgentRequest` struct in `pkg/hub/handlers.go` was missing the `Resume` field that the CLI client sends via `pkg/hubclient/agents.go`. The Hub therefore ignored the resume intent entirely.

In `handleExistingAgent()`, stopped agents always fell through to the "stale cleanup" path which deletes the agent from the database and recreates it from scratch. Only suspended agents had the in-place restart path.

## Fix

1. Added `Resume bool` field to the Hub's `CreateAgentRequest` struct
2. Added a new condition block in `handleExistingAgent` that checks `req.Resume && existingAgent.Phase == PhaseStopped` and handles it like the suspended path: restart in-place via `DispatchAgentStart`, preserving the agent record
3. The existing delete+recreate behavior for `scion start` (without Resume flag) is preserved unchanged

## Tests Added

- `TestCreateAgent_ResumeFromStoppedStatus` — resume returns 200 with preserved agent ID
- `TestCreateAgent_StartFromStoppedStatus_NoResume` — start still recreates with 201
- `TestCreateProjectAgent_ResumeFromStoppedStatus` — project-scoped resume test

## Observations

- The suspended vs stopped distinction is purely at the phase-marker level; both use the same `docker stop` under the hood. The broker already handles starting stopped containers correctly.
- The CLI intentionally sets harness-level resume to false for stopped agents (fresh session), which is correct since stopped harness state is unrecoverable. The fix only affects the Hub-level agent record preservation.
