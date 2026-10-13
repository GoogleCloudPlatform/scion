/*
Copyright 2025 The Scion Authors.
*/

package dialects

import "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"

// CodexDialect parses Codex notify payloads and Codex hook payloads
// (UserPromptSubmit, PreToolUse, PostToolUse, Stop, SubagentStop and the
// session events), mapping them to the same normalized events as the staged
// harnesses/codex/dialect.yaml.
type CodexDialect struct{}

func NewCodexDialect() *CodexDialect {
	return &CodexDialect{}
}

func (d *CodexDialect) Name() string {
	return "codex"
}

func (d *CodexDialect) Parse(data map[string]interface{}) (*hooks.Event, error) {
	rawName := getString(data, "type")
	if rawName == "" {
		rawName = getString(data, "event")
	}
	if rawName == "" {
		rawName = getString(data, "hook_event_name")
	}

	event := &hooks.Event{
		Name:    d.normalizeEventName(rawName),
		RawName: rawName,
		Dialect: "codex",
		Data: hooks.EventData{
			Message:   firstNonEmptyString(getString(data, "title"), getString(data, "message")),
			Prompt:    getString(data, "prompt"),
			ToolName:  getString(data, "tool_name"),
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

// normalizeEventName maps a raw Codex event name to a normalized event name.
// It must map every event harnesses/codex/dialect.yaml maps, to the same
// normalized name, so status tracking works the same with or without the
// staged dialect file; TestCodexDialect_MatchesStagedDialect enforces this.
func (d *CodexDialect) normalizeEventName(name string) string {
	switch name {
	case "agent-turn-complete":
		return hooks.EventResponseComplete
	case "notification":
		return hooks.EventNotification
	case "session-start", "SessionStart":
		return hooks.EventSessionStart
	case "session-end", "SessionEnd":
		return hooks.EventSessionEnd
	case "UserPromptSubmit":
		return hooks.EventPromptSubmit
	case "tool-start", "BeforeTool", "PreToolUse":
		return hooks.EventToolStart
	case "tool-end", "AfterTool", "PostToolUse":
		return hooks.EventToolEnd
	case "model-start", "BeforeModel":
		return hooks.EventModelStart
	case "model-end", "AfterModel":
		return hooks.EventModelEnd
	case "Stop":
		return hooks.EventAgentEnd
	case "SubagentStop":
		return hooks.EventSubagentEnd
	default:
		return name
	}
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
