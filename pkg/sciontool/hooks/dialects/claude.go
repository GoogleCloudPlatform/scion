/*
Copyright 2025 The Scion Authors.
*/

// Package dialects provides harness-specific event format parsers.
package dialects

import (
	"bufio"
	"encoding/json"
	"os"
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
//	  "hook_event_name": "PreToolUse" | "PostToolUse" | "UserPromptSubmit" | etc.,
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
		}
	}
	if val, ok := data["error"]; ok {
		if str, ok := val.(string); ok {
			event.Data.Error = str
		}
	}

	// Extract token usage from top-level or nested "usage" object.
	// Claude Code may report tokens at top level or inside a usage map.
	extractTokens(data, &event.Data)

	// Extract file_path from tool_input/tool_response objects
	extractFilePath(data, &event.Data)

	// For end-of-turn events (Stop / SubagentStop), Claude Code passes
	// the final assistant text so downstream handlers can surface it as
	// an outbound agent→user message.
	//
	// Preferred source: the top-level "last_assistant_message" field,
	// which Claude Code 2.1+ includes in the Stop hook payload directly.
	// This is authoritative and race-free: the payload is handed to the
	// hook process as structured JSON, not via a file that may still be
	// flushing when we read it.
	//
	// Fallback: read "transcript_path" (a JSONL conversation log) and
	// collect text from the trailing contiguous run of assistant entries.
	// The transcript fallback covers older Claude Code versions that
	// omit last_assistant_message and any harness that exposes only the
	// transcript. It is racy against the harness's own writes, so it is
	// strictly a fallback, not the primary path.
	if event.Name == hooks.EventAgentEnd {
		if text := strings.TrimSpace(getString(data, "last_assistant_message")); text != "" {
			event.Data.AssistantText = text
		} else if path := getString(data, "transcript_path"); path != "" {
			if text := extractFinalAssistantText(path); text != "" {
				event.Data.AssistantText = text
			}
		}
	}

	return event, nil
}

// extractFinalAssistantText reads a Claude Code transcript JSONL file and
// returns the concatenated text blocks from the final assistant turn. A
// "final turn" is the contiguous run of assistant entries at the end of the
// transcript, stopped at the first preceding user entry. Tool-use and other
// non-text content blocks are skipped. On any error the function returns an
// empty string — callers must treat absence as "no assistant text
// available" rather than a failure condition.
func extractFinalAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Collect text from contiguous assistant entries at the tail of the
	// transcript. Iterate forward once (Claude transcripts are small
	// enough that double-pass or reverse-scan overhead is unnecessary);
	// reset the collected text whenever a user entry is seen so that by
	// the end of the scan we hold exactly the final assistant turn.
	var turnParts []string
	scanner := bufio.NewScanner(f)
	// Transcript lines can contain very long tool outputs; raise the
	// scanner buffer so we don't silently truncate them.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entryType := entry.Type
		if entryType == "" {
			entryType = entry.Message.Role
		}

		switch entryType {
		case "user":
			// A user entry ends any prior assistant turn.
			turnParts = turnParts[:0]
		case "assistant":
			if text := assistantContentText(entry.Message.Content); text != "" {
				turnParts = append(turnParts, text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ""
	}

	return strings.TrimSpace(strings.Join(turnParts, "\n\n"))
}

// assistantContentText extracts text from an assistant message's content
// field, which in Claude transcripts is either a JSON array of typed blocks
// or (rarely) a plain string. Only "text" blocks contribute; tool_use and
// other block types are ignored.
func assistantContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Plain string content (older/simpler transcript shape).
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return strings.TrimSpace(s)
	}

	// Typed block array.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
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
	case "PostToolUse":
		return hooks.EventToolEnd
	case "Stop", "SubagentStop":
		return hooks.EventAgentEnd
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
