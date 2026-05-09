# Harness Observability Architecture Analysis

## Status
**Research** | May 2026

## Executive Summary

This document analyzes the three systems that capture and surface harness activity in Scion: the **hook-based event system**, the **message broker integration**, and the **Agent Control Protocol (ACP)**. It identifies the root cause of thinking/reasoning content leaking into chat, documents data loss points in the message pipeline, and proposes an architecture for clean chat integration with content-type filtering.

---

## 1. HOOK-BASED SYSTEM

### Overview

The hook system is the low-level mechanism for capturing harness events inside agent containers and updating agent state in the Hub. It operates as a **unidirectional pipe**: the harness fires lifecycle events → `sciontool hook` normalizes them → handlers dispatch state updates, log entries, telemetry, and outbound messages.

### Hook Configuration

Hooks are configured in the harness's embedded `settings.json`. For Claude Code (`pkg/harness/claude/embeds/settings.json`), **8 hook events** are registered:

| Hook Event | What Triggers It |
|---|---|
| `SessionStart` | Claude Code session begins |
| `SessionEnd` | Claude Code session ends |
| `UserPromptSubmit` | User submits a prompt |
| `Stop` | Agent turn completes (main agent) |
| `SubagentStop` | Subagent turn completes |
| `PreToolUse` | Before a tool executes |
| `PostToolUse` | After a tool executes |
| `Notification` (matcher: `ToolPermission`) | Tool permission requested |

All hooks invoke the same command: `sciontool hook --dialect=claude`, receiving the hook payload as JSON via stdin.

### Event Normalization Pipeline

```
Claude Code fires hook event
    │
    ▼
JSON payload via stdin to: sciontool hook --dialect=claude
    │
    ▼
HarnessProcessor.ProcessFromStdin()          [pkg/sciontool/hooks/harness.go]
    │
    ▼
ClaudeDialect.Parse(data)                    [pkg/sciontool/hooks/dialects/claude.go]
    │   Maps raw event names → normalized names
    │   Extracts: prompt, tool_name, message, tokens, session_id, etc.
    │   For Stop/SubagentStop: extracts AssistantText (see below)
    │
    ▼
Normalized hooks.Event struct                [pkg/sciontool/hooks/types.go]
    │
    ▼
Handler chain (called sequentially):
    ├── StatusHandler   → writes ~/agent-info.json (local state)
    ├── LoggingHandler  → appends to agent log file
    ├── PromptHandler   → saves first user prompt to ~/prompt.md
    ├── HubHandler      → HTTP POST to Hub API (status + outbound messages)
    ├── TelemetryHandler→ OTLP spans for observability
    └── LimitsHandler   → enforces turn/model call limits
```

### Normalized Event Types

The dialect parser maps harness-specific event names to a normalized vocabulary (`pkg/sciontool/hooks/types.go`):

| Normalized Event | Claude Raw Name | Gemini Raw Name |
|---|---|---|
| `session-start` | SessionStart | SessionStart |
| `session-end` | SessionEnd | SessionEnd |
| `prompt-submit` | UserPromptSubmit | — |
| `tool-start` | PreToolUse | BeforeTool |
| `tool-end` | PostToolUse | AfterTool |
| `agent-end` | Stop | AfterAgent |
| `subagent-end` | SubagentStop | — |
| `model-start` | BeforeModel | BeforeModel |
| `model-end` | AfterModel | AfterModel |
| `notification` | Notification | Notification |

### EventData Structure

The normalized event carries a flat `EventData` struct with these fields:

```go
type EventData struct {
    Prompt       string  // User prompt text
    ToolName     string  // Tool being used
    Message      string  // General message
    Reason       string  // Termination reason
    Source       string  // Event source
    SessionID    string  // Session identifier
    ToolInput    string  // Tool input data
    ToolOutput   string  // Tool output data
    FilePath     string  // File path when relevant
    InputTokens  int64   // LLM input tokens
    OutputTokens int64   // LLM output tokens
    CachedTokens int64   // Cached tokens
    AssistantText string // Final assistant output (CRITICAL — see §1.4)
    Success      bool
    Error        string
    Raw          map[string]interface{} // Full unparsed payload
}
```

### AssistantText Extraction — The Thinking Leak Vector

