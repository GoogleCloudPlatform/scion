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

	// Model is the model named by the event payload itself, when the
	// harness reports one (e.g. antigravity's modelName, or the opencode
	// bridge's joined provider/model). Telemetry prefers it over
	// SCION_MODEL for the model label (design §3.2); empty means the
	// payload carried none.
	Model string `json:"model,omitempty"`

	// Tool-specific fields
	ToolInput  string `json:"tool_input,omitempty"`
	ToolOutput string `json:"tool_output,omitempty"`
	FilePath   string `json:"file_path,omitempty"`

	// Token usage fields (populated from model-end / session-end events)
	InputTokens        int64         `json:"input_tokens,omitempty"`
	OutputTokens       int64         `json:"output_tokens,omitempty"`
	CachedTokens       int64         `json:"cached_tokens,omitempty"`
	CacheWriteTokens   int64         `json:"cache_write_tokens,omitempty"`
	ReasoningTokens    int64         `json:"reasoning_tokens,omitempty"`
	InputTokensDerived bool          `json:"input_tokens_derived,omitempty"`
	TokenPresence      TokenPresence `json:"-"`

	// Status fields
	Success bool   `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	// Failed is true when the harness reported the call as failed: its
	// success field was present and false, or the event is a failure event
	// (claude PostToolUseFailure). Success alone cannot tell a failure from
	// a success, since most harnesses send no success field for a call that
	// succeeded.
	Failed bool `json:"failed,omitempty"`

	// Interrupted is true when the user interrupted the tool call (claude
	// PostToolUseFailure with is_interrupt). An interrupted call is not a
	// failure of the tool.
	Interrupted bool `json:"interrupted,omitempty"`

	// Uncounted is true when the event reports a model call that call
	// accounting must not count: max_model_calls, the gen_ai.api.calls
	// metric, usage records and session model-call counts. A harness sets
	// it (dialect mapping "uncounted") for an event it sends for only some
	// of its calls, such as copilot's errorOccurred for a failed call, when
	// it reports no successful ones. Status handling is unchanged.
	Uncounted bool `json:"uncounted,omitempty"`

	// Raw contains the original unparsed data
	Raw map[string]interface{} `json:"raw,omitempty"`
}

// TokenPresence records which token usage fields on EventData were explicitly
// reported by the harness payload (including measured zeros) versus absent.
type TokenPresence struct {
	Input      bool
	Output     bool
	Cached     bool
	CacheWrite bool
	Reasoning  bool
}

// HasInputTokens reports whether InputTokens was explicitly reported (including
// a measured 0) or populated with a positive value directly on EventData.
func (d EventData) HasInputTokens() bool { return d.TokenPresence.Input || d.InputTokens > 0 }

// HasOutputTokens reports whether OutputTokens was explicitly reported
// (including a measured 0) or populated with a positive value directly on EventData.
func (d EventData) HasOutputTokens() bool { return d.TokenPresence.Output || d.OutputTokens > 0 }

// HasCachedTokens reports whether CachedTokens was explicitly reported
// (including a measured 0) or populated with a positive value directly on EventData.
func (d EventData) HasCachedTokens() bool { return d.TokenPresence.Cached || d.CachedTokens > 0 }

// HasCacheWriteTokens reports whether CacheWriteTokens was explicitly reported
// (including a measured 0) or populated with a positive value directly on EventData.
func (d EventData) HasCacheWriteTokens() bool {
	return d.TokenPresence.CacheWrite || d.CacheWriteTokens > 0
}

// HasReasoningTokens reports whether ReasoningTokens was explicitly reported
// (including a measured 0) or populated with a positive value directly on EventData.
func (d EventData) HasReasoningTokens() bool {
	return d.TokenPresence.Reasoning || d.ReasoningTokens > 0
}

// SetInputTokens records a measured input token count and marks it present.
func (d *EventData) SetInputTokens(v int64) {
	d.InputTokens = v
	d.TokenPresence.Input = true
}

// SetOutputTokens records a measured output token count and marks it present.
func (d *EventData) SetOutputTokens(v int64) {
	d.OutputTokens = v
	d.TokenPresence.Output = true
}

// SetCachedTokens records a measured cached token count and marks it present.
func (d *EventData) SetCachedTokens(v int64) {
	d.CachedTokens = v
	d.TokenPresence.Cached = true
}

// SetCacheWriteTokens records a measured cache-write token count and marks it present.
func (d *EventData) SetCacheWriteTokens(v int64) {
	d.CacheWriteTokens = v
	d.TokenPresence.CacheWrite = true
}

// SetReasoningTokens records a measured reasoning token count and marks it present.
func (d *EventData) SetReasoningTokens(v int64) {
	d.ReasoningTokens = v
	d.TokenPresence.Reasoning = true
}

// ClearTokens zeroes all token counts and clears their presence flags.
func (d *EventData) ClearTokens() {
	d.InputTokens = 0
	d.OutputTokens = 0
	d.CachedTokens = 0
	d.CacheWriteTokens = 0
	d.ReasoningTokens = 0
	d.TokenPresence = TokenPresence{}
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

	// EventAgentError is a turn that ended on an error, from a harness
	// that reports a failed turn on a dedicated hook (claude and
	// grok-build StopFailure) instead of agent-end with an error. Data.Error
	// carries the reason.
	EventAgentError = "agent-error"

	// EventIgnored is a harness event that its dialect matched but
	// discards (dialect mapping "skip_when"). No handler acts on it.
	EventIgnored = "ignored"

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
