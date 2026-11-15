/*
Copyright 2025 The Scion Authors.
*/

// Package dialects provides harness-specific event format parsers.
package dialects

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// ClaudeDialect parses Claude Code hook events.
type ClaudeDialect struct{}

// NewClaudeDialect creates a new Claude dialect parser.
func NewClaudeDialect() *ClaudeDialect {
	return &ClaudeDialect{}
}

// Name returns the dialect name.
func (d *ClaudeDialect) Name() string {
	return "claude"
}

// Parse converts Claude Code event format to normalized Event.
//
// Claude Code sends events with the following format:
//
//	{
//	  "hook_event_name": "PreToolUse" | "PostToolUse" | "PostToolUseFailure" | "UserPromptSubmit" | etc.,
//	  "tool_name": "...",
//	  "prompt": "...",
//	  "message": "...",
//	  ...
//	}
func (d *ClaudeDialect) Parse(data map[string]interface{}) (*hooks.Event, error) {
	rawName := getString(data, "hook_event_name")
	if rawName == "" {
		// Fallback to checking other common fields
		rawName = getString(data, "event")
	}

	event := &hooks.Event{
		Name:    d.normalizeEventName(rawName),
		RawName: rawName,
		Dialect: "claude",
		Data: hooks.EventData{
			Prompt:    getString(data, "prompt"),
			ToolName:  getString(data, "tool_name"),
			Message:   getString(data, "message"),
			Reason:    getString(data, "reason"),
			Source:    getString(data, "source"),
			SessionID: getString(data, "session_id"),
			Raw:       data,
		},
	}

	// Extract tool input/output if available
	if val, ok := data["tool_input"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolInput = str
		}
	}
	if val, ok := data["tool_output"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolOutput = str
		}
	}

	// Extract status fields
	if val, ok := data["success"]; ok {
		if b, ok := val.(bool); ok {
			event.Data.Success = b
			event.Data.Failed = !b
		}
	}
	if val, ok := data["error"]; ok {
		if str, ok := val.(string); ok {
			event.Data.Error = str
		}
	}
	// A StopFailure is a turn that ended on an API error. Its error field
	// is a bare category ("unknown", "authentication_failed", ...); the
	// provider's message is in last_assistant_message.
	if rawName == "StopFailure" {
		event.Data.Error = claudeStopFailureError(data)
	}
	// A PostToolUseFailure is a failed tool call even when its error string
	// is empty. is_interrupt marks a call the user interrupted.
	if rawName == "PostToolUseFailure" {
		event.Data.Failed = true
		if b, ok := data["is_interrupt"].(bool); ok && b {
			event.Data.Interrupted = true
		}
	}

	// Extract token usage from top-level or nested "usage" object.
	// Claude Code may report tokens at top level or inside a usage map.
	extractTokens(data, &event.Data)

	// Extract file_path from tool_input/tool_response objects
	extractFilePath(data, &event.Data)

	return event, nil
}

// normalizeEventName maps Claude Code event names to normalized names.
func (d *ClaudeDialect) normalizeEventName(name string) string {
	switch name {
	case "SessionStart":
		return hooks.EventSessionStart
	case "SessionEnd":
		return hooks.EventSessionEnd
	case "UserPromptSubmit":
		return hooks.EventPromptSubmit
	case "PreToolUse":
		return hooks.EventToolStart
	case "PostToolUse", "PostToolUseFailure":
		// A failed tool call arrives as PostToolUseFailure. It ends the tool
		// call just like PostToolUse; Parse carries its error string.
		return hooks.EventToolEnd
	case "Stop":
		return hooks.EventAgentEnd
	case "StopFailure":
		// Claude Code fires StopFailure instead of Stop when the turn ends
		// on an API error (captured from 2.1.325, testdata/claude).
		return hooks.EventAgentError
	case "SubagentStop":
		return hooks.EventSubagentEnd
	case "Notification":
		return hooks.EventNotification
	case "BeforeModel", "ModelRequest":
		return hooks.EventModelStart
	case "AfterModel", "ModelResponse":
		return hooks.EventModelEnd
	default:
		return name
	}
}

// claudeStopFailureCodeUnknown is the StopFailure error category Claude Code
// sends when it has no more specific one (for example an HTTP 400).
const claudeStopFailureCodeUnknown = "unknown"

// claudeStopFailureError returns the turn error of a StopFailure payload:
// "<category>: <message>", with the category left out when it is "unknown"
// or missing, and the message (last_assistant_message, which carries the
// provider's text, such as "API Error: 400 ...") left out when missing.
func claudeStopFailureError(data map[string]interface{}) string {
	code := strings.TrimSpace(getString(data, "error"))
	message := strings.TrimSpace(getString(data, "last_assistant_message"))
	if code == claudeStopFailureCodeUnknown {
		code = ""
	}
	switch {
	case code == "":
		return message
	case message == "":
		return code
	}
	return code + ": " + message
}