**This is the critical mechanism for the thinking/reasoning content leak.**

On `agent-end` events (Stop only — SubagentStop now normalizes to `subagent-end` and is excluded), the Claude dialect extracts the assistant's final response text from two sources (`pkg/sciontool/hooks/dialects/claude.go:110-118`):

#### Source 1: `last_assistant_message` field (preferred)

Claude Code 2.1+ includes a top-level `last_assistant_message` field in the Stop hook payload. The dialect takes this **verbatim as a flat string**:

```go
if text := strings.TrimSpace(getString(data, "last_assistant_message")); text != "" {
    event.Data.AssistantText = text
}
```

**This is the primary leak vector.** If Claude Code includes extended thinking/reasoning content in this field, it passes through with zero filtering. There is no content-type inspection, no block-type parsing, no thinking detection.

#### Source 2: `transcript_path` fallback (older Claude Code)

When `last_assistant_message` is absent, the dialect reads the JSONL transcript file and extracts text from the trailing assistant turn via `extractFinalAssistantText()` → `assistantContentText()`.

The transcript fallback actually has **partial filtering**: `assistantContentText()` only selects content blocks where `type == "text"` and skips `tool_use` and other block types:

```go
for _, b := range blocks {
    if b.Type == "text" && b.Text != "" {
        parts = append(parts, b.Text)
    }
}
```

This would correctly skip `type: "thinking"` blocks if they exist as separate typed blocks in the transcript. However, if thinking content is embedded within a `type: "text"` block (e.g., as part of the response text itself), it would still leak through.

#### Why the Leak Happens

The `last_assistant_message` path (the preferred, non-racy path) receives a **flat string** from Claude Code. Claude Code's Stop hook payload includes the full assistant message including extended thinking content. Since the dialect treats this as an opaque string:

1. Claude Code fires main-agent Stop hook with `last_assistant_message` containing thinking + response
2. ClaudeDialect takes the string verbatim → `event.Data.AssistantText`
3. HubHandler forwards it to the Hub as an outbound `assistant-reply` message (64KB truncation only)
4. Hub persists it in the message store
5. Hub dispatches it via broker → chat app, SSE → web UI, channels → Slack/webhook
6. User sees the full thinking content in the Messages tab / chat interface

**There is no content-type filtering at any layer of the pipeline.**

### Handler Data Flow: HubHandler

The HubHandler (`pkg/sciontool/hooks/handlers/hub.go`) is the bridge between hook events and the Hub API. Key behaviors:

| Event | Hub Action | Details |
|---|---|---|
| `session-start` | `ReportState(running, idle)` | Clears sticky states |
| `prompt-submit` | `UpdateStatus(thinking)` | Always clears sticky (new work) |
| `model-start` | `UpdateStatus(thinking)` | Respects sticky states |
| `tool-start` | `UpdateStatus(executing)` | Reports tool name; detects `AskUserQuestion`/`ExitPlanMode` as `waiting_for_input` |
| `agent-end` | **`SendOutboundMessage(AssistantText)`** + `UpdateStatus(idle)` | **Main agent only — this forwards thinking content** |
| `subagent-end` | *(no action)* | Excluded — subagent turns do not affect scion agent state |
| `session-end` | `ReportState(stopped)` | Terminal state |
| `notification` | `UpdateStatus(waiting_for_input)` | Tool permission requests |

The outbound message send on `agent-end` (lines 132-151):
```go
if event.Name == hooks.EventAgentEnd && event.Data.AssistantText != "" {
    text := event.Data.AssistantText
    const maxAssistantTextBytes = 64 * 1024
    if len(text) > maxAssistantTextBytes {
        text = text[:maxAssistantTextBytes] + "\n[truncated]"
    }
    h.client.SendOutboundMessage(msgCtx, hub.OutboundMessage{
        Msg:  text,
        Type: "assistant-reply",
    })
}
```

Note: the message type is `assistant-reply`, which is **outside the validated type enum** (`instruction`, `input-needed`, `state-change`). This type is accepted by the Hub's outbound message handler (which defaults empty type to `input-needed`, but accepts any string), but it is not part of the structured message vocabulary. This means downstream consumers (chat app, broker plugins) may not recognize or handle it specially.

### Harness Support Matrix

