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

	// For main-agent end-of-turn events (Stop only — not SubagentStop),
	// Claude Code passes the final assistant text so downstream handlers
	// can surface it as an outbound agent→user message. Subagent turns
	// are internal to the harness and should not drive scion agent state.
	//
	// Content-type filtering: the extraction pipeline classifies content
	// blocks by type (text, thinking, tool_use, etc.) and stores them in
	// AssistantContent. AssistantText receives only user-visible "text"
	// blocks — thinking/reasoning content is filtered out to prevent it
	// from leaking into chat messages.
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
		text, content := extractAssistantContentFromPayload(data)
		if content != nil {
			event.Data.AssistantText = text
			event.Data.AssistantContent = content
		} else if path := getString(data, "transcript_path"); path != "" {
			text, content := extractFinalAssistantContentFromTranscript(path)
			if content != nil {
				event.Data.AssistantText = text
				event.Data.AssistantContent = content
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
	defer func() { _ = f.Close() }()

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
	// If the scanner hit an error (e.g. a single line exceeded the 16MB
	// buffer limit), return whatever text was collected before the error
	// rather than discarding the entire turn.
	if err := scanner.Err(); err != nil && len(turnParts) == 0 {
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

// extractAssistantContentFromPayload extracts and classifies content from
// the last_assistant_message field in the hook payload. The field value may be:
//
//   - A string: treated as plain text (single text block), or parsed as a
//     JSON array of typed content blocks if it looks like JSON.
//   - A []interface{}: a structured array of content blocks with type fields.
//
// Returns the filtered user-visible text (thinking blocks stripped) and the
// full classified AssistantContent. Returns ("", nil) if no content is found.
func extractAssistantContentFromPayload(data map[string]interface{}) (string, *hooks.AssistantContent) {
	val, ok := data["last_assistant_message"]
	if !ok {
		return "", nil
	}

	switch v := val.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return "", nil
		}
		// Try to parse as a JSON array of typed content blocks.
		// Claude Code may send structured blocks as a JSON-encoded string.
		if blocks := parseContentBlocksFromJSON(text); len(blocks) > 0 {
			content := &hooks.AssistantContent{Blocks: blocks}
			return content.TextOnly(), content
		}
		// Plain string — treat as a single text block.
		content := &hooks.AssistantContent{
			Blocks: []hooks.ContentBlock{{Type: hooks.ContentBlockText, Text: text}},
		}
		return text, content

	case []interface{}:
		// Structured array of content blocks (already parsed from JSON).
		blocks := parseContentBlocksFromArray(v)
		if len(blocks) == 0 {
			return "", nil
		}
		content := &hooks.AssistantContent{Blocks: blocks}
		return content.TextOnly(), content

	default:
		return "", nil
	}
}

// parseContentBlocksFromJSON attempts to parse a string as a JSON array of
// typed content blocks. Returns nil if the string is not valid JSON or not
// an array of objects with "type" fields.
func parseContentBlocksFromJSON(s string) []hooks.ContentBlock {
	// Quick check: must look like a JSON array.
	if len(s) < 2 || s[0] != '[' {
		return nil
	}

	var rawBlocks []map[string]interface{}
	if err := json.Unmarshal([]byte(s), &rawBlocks); err != nil {
		return nil
	}

	// Validate that at least one entry has a "type" field — this
	// distinguishes a content block array from arbitrary JSON arrays.
	hasType := false
	for _, b := range rawBlocks {
		if _, ok := b["type"]; ok {
			hasType = true
			break
		}
	}
	if !hasType {
		return nil
	}

	var blocks []hooks.ContentBlock
	for _, b := range rawBlocks {
		block := classifyContentBlock(b)
		if block.Type != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// parseContentBlocksFromArray converts a []interface{} (from JSON
// unmarshalling) into classified ContentBlocks.
func parseContentBlocksFromArray(arr []interface{}) []hooks.ContentBlock {
	var blocks []hooks.ContentBlock
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		block := classifyContentBlock(m)
		if block.Type != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// classifyContentBlock converts a raw content block map into a typed
// ContentBlock. Supported block types:
//   - "text": user-visible text (text field)
//   - "thinking": reasoning/thinking traces (thinking field)
//   - "tool_use": tool invocations (name + input)
//   - "tool_result": tool outputs (content field)
func classifyContentBlock(m map[string]interface{}) hooks.ContentBlock {
	blockType := getString(m, "type")
	switch blockType {
	case "text":
		return hooks.ContentBlock{
			Type: hooks.ContentBlockText,
			Text: getString(m, "text"),
		}
	case "thinking":
		// Claude's thinking blocks use "thinking" field, not "text".
		text := getString(m, "thinking")
		if text == "" {
			text = getString(m, "text")
		}
		return hooks.ContentBlock{
			Type: hooks.ContentBlockThinking,
			Text: text,
		}
	case "tool_use":
		// Summarize as "tool_name: <name>" for downstream consumers.
		name := getString(m, "name")
		return hooks.ContentBlock{
			Type: hooks.ContentBlockToolUse,
			Text: name,
		}
	case "tool_result":
		text := getString(m, "content")
		if text == "" {
			text = getString(m, "text")
		}
		return hooks.ContentBlock{
			Type: hooks.ContentBlockToolResult,
			Text: text,
		}
	default:
		if blockType == "" {
			return hooks.ContentBlock{}
		}
		// Unknown block type — preserve with original type string.
		text := getString(m, "text")
		if text == "" {
			text = getString(m, "content")
		}
		return hooks.ContentBlock{Type: blockType, Text: text}
	}
}

// extractFinalAssistantContentFromTranscript reads a Claude Code transcript
// JSONL file and returns both filtered text and classified content blocks
// from the final assistant turn. This is the content-type-aware version of
// extractFinalAssistantText that populates AssistantContent.
func extractFinalAssistantContentFromTranscript(path string) (string, *hooks.AssistantContent) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer func() { _ = f.Close() }()

	var turnBlocks []hooks.ContentBlock
	scanner := bufio.NewScanner(f)
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
			turnBlocks = turnBlocks[:0]
		case "assistant":
			blocks := classifyTranscriptContent(entry.Message.Content)
			turnBlocks = append(turnBlocks, blocks...)
		}
	}

	if len(turnBlocks) == 0 {
		return "", nil
	}

	content := &hooks.AssistantContent{Blocks: turnBlocks}
	return content.TextOnly(), content
}

// classifyTranscriptContent parses assistant message content from a transcript
// entry and returns classified ContentBlocks. Content may be a JSON array of
// typed blocks or (rarely) a plain string.
func classifyTranscriptContent(content json.RawMessage) []hooks.ContentBlock {
	if len(content) == 0 {
		return nil
	}

	// Plain string content (older/simpler transcript shape).
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		text := strings.TrimSpace(s)
		if text == "" {
			return nil
		}
		return []hooks.ContentBlock{{Type: hooks.ContentBlockText, Text: text}}
	}

	// Typed block array.
	var rawBlocks []map[string]interface{}
	if err := json.Unmarshal(content, &rawBlocks); err != nil {
		return nil
	}

	var blocks []hooks.ContentBlock
	for _, b := range rawBlocks {
		block := classifyContentBlock(b)
		if block.Type != "" {
			blocks = append(blocks, block)
		}
	}
	return blocks
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
	case "Stop":
		return hooks.EventAgentEnd
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
