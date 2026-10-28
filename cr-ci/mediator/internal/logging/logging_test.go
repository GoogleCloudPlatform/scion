package logging

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestCloudLoggingFields(t *testing.T) {
	var b strings.Builder
	New(&b, slog.LevelDebug).Warn("hello", "event", EventDispatch)
	var m map[string]any
	if err := json.Unmarshal([]byte(b.String()), &m); err != nil {
		t.Fatalf("not JSON: %q", b.String())
	}
	if m["severity"] != "WARNING" || m["message"] != "hello" || m["event"] != EventDispatch {
		t.Errorf("got %v", m)
	}
	if _, ok := m["level"]; ok {
		t.Errorf("level key not renamed: %v", m)
	}
}