| Capability | Claude | Gemini | OpenCode | Codex |
|---|---|---|---|---|
| Hooks Support | ✅ | ✅ | ❌ | ❌ |
| AssistantText Extraction | ✅ (Stop hook) | ❌ (no extraction) | N/A | N/A |
| Session ID | ✅ | ❌ | ❌ | ❌ |
| Token Tracking | ✅ | ✅ | ❌ | ✅ (OTLP) |

Key observation: Only the Claude dialect extracts `AssistantText`. The Gemini dialect (`pkg/sciontool/hooks/dialects/gemini.go`) does not implement assistant text extraction at all — it has no equivalent of the `last_assistant_message` / `transcript_path` logic. This means the thinking leak is **Claude-specific**.

---

## 2. MESSAGE BROKER INTEGRATION

### Architecture

The message broker system uses a **plugin-based abstraction** (`pkg/broker/broker.go`) with NATS-style topic routing:

```
Broker Interface
    ├── Reference Broker (in-memory, built-in)
    └── External Broker Plugins (go-plugin RPC: NATS, Redis, custom)
```

Topic hierarchy:
```
scion.grove.<groveID>.agent.<agentSlug>.messages   — agent-targeted
scion.grove.<groveID>.user.<userID>.messages        — user-targeted
scion.grove.<groveID>.broadcast                     — grove broadcast
scion.global.broadcast                              — global broadcast
```

### Data Flow: Hook → Hub → Broker → Chat

```
Hook Layer (inside agent container):
┌──────────────────────────────────────────────────┐
│ sciontool hook → HubHandler.Handle()             │
│ • SendOutboundMessage(AssistantText)             │
│ • POST /api/v1/agents/{id}/outbound-message      │
└─────────────────────┬────────────────────────────┘
                      │ HTTP POST (agent token auth)
                      ▼
Hub Layer (handleAgentOutboundMessage):
┌──────────────────────────────────────────────────┐
│ 1. Resolve recipient (explicit or agent creator) │
│ 2. Build store.Message + StructuredMessage       │
│ 3. Route:                                        │
│    ├── IF broker exists:                         │
│    │   bp.PublishUserMessage(groveID, userID, m) │
│    └── ELSE:                                     │
│        ├── store.CreateMessage()                 │
│        ├── events.PublishUserMessage() → SSE     │
│        └── channelRegistry.Dispatch() → Slack    │
└─────────────────────┬────────────────────────────┘
                      │ Broker publishes to topic:
                      │ scion.grove.<id>.user.<id>.messages
                      ▼
Broker Plugin:
┌──────────────────────────────────────────────────┐
│ Plugin.Publish(topic, message)                   │
│ └── Calls back into Hub:                         │
│     POST /api/v1/broker/inbound                  │
│     (for fan-out or chat app delivery)           │
└─────────────────────┬────────────────────────────┘
                      │
                      ▼
Chat App (NotificationRelay.HandleBrokerMessage):
┌──────────────────────────────────────────────────┐
│ 1. Filter: only user-targeted topics pass        │
│ 2. If Type == "instruction": render as user card │
│ 3. Else: route to notification card rendering    │
│    → extractActivity() by string-matching msg    │
│    → renderNotificationCard()                    │
│    → SendCard() to Google Chat API               │
└──────────────────────────────────────────────────┘
```

### What Gets Lost in Translation

| Data Element | Hook Source | Hub Message | Broker/Chat | Where It's Lost |
|---|---|---|---|---|
| **Content structure** | Typed blocks (text, thinking, tool_use) | Flat `msg` string | Flat string | ClaudeDialect flattens to string |
| **Content type** | `assistant-reply` (non-standard) | Preserved in `type` field | Chat defaults to notification card | Chat app doesn't handle `assistant-reply` type |
| **Agent detail** | Phase, activity, tool name, limits | AgentStatusEvent has full detail | NotificationMessage is formatted string | `formatNotificationMessage()` serializes to human text |
| **Tool context** | Tool name, input, output | Available in event | Not in outbound message | HubHandler only forwards AssistantText, not tool data |
| **Limits tracking** | CurrentTurns, CurrentModelCalls | In StatusUpdate to Hub | Not in broker messages | Never serialized to StructuredMessage |
| **Attachments** | StructuredMessage has `[]string` | Preserved | Chat app ignores | Chat app never reads `msg.Attachments` |
| **Activity parsing** | Closed enum in Hub | Formatted string | String-matched in chat app | `extractActivity()` does fragile string matching against message body |

