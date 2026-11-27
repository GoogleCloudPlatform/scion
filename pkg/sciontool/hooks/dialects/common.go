/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"math"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// getString safely extracts a string value from a map.
func getString(data map[string]interface{}, key string) string {
	if val, ok := data[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

// extractTokens populates token usage fields on EventData from raw event data.
// It checks top-level fields first, then falls back to a nested "usage" object.
// Supports field names: input_tokens, output_tokens, cached_tokens,
// cache_read_tokens, cache_read_input_tokens, cache_write_tokens,
// cache_creation_input_tokens, reasoning_tokens, thinking_tokens,
// prompt_tokens, completion_tokens.
// Measured zeros (>= 0) are preserved and marked present in TokenPresence;
// fields already populated (e.g. via mapping entry `fields:`) are not overwritten.
func extractTokens(data map[string]interface{}, ed *hooks.EventData) {
	extractTokensFromMap(data, ed)

	// Fall back to nested "usage" object when neither input nor output was
	// reported at the top level.
	if !ed.HasInputTokens() && !ed.HasOutputTokens() {
		if usageRaw, ok := data["usage"]; ok {
			if usage, ok := usageRaw.(map[string]interface{}); ok {
				extractTokensFromMap(usage, ed)
			}
		}
	}
}

func extractTokensFromMap(data map[string]interface{}, ed *hooks.EventData) {
	if !ed.HasInputTokens() {
		if v, ok := getInt64(data, "input_tokens"); ok {
			ed.SetInputTokens(v)
		} else if v, ok := getInt64(data, "prompt_tokens"); ok {
			ed.SetInputTokens(v)
		}
	}
	if !ed.HasOutputTokens() {
		if v, ok := getInt64(data, "output_tokens"); ok {
			ed.SetOutputTokens(v)
		} else if v, ok := getInt64(data, "completion_tokens"); ok {
			ed.SetOutputTokens(v)
		}
	}
	if !ed.HasCachedTokens() {
		// Claude uses cache_read_input_tokens; Muse Code also emits cache_read_tokens.
		if v, ok := getInt64(data, "cached_tokens"); ok {
			ed.SetCachedTokens(v)
		} else if v, ok := getInt64(data, "cache_read_tokens"); ok {
			ed.SetCachedTokens(v)
		} else if v, ok := getInt64(data, "cache_read_input_tokens"); ok {
			ed.SetCachedTokens(v)
		}
	}
	if !ed.HasCacheWriteTokens() {
		if v, ok := getInt64(data, "cache_write_tokens"); ok {
			ed.SetCacheWriteTokens(v)
		} else if v, ok := getInt64(data, "cache_creation_input_tokens"); ok {
			ed.SetCacheWriteTokens(v)
		}
	}
	if !ed.HasReasoningTokens() {
		if v, ok := getInt64(data, "reasoning_tokens"); ok {
			ed.SetReasoningTokens(v)
		} else if v, ok := getInt64(data, "thinking_tokens"); ok {
			ed.SetReasoningTokens(v)
		}
	}
}

// extractFilePath greedily extracts a file_path value from tool_input or
// tool_response fields when they are JSON objects (map[string]interface{}).
// It checks tool_input.file_path first, then tool_response.filePath / file_path.
func extractFilePath(data map[string]interface{}, ed *hooks.EventData) {
	// Try tool_input.file_path (common in Claude Write/Edit/Read, Gemini replace/read_file)
	if raw, ok := data["tool_input"]; ok {
		if m, ok := raw.(map[string]interface{}); ok {
			if fp := getString(m, "file_path"); fp != "" {
				ed.FilePath = fp
				return
			}
		}
	}

	// Try tool_response.filePath (Claude Write tool response uses camelCase)
	if raw, ok := data["tool_response"]; ok {
		if m, ok := raw.(map[string]interface{}); ok {
			if fp := getString(m, "filePath"); fp != "" {
				ed.FilePath = fp
				return
			}
			if fp := getString(m, "file_path"); fp != "" {
				ed.FilePath = fp
				return
			}
		}
	}

	// Try top-level file_path (in case a harness sends it directly)
	if fp := getString(data, "file_path"); fp != "" {
		ed.FilePath = fp
	}
}

// getInt64 safely extracts a non-negative int64 value from a map.
// JSON numbers arrive as float64, so both float64 and direct integer types are
// handled. Missing, non-numeric, non-finite, or negative values return (0, false).
func getInt64(data map[string]interface{}, key string) (int64, bool) {
	val, ok := data[key]
	if !ok || val == nil {
		return 0, false
	}
	return toNonNegInt64(val)
}

func toNonNegInt64(val interface{}) (int64, bool) {
	switch v := val.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false
		}
		return int64(v), true
	case int64:
		if v < 0 {
			return 0, false
		}
		return v, true
	case int:
		if v < 0 {
			return 0, false
		}
		return int64(v), true
	}
	return 0, false
}
