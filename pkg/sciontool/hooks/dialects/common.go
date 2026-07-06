/*
Copyright 2025 The Scion Authors.
*/

package dialects

import "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"

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
// cache_read_input_tokens, prompt_tokens, completion_tokens.
func extractTokens(data map[string]interface{}, ed *hooks.EventData) {
	// Try top-level fields first
	if v := getInt64(data, "input_tokens"); v > 0 {
		ed.InputTokens = v
	}
	if v := getInt64(data, "output_tokens"); v > 0 {
		ed.OutputTokens = v
	}
	if v := getInt64(data, "cached_tokens"); v > 0 {
		ed.CachedTokens = v
	}
	// Claude uses cache_read_input_tokens
	if ed.CachedTokens == 0 {
		if v := getInt64(data, "cache_read_input_tokens"); v > 0 {
			ed.CachedTokens = v
		}
	}
	// OpenAI-style names
	if ed.InputTokens == 0 {
		if v := getInt64(data, "prompt_tokens"); v > 0 {
			ed.InputTokens = v
		}
	}
	if ed.OutputTokens == 0 {
		if v := getInt64(data, "completion_tokens"); v > 0 {
			ed.OutputTokens = v
		}
	}

	// Fall back to nested "usage" object
	if ed.InputTokens == 0 && ed.OutputTokens == 0 {
		if usageRaw, ok := data["usage"]; ok {
			if usage, ok := usageRaw.(map[string]interface{}); ok {
				if v := getInt64(usage, "input_tokens"); v > 0 {
					ed.InputTokens = v
				}
				if v := getInt64(usage, "output_tokens"); v > 0 {
					ed.OutputTokens = v
				}
				if v := getInt64(usage, "cached_tokens"); v > 0 {
					ed.CachedTokens = v
				}
				if ed.CachedTokens == 0 {
					if v := getInt64(usage, "cache_read_input_tokens"); v > 0 {
						ed.CachedTokens = v
					}
				}
				if ed.InputTokens == 0 {
					if v := getInt64(usage, "prompt_tokens"); v > 0 {
						ed.InputTokens = v
					}
				}
				if ed.OutputTokens == 0 {
					if v := getInt64(usage, "completion_tokens"); v > 0 {
						ed.OutputTokens = v
					}
				}
			}
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

// getInt64 safely extracts an int64 value from a map.
// JSON numbers arrive as float64, so both float64 and direct integer types are handled.
func getInt64(data map[string]interface{}, key string) int64 {
	if val, ok := data[key]; ok {
		switch v := val.(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
	}
	return 0
}