### The Lossy Pipeline — Root Causes

1. **Semantic flattening**: Structured agent state (phase + activity + detail) is formatted into a human-readable notification message string at `formatNotificationMessage()` (notifications.go:471-512). Downstream consumers must reverse-engineer the activity from the string content.

2. **No content-type vocabulary**: The StructuredMessage type field supports only `instruction`, `input-needed`, `state-change`. The `assistant-reply` type used by the hook system is outside this vocabulary, so the chat app treats it as a notification rather than a direct agent reply.

3. **Intentional filtering**: The chat app's `HandleBrokerMessage()` **deliberately drops non-user-targeted messages** (line 62-69) to prevent "harness terminal output" from leaking into chat. This is the right instinct — but the filter is topic-based, not content-based. An `assistant-reply` routed via the user topic passes through.

4. **No message-level content filtering**: There is no layer in the pipeline that inspects message body content. No regex for thinking markers, no block-type detection, no content classification. The pipeline is a transparent pipe from hook to chat.

### Parallel Message Systems

The system has **two independent message pipelines** that serve different purposes:

| System | Trigger | Content | Direction |
|---|---|---|---|
| **Notifications** | Agent status change (automatic) | Formatted status string | System → subscriber |
| **Messages** | Explicit send (agent or human) | Arbitrary text | Agent ↔ human |

An `ask_user` fires both: a WAITING_FOR_INPUT notification AND an `input-needed` message. They are independent records.

The `assistant-reply` outbound message from the hook system sits awkwardly between these: it's automatic (triggered by every agent turn), but it's sent via the message system (not the notification system). This creates a hybrid that delivers raw agent output through a channel designed for deliberate communication.

---

## 3. AGENT CONTROL PROTOCOL (ACP)

### Overview

The ACP is implemented as a **WebSocket-based full-duplex control channel** between the Hub and Runtime Brokers. It is not named "ACP" in the code; the implementation lives across:

- `pkg/wsprotocol/` — Message types and WebSocket connection wrapper (733 lines)
- `pkg/runtimebroker/controlchannel.go` — Broker-side client (779 lines)
- `pkg/hub/controlchannel.go` — Hub-side manager (689 lines)
- `pkg/hub/controlchannel_client.go` — Tunneling client API (568 lines)

### Protocol Capabilities

| Capability | Message Types | Description |
|---|---|---|
| **Connection** | `connect` / `connected` | Handshake with HMAC-SHA256 auth |
| **HTTP Tunneling** | `request` / `response` | Tunnel arbitrary HTTP requests to broker |
| **Stream Multiplexing** | `stream_open` / `stream` / `stream_close` / `stream_resize` | Multiple concurrent PTY/event/log streams over single WS |
| **Events** | `event` (heartbeat, agent_status, agent_output) | Async event delivery |
| **Keepalive** | `ping` / `pong` | Connection health monitoring |

Stream types: `pty` (terminal), `events` (status), `logs` (agent logs).

### Remote Interface API

The `ControlChannelClient` (`pkg/hub/controlchannel_client.go`) provides:

```go
CreateAgent()            // Create agent on remote broker
StartAgent()             // Start/resume agent
StopAgent()              // Stop agent
RestartAgent()           // Restart agent
DeleteAgent()            // Delete agent
MessageAgent()           // Send message/task to running agent
ExecAgent()              // Execute command in agent
GetAgentLogs()           // Stream logs
CheckAgentPrompt()       // Query agent state
```

All of these are implemented as **HTTP requests tunneled through the WebSocket**, not as custom protocol messages. The broker receives them as regular HTTP requests and routes them to its local handler infrastructure.

### ACP vs. Hook Approach — Fundamental Differences

| Aspect | Hook System | ACP (Control Channel) |
|---|---|---|
| **Direction** | Unidirectional (harness → Hub) | Bidirectional (Hub ↔ Broker) |
| **Trigger** | Harness event-driven (reactive) | On-demand (imperative) |
| **Scope** | Single agent lifecycle | Cross-agent, cross-grove |
| **Location** | Runs inside agent container | Runs on broker host |
| **Latency** | Per-event HTTP POST (~100ms) | Persistent WebSocket (~10ms) |
| **Content** | Normalized event data + AssistantText | Full HTTP request/response payloads |
| **Streaming** | None (fire-and-forget) | PTY, logs, events via multiplexed streams |
| **Authentication** | Agent token (self-access) | HMAC-SHA256 (broker identity) |

