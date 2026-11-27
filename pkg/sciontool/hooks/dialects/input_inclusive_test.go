/*
Copyright 2026 The Scion Authors.
*/

package dialects

import "testing"

func TestMappingDialect_InputTokensInclusive(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "test-inclusive",
		EventNameField: "hook_event_name",
		Mappings: map[string]MappingEntrySpec{
			"PostLLMCall": {
				Event:                "model-end",
				InputTokensInclusive: true,
			},
		},
	}
	md := NewMappingDialect(spec)

	t.Run("subtracts cached and cache_write from nested usage", func(t *testing.T) {
		ev, err := md.Parse(map[string]interface{}{
			"hook_event_name": "PostLLMCall",
			"usage": map[string]interface{}{
				"input_tokens":       float64(1000),
				"output_tokens":      float64(300),
				"cached_tokens":      float64(250),
				"cache_write_tokens": float64(100),
				"reasoning_tokens":   float64(80),
			},
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ev.Data.InputTokens != 650 {
			t.Errorf("InputTokens = %d, want 650", ev.Data.InputTokens)
		}
		if !ev.Data.InputTokensDerived {
			t.Error("InputTokensDerived = false, want true")
		}
		if ev.Data.CachedTokens != 250 || ev.Data.CacheWriteTokens != 100 || ev.Data.OutputTokens != 300 || ev.Data.ReasoningTokens != 80 {
			t.Errorf("unexpected token fields: %+v", ev.Data)
		}
	})

	t.Run("recognizes cache_read_tokens alias and clamps at zero", func(t *testing.T) {
		ev, err := md.Parse(map[string]interface{}{
			"hook_event_name": "PostLLMCall",
			"usage": map[string]interface{}{
				"input_tokens":      float64(200),
				"output_tokens":     float64(50),
				"cache_read_tokens": float64(250),
			},
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ev.Data.CachedTokens != 250 {
			t.Errorf("CachedTokens = %d, want 250", ev.Data.CachedTokens)
		}
		if ev.Data.InputTokens != 0 {
			t.Errorf("InputTokens = %d, want 0 (clamped)", ev.Data.InputTokens)
		}
		if !ev.Data.InputTokensDerived {
			t.Error("InputTokensDerived = false, want true")
		}
	})

	t.Run("explicit zero input_tokens sets InputTokensDerived true and preserves presence", func(t *testing.T) {
		ev, err := md.Parse(map[string]interface{}{
			"hook_event_name": "PostLLMCall",
			"usage": map[string]interface{}{
				"input_tokens":  float64(0),
				"output_tokens": float64(0),
				"cached_tokens": float64(0),
			},
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ev.Data.InputTokens != 0 || !ev.Data.HasInputTokens() || !ev.Data.InputTokensDerived {
			t.Errorf("InputTokens=%d has=%v InputTokensDerived=%v, want 0/true/true", ev.Data.InputTokens, ev.Data.HasInputTokens(), ev.Data.InputTokensDerived)
		}
	})

	t.Run("absent input_tokens leaves InputTokensDerived false", func(t *testing.T) {
		ev, err := md.Parse(map[string]interface{}{
			"hook_event_name": "PostLLMCall",
			"usage": map[string]interface{}{
				"output_tokens": float64(10),
				"cached_tokens": float64(0),
			},
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if ev.Data.InputTokens != 0 || ev.Data.HasInputTokens() || ev.Data.InputTokensDerived {
			t.Errorf("InputTokens=%d has=%v InputTokensDerived=%v, want 0/false/false", ev.Data.InputTokens, ev.Data.HasInputTokens(), ev.Data.InputTokensDerived)
		}
	})
}
