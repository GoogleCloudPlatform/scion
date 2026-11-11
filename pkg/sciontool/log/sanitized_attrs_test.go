/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultSlogHandler_SanitizesErrorAttrs is the agent-log guard: an
// error attribute logged through the default slog handler, as a record
// attr or a WithAttrs-bound one, reaches agent.log sanitized and
// truncated, with none of its key-shaped values.
func TestDefaultSlogHandler_SanitizesErrorAttrs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	cleanup := setLogPathForTest(t, path)
	defer cleanup()
	origQuiet := quiet.Load()
	SetQuiet(true)
	defer SetQuiet(origQuiet)

	err := fmt.Errorf("request failed: %w", errors.New(
		"env FOO_API_KEY=sk-live-0123456789abcdefABCDEF "+
			"pull https://deploy:pw-value-123@registry.example.com/img "+
			strings.Repeat("output line\n", 150)))
	slog.New(defaultSlogHandler()).With("bound", err).Error("op failed", "error", err)

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read agent.log: %v", readErr)
	}
	out := string(data)
	for _, raw := range []string{"sk-live-0123456789abcdefABCDEF", "pw-value-123"} {
		if strings.Contains(out, raw) {
			t.Errorf("agent.log holds %q:\n%s", raw, out)
		}
	}
	for _, key := range []string{"bound=", "error="} {
		i := strings.Index(out, key)
		if i < 0 {
			t.Fatalf("agent.log lacks %q:\n%s", key, out)
		}
		rest := out[i+len(key):]
		for _, want := range []string{"FOO_API_KEY=[REDACTED]", "https://[REDACTED]@registry.example.com", "...[truncated, "} {
			if !strings.Contains(rest, want) {
				t.Errorf("%s value lacks %q", key, want)
			}
		}
	}
}
