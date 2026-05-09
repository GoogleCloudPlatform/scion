/*
Copyright 2025 The Scion Authors.
*/

// Package hooks provides the hook system for sciontool.
// It handles both Scion lifecycle hooks (pre-start, post-start, session-end)
// and harness hooks (events from Claude Code, Gemini CLI, etc.).
package hooks

// Event represents a normalized hook event.
type Event struct {
	// Name is the normalized event name (e.g., "tool-start", "session-start")
	Name string `json:"name"`

	// RawName is the original event name as received from the harness
	RawName string `json:"raw_name,omitempty"`

	// Dialect is the source dialect (e.g., "claude", "gemini")
	Dialect string `json:"dialect,omitempty"`

	// Data contains event-specific data
	Data EventData `json:"data"`
}

// EventData contains the parsed event payload.
type EventData struct {
	// Common fields
	Prompt    string `json:"prompt,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Message   string `json:"message,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Source    string `json:"source,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// Tool-specific fields
	ToolInput  string `json:"tool_input,omitempty"`
	ToolOutput string `json:"tool_output,omitempty"`
	FilePath   string `json:"file_path,omitempty"`

	// Token usage fields (populated from model-end / session-end events)
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	CachedTokens int64 `json:"cached_tokens,omitempty"`

	// AssistantText holds the textual output of the agent's final turn,
	// populated by dialect parsers for end-of-turn events when the
	// underlying harness exposes assistant content (e.g. Claude Code's
	// Stop hook via transcript_path). Handlers may forward this to the
	// hub message store as an outbound agent→user message so the Messages
	// tab reflects agent replies without re-ingesting the entire
	// transcript.
	AssistantText string `json:"assistant_text,omitempty"`

	// Status fields
	Success bool   `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	// Raw contains the original unparsed data
	Raw map[string]interface{} `json:"raw,omitempty"`
}

// NormalizedEventName constants for standard event types.
const (
	// Session lifecycle events
	EventSessionStart = "session-start"
	EventSessionEnd   = "session-end"

	// Agent turn events
	EventAgentStart  = "agent-start"
	EventAgentEnd    = "agent-end"
	EventSubagentEnd = "subagent-end"

	// Tool execution events
	EventToolStart = "tool-start"
	EventToolEnd   = "tool-end"

	// User interaction events
	EventPromptSubmit     = "prompt-submit"
	EventResponseComplete = "response-complete"

	// Notification events
	EventNotification = "notification"

	// Scion lifecycle events (internal)
	EventPreStart  = "pre-start"
	EventPostStart = "post-start"
	EventPreStop   = "pre-stop"

	// LLM model events
	EventModelStart = "model-start"
	EventModelEnd   = "model-end"
)

// Handler is a function that processes a hook event.
type Handler func(event *Event) error

// Dialect is an interface for parsing harness-specific event formats.
type Dialect interface {
	// Name returns the dialect name (e.g., "claude", "gemini")
	Name() string

	// Parse parses raw input data into a normalized Event.
	Parse(data map[string]interface{}) (*Event, error)
}
