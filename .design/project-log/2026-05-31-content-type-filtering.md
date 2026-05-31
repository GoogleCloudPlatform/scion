# Content-Type Filtering for Harness Observability

**Date:** 2026-05-31
**Issue:** #28
**Branch:** scion/dev-issue-28

## Summary

Implemented content-type filtering to prevent thinking/reasoning content from leaking into user-facing chat messages. The solution follows the design doc's recommended approach (Option A + C): filter at the source in the ClaudeDialect, and add content classification to the messaging pipeline.

## Root Cause

The `last_assistant_message` field from Claude Code's Stop hook was treated as an opaque string. When Claude Code includes extended thinking/reasoning content in this field, it passes through the entire pipeline (ClaudeDialect → HubHandler → Hub API → broker → chat app) with zero filtering. No layer in the pipeline inspected or classified the content.

## Changes

### Content Classification Types (`pkg/sciontool/hooks/types.go`)
- Added `ContentBlock` struct with `Type` field (text, thinking, tool_use, tool_result, error)
- Added `AssistantContent` struct with `Blocks []ContentBlock` to hold classified content
- Added `TextOnly()` method that returns only user-visible text blocks
- Added `HasThinking()` method for downstream classification checks
- Added `AssistantContent` field to `EventData` alongside existing `AssistantText`

### Content-Type Filtering in ClaudeDialect (`pkg/sciontool/hooks/dialects/claude.go`)
- New `extractAssistantContentFromPayload()` handles three input formats:
  - Plain string: treated as a single text block
  - JSON-encoded array: parsed and classified by block type
  - Native array (`[]interface{}`): classified by block type
- `classifyContentBlock()` maps raw content blocks to typed ContentBlocks
- `extractFinalAssistantContentFromTranscript()` replaces the text-only transcript path with a content-type-aware version
- `AssistantText` now contains only filtered text (thinking stripped)
- `AssistantContent` preserves the full typed structure for verbose consumers

### Visibility Tagging (`pkg/messages/types.go`)
- Added `Visibility` field to `StructuredMessage` (normal, verbose, full)
- `normal`: explicit agent→user messages (scion message, ask_user)
- `verbose`: automatic assistant replies from hooks (thinking filtered)
- `full`: everything including thinking traces (for ACP/debugging)

### Hub Handler Updates (`pkg/sciontool/hooks/handlers/hub.go`)
- Outbound assistant-reply messages tagged with `visibility: "verbose"`
- Content classification metadata added: `source=hook`, `has_thinking` flag

### Message Pipeline (`pkg/hub/handlers.go`, `pkg/sciontool/hub/client.go`)
- Added `Visibility` and `Metadata` fields to both `OutboundMessage` (client) and `OutboundMessageRequest` (server)
- Visibility and metadata propagated through to `StructuredMessage` for downstream consumers

## Design Decisions

1. **Filter at source, not at sink**: Filtering in the ClaudeDialect (earliest layer) prevents thinking content from propagating through any downstream handler. This is more robust than filtering at each consumer.

2. **Preserve structure for verbose consumers**: The full `AssistantContent` with typed blocks is preserved alongside the filtered `AssistantText`. This enables future verbose/debug views without re-parsing.

3. **Backward compatible**: Plain string `last_assistant_message` without content blocks works exactly as before — treated as a single text block. The transcript fallback path is also preserved.

4. **Visibility as a message-level concern**: Rather than filtering at the broker topic level, visibility is a field on the message itself. This lets each consumer decide its own fidelity level.

## Test Coverage

- Content block parsing: plain strings, JSON arrays, native arrays, invalid JSON, arrays without type fields
- Thinking filtering: thinking-only, mixed blocks, multiple text blocks
- AssistantContent methods: TextOnly(), HasThinking() with nil/empty
- Transcript classification with thinking blocks
- Backward compatibility: all existing behavior preserved
- HubHandler: verbose visibility tag, has_thinking metadata