### Which Harnesses Support ACP

ACP operates at the **broker level**, not the harness level. All harnesses that run on a connected Runtime Broker can be managed via ACP, because ACP tunnels HTTP requests that the broker translates to local harness operations. The harness doesn't know or care whether the command came via a direct HTTP request or a tunneled WebSocket request.

However, the **quality of the remote experience** depends on harness capabilities:

| Harness | PTY Stream | Remote Messaging | Hook Status | Full Remote UI |
|---|---|---|---|---|
| Claude | ✅ (tmux) | ✅ (enqueue) | ✅ (rich) | ✅ |
| Gemini | ✅ (tmux) | ✅ (enqueue) | ✅ (basic) | ✅ |
| OpenCode | ✅ (tmux) | ✅ (enqueue) | ❌ | Partial |
| Codex | ✅ (tmux) | ✅ (enqueue) | ❌ | Partial |

### ACP and Future Mobile/Speech Interface

ACP is the right foundation for a full-fidelity remote interface because:

1. **Full-duplex dialogue**: WebSocket supports real-time bidirectional communication
2. **Stream multiplexing**: Multiple concurrent streams (PTY + events + logs) over single connection
3. **Reconnection**: Exponential backoff with automatic recovery (1s → 60s max)
4. **No data loss in translation**: HTTP tunneling preserves full request/response semantics
5. **Low latency**: Persistent connection avoids per-request overhead

For a mobile/speech interface, the ACP could be extended with:
- A new stream type for structured agent output (not raw terminal)
- Content-type metadata in stream frames to enable thinking/response separation
- A "conversation view" stream that delivers only user-visible content

---

## 4. THE THINKING LEAK — ROOT CAUSE AND FIX

### Root Cause Chain

```
Claude Code Stop hook fires
    │
    ├── last_assistant_message field contains:
    │   "I'll analyze this carefully... [extended reasoning]
    │    Here is my response: [actual visible output]"
    │
    ▼
ClaudeDialect.Parse()
    │  getString(data, "last_assistant_message") → flat string
    │  NO content-type filtering
    │  NO thinking block detection
    │
    ▼
event.Data.AssistantText = full text including thinking
    │
    ▼
HubHandler.Handle()
    │  SendOutboundMessage(text, type="assistant-reply")
    │  Only applies 64KB truncation
    │  NO content filtering
    │
    ▼
Hub handleAgentOutboundMessage()
    │  Persists to message store
    │  Publishes via broker / SSE / channels
    │  NO content filtering
    │
    ▼
Chat app / Web UI / Slack receives full thinking content
```

### Why the Transcript Fallback Doesn't Leak (As Much)

The transcript fallback path has an accidental content filter: `assistantContentText()` only selects blocks where `type == "text"`. If Claude Code writes thinking as separate `type: "thinking"` blocks in the transcript, those are skipped. However, this is a fallback path — the preferred `last_assistant_message` path has no such filtering.

### Fix Strategy

The fix should be applied at the **earliest possible layer** to prevent thinking content from propagating through the entire pipeline. Options:

#### Option A: Filter in ClaudeDialect (recommended)

Add thinking content detection in `ClaudeDialect.Parse()` when extracting `last_assistant_message`. If the field is a structured JSON array of content blocks (which newer Claude Code versions provide), parse it and filter to only `type: "text"` blocks. If it's a flat string, apply heuristic stripping of thinking markers.

**Pros**: Fixes at the source, before any handler sees it. Single change.
**Cons**: Requires knowledge of Claude Code's output format. Heuristic stripping of flat strings is fragile.

#### Option B: Filter in HubHandler before SendOutboundMessage

Add a content filter in the HubHandler that strips thinking content before forwarding. Could be a configurable transform.

**Pros**: Handler is already the decision point for what to forward. Easy to make configurable.
**Cons**: Thinking content still exists in `event.Data.AssistantText` and could leak through other handlers.

