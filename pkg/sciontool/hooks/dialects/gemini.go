/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// GeminiDialect parses Gemini CLI hook events.
type GeminiDialect struct{}

// NewGeminiDialect creates a new Gemini dialect parser.
func NewGeminiDialect() *GeminiDialect {
	return &GeminiDialect{}
}

// Name returns the dialect name.
func (d *GeminiDialect) Name() string {
	return "gemini"
}

// Parse converts Gemini CLI event format to normalized Event.
//
// Gemini CLI sends events with the following format:
//
//	{
//	  "hook_event_name": "BeforeAgent" | "AfterAgent" | "BeforeTool" | etc.,
//	  "tool_name": "...",
//	  "prompt": "...",
//	  ...
//	}
func (d *GeminiDialect) Parse(data map[string]interface{}) (*hooks.Event, error) {
	rawName := getString(data, "hook_event_name")
	if rawName == "" {
		rawName = getString(data, "event")
	}

	event := &hooks.Event{
		Name:    d.normalizeEventName(rawName),
		RawName: rawName,
		Dialect: "gemini",
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

	// Extract token usage
	extractTokens(data, &event.Data)

	// Extract file_path from tool_input/tool_response objects
	extractFilePath(data, &event.Data)

	return event, nil
}

// normalizeEventName maps Gemini CLI event names to normalized names.
func (d *GeminiDialect) normalizeEventName(name string) string {
	switch name {
	case "SessionStart":
		return hooks.EventSessionStart
	case "SessionEnd":
		return hooks.EventSessionEnd
	case "BeforeAgent":
		return hooks.EventAgentStart
	case "AfterAgent":
		return hooks.EventAgentEnd
	case "BeforeTool":
		return hooks.EventToolStart
	case "AfterTool":
		return hooks.EventToolEnd
	case "BeforeModel":
		return hooks.EventModelStart
	case "AfterModel":
		return hooks.EventModelEnd
	case "Notification":
		return hooks.EventNotification
	default:
		return name
	}
}
