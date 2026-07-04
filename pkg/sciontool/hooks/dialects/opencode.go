/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// OpenCodeDialect parses events emitted by the scion-plugin.js OpenCode plugin.
//
// The plugin emits pre-normalized events in the following format:
//
//	{
//	  "name": "tool-start" | "tool-end" | "session-start" | etc.,
//	  "tool_name": "...",
//	  "prompt": "...",
//	  "success": true,
//	  ...
//	}
//
// Unlike Claude/Gemini/Codex dialects which normalize harness-specific event
// names (e.g., "PreToolUse" -> "tool-start"), the opencode dialect receives
// already-normalized event names and passes them through directly.
type OpenCodeDialect struct{}

// NewOpenCodeDialect creates a new OpenCode dialect parser.
func NewOpenCodeDialect() *OpenCodeDialect {
	return &OpenCodeDialect{}
}

// Name returns the dialect name.
func (d *OpenCodeDialect) Name() string {
	return "opencode"
}

// Parse converts OpenCode plugin event format to normalized Event.
func (d *OpenCodeDialect) Parse(data map[string]interface{}) (*hooks.Event, error) {
	rawName := getString(data, "name")
	if rawName == "" {
		rawName = getString(data, "hook_event_name")
	}
	if rawName == "" {
		return nil, fmt.Errorf("opencode: missing event name")
	}

	payload := data
	if nested, ok := data["data"]; ok {
		if m, ok := nested.(map[string]interface{}); ok && len(m) > 0 {
			payload = m
		}
	}

	event := &hooks.Event{
		Name:    d.normalizeEventName(rawName),
		RawName: rawName,
		Dialect: "opencode",
		Data: hooks.EventData{
			Prompt:        getString(payload, "prompt"),
			ToolName:      getString(payload, "tool_name"),
			Message:       getString(payload, "message"),
			Reason:        getString(payload, "reason"),
			Source:        getString(payload, "source"),
			SessionID:     getString(payload, "session_id"),
			Success:       getBool(payload, "success"),
			Error:         getString(payload, "error"),
			AssistantText: getString(payload, "assistant_text"),
			Raw:           payload,
		},
	}

	if val, ok := payload["tool_input"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolInput = str
		}
	}
	if val, ok := payload["tool_output"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolOutput = str
		}
	}

	extractTokens(payload, &event.Data)

	extractFilePath(payload, &event.Data)

	if isHB, _ := data["_scion_heartbeat"].(bool); isHB {
		if event.Data.Raw == nil {
			event.Data.Raw = make(map[string]interface{})
		}
		event.Data.Raw["_scion_heartbeat"] = true
	}

	return event, nil
}

// normalizeEventName passes through pre-normalized event names from the plugin.
func (d *OpenCodeDialect) normalizeEventName(name string) string {
	switch name {
	case "_activity":
		return ""
	default:
		return name
	}
}