#### Option C: Add content-type metadata to StructuredMessage

Extend `StructuredMessage` with a `ContentType` field that classifies the content (e.g., `text`, `thinking`, `tool-output`). Consumers (chat app, web UI) filter based on content type.

**Pros**: Enables the normal-mode vs verbose-mode distinction cleanly.
**Cons**: Requires changes across multiple layers. Doesn't prevent storage of thinking content.

#### Recommended approach: A + C combined

1. **Immediate fix**: Filter thinking content in ClaudeDialect when extracting AssistantText. Parse `last_assistant_message` as structured content blocks if possible; strip thinking blocks before flattening to string.
2. **Architectural improvement**: Add content classification to the messaging pipeline to support normal/verbose modes.

---

## 5. ARCHITECTURE FOR CLEAN CHAT INTEGRATION

### Proposed Modes

| Mode | Who Sees What | Use Case |
|---|---|---|
| **Normal** | Only explicit `scion message` output | Chat users who want signal, not noise |
| **Verbose** | Explicit messages + assistant replies (no thinking) | Power users who want to follow agent work |
| **Full fidelity** | Everything including thinking | ACP remote interface, debugging |

### Current State vs. Desired State

**Current**: All `assistant-reply` messages (including thinking content) flow through the same pipeline as explicit `scion message` commands. The chat app cannot distinguish between:
- A deliberate message from the agent (`sciontool status ask_user "question"`)
- An automatic assistant reply dump from a hook (thinking + response text)
- A status notification (COMPLETED, WAITING_FOR_INPUT)

**Desired**: The pipeline should classify content at the source and allow consumers to subscribe at their desired fidelity level.

### Proposed Changes

#### 1. Content Classification in Dialect Layer

Extend `EventData` with structured content:

```go
type AssistantContent struct {
    Text     string // User-visible response text only
    Thinking string // Reasoning/thinking content (if present)
    Raw      string // Full unfiltered content
}
```

The ClaudeDialect would populate all three fields, with `Text` being the filtered version suitable for chat display.

#### 2. Message Type Vocabulary Extension

Add `assistant-reply` to the StructuredMessage type enum, with a sub-classification:

```go
const (
    TypeInstruction   = "instruction"
    TypeInputNeeded   = "input-needed"
    TypeStateChange   = "state-change"
    TypeAssistantReply = "assistant-reply" // NEW
)
```

Add a `Visibility` field to StructuredMessage:

```go
type StructuredMessage struct {
    // ... existing fields ...
    Visibility string `json:"visibility,omitempty"` // "normal", "verbose", "full"
}
```

#### 3. Hub Filtering by Mode

The Hub's outbound message handler should tag messages with visibility:
- Explicit `scion message` → visibility: `normal`
- `ask_user` messages → visibility: `normal`
- Hook-generated `assistant-reply` → visibility: `verbose`

#### 4. Broker/Chat App Subscription Filtering

The chat app's `HandleBrokerMessage()` should filter by visibility:
- Normal mode: only relay messages with `visibility == "normal"`
- Verbose mode: relay `normal` + `verbose`, but never `full`

This replaces the current topic-based filtering with content-aware filtering.

#### 5. ACP Stream Types for Full Fidelity

For the future mobile/speech interface via ACP, add a new stream type:

```go
const StreamTypeConversation = "conversation" // Structured agent output
```

Conversation stream frames would carry:
```go
type ConversationFrame struct {
    Role     string `json:"role"`     // "assistant", "user", "system"
    Type     string `json:"type"`     // "text", "thinking", "tool_use", "tool_result"
    Content  string `json:"content"`
    Metadata map[string]interface{} `json:"metadata,omitempty"`
}
```

This gives the mobile client full control over what to display, enabling features like collapsible thinking sections.

---

## 6. KEY FILES REFERENCE

### Hook System
| File | Purpose |
|---|---|
| `pkg/harness/claude/embeds/settings.json` | Hook event definitions for Claude |
| `cmd/sciontool/commands/hook.go` | Hook command entry point |
| `pkg/sciontool/hooks/types.go` | Event and EventData structures |
| `pkg/sciontool/hooks/harness.go` | HarnessProcessor pipeline |
| `pkg/sciontool/hooks/dialects/claude.go` | Claude event parser; **AssistantText extraction** |
| `pkg/sciontool/hooks/dialects/gemini.go` | Gemini event parser (no AssistantText) |
| `pkg/sciontool/hooks/handlers/hub.go` | **Forwards AssistantText to Hub as outbound message** |
| `pkg/sciontool/hooks/handlers/status.go` | Local agent-info.json writer |

### Message Broker
| File | Purpose |
|---|---|
| `pkg/broker/broker.go` | Broker interface & topic helpers |
| `pkg/hub/messagebroker.go` | Hub-side message broker proxy |
| `pkg/hub/notifications.go` | Notification dispatch system |
| `pkg/hub/handlers.go:1927` | `handleAgentOutboundMessage` — Hub endpoint |
| `pkg/hub/events.go` | Internal event publishing (SSE) |
| `pkg/messages/types.go` | StructuredMessage definition |
| `pkg/sciontool/hub/client.go` | Sciontool HTTP client for Hub |

### Chat App
| File | Purpose |
|---|---|
| `extras/scion-chat-app/internal/chatapp/notifications.go` | **Chat app message consumer** |

### ACP / Control Channel
| File | Purpose |
|---|---|
| `pkg/wsprotocol/protocol.go` | WebSocket message types |
| `pkg/wsprotocol/connection.go` | WebSocket connection wrapper |
| `pkg/runtimebroker/controlchannel.go` | Broker-side control channel (779 lines) |
| `pkg/hub/controlchannel.go` | Hub-side control channel manager |
| `pkg/hub/controlchannel_client.go` | Tunneling client API |

### Design Docs
| File | Purpose |
|---|---|
| `.design/messages-evolution.md` | Messages system design (complete) |
| `.design/decoupled-harness-implementation.md` | Script-based harness provisioning |
| `docs-site/src/content/docs/supported-harnesses.md` | Harness capability matrix |

---

## 7. SUMMARY OF FINDINGS

1. **Thinking content leaks because `last_assistant_message` is treated as an opaque string.** The Claude dialect extracts it verbatim and the entire pipeline (HubHandler → Hub API → broker → chat app) passes it through without content inspection. The transcript fallback has accidental filtering (only `type: "text"` blocks), but it's not the primary path.

2. **The messaging pipeline is lossy because it flattens structured data to strings.** Agent state (phase + activity + detail) is serialized to human-readable notification text, then the chat app tries to reverse-engineer the activity by string-matching the message body. The `assistant-reply` type is outside the message type vocabulary, causing it to be treated as a generic notification.

3. **The ACP provides the right foundation for a full-fidelity remote interface**, but it currently operates at the HTTP-tunneling level, not at the agent-output-streaming level. Adding a structured conversation stream type would enable mobile/speech interfaces without going through the lossy notification pipeline.

4. **The fix requires changes at two levels**: (a) immediate content filtering in the Claude dialect to strip thinking from AssistantText, and (b) architectural content classification in the messaging pipeline to support normal/verbose/full fidelity modes for different consumers.

---

## CHANGE LOG

### 2026-05-06 — Separate SubagentStop from main agent state

**Problem**: `SubagentStop` events were normalized to the same `agent-end` event as main-agent `Stop`, causing subagent turn completions to drive scion agent state changes (idle status updates, outbound assistant-reply messages, turn count increments in limits tracking). Only the main agent loop should drive the scion agent's externally-visible state.

**Changes**:
- Added `EventSubagentEnd = "subagent-end"` normalized event constant (`pkg/sciontool/hooks/types.go`)
- Split `normalizeEventName()` in the Claude dialect: `Stop` → `agent-end`, `SubagentStop` → `subagent-end` (`pkg/sciontool/hooks/dialects/claude.go`)
- AssistantText extraction (from `last_assistant_message` or `transcript_path`) now only runs for `agent-end`, skipping `subagent-end`
- No handler changes required — all handlers (StatusHandler, HubHandler, LimitsHandler, TelemetryHandler, LoggingHandler) match on explicit `EventAgentEnd`; the new `EventSubagentEnd` falls to their `default` case (no-op)

**Effect**: SubagentStop events still flow through the hook pipeline and are visible in logs (via the default logging path), but they no longer trigger status updates to the Hub, local agent-info.json changes, outbound assistant-reply messages, or turn count increments.
