// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"context"
	"log/slog"
	"testing"

	gcplog "cloud.google.com/go/logging"
)

func TestSlogLevelToSeverity(t *testing.T) {
	tests := []struct {
		level    slog.Level
		expected gcplog.Severity
	}{
		{slog.LevelDebug, gcplog.Debug},
		{slog.LevelInfo, gcplog.Info},
		{slog.LevelWarn, gcplog.Warning},
		{slog.LevelError, gcplog.Error},
		// Levels between standard ones
		{slog.LevelDebug - 1, gcplog.Debug},
		{slog.LevelInfo + 1, gcplog.Info},
		{slog.LevelWarn + 1, gcplog.Warning},
		{slog.LevelError + 1, gcplog.Error},
	}

	for _, tt := range tests {
		t.Run(tt.level.String(), func(t *testing.T) {
			got := slogLevelToSeverity(tt.level)
			if got != tt.expected {
				t.Errorf("slogLevelToSeverity(%v) = %v, want %v", tt.level, got, tt.expected)
			}
		})
	}
}

func TestAddAttrToMap(t *testing.T) {
	t.Run("simple string", func(t *testing.T) {
		m := make(map[string]any)
		addAttrToMap(m, slog.String("key", "value"))
		if m["key"] != "value" {
			t.Errorf("expected key=value, got %v", m["key"])
		}
	})

	t.Run("simple int", func(t *testing.T) {
		m := make(map[string]any)
		addAttrToMap(m, slog.Int("count", 42))
		if m["count"] != int64(42) {
			t.Errorf("expected count=42, got %v (%T)", m["count"], m["count"])
		}
	})

	t.Run("nested group", func(t *testing.T) {
		m := make(map[string]any)
		addAttrToMap(m, slog.Group("request",
			slog.String("method", "GET"),
			slog.Int("status", 200),
		))

		group, ok := m["request"].(map[string]any)
		if !ok {
			t.Fatalf("expected request to be a map, got %T", m["request"])
		}
		if group["method"] != "GET" {
			t.Errorf("expected method=GET, got %v", group["method"])
		}
		if group["status"] != int64(200) {
			t.Errorf("expected status=200, got %v", group["status"])
		}
	})

	t.Run("inline group (empty key)", func(t *testing.T) {
		m := make(map[string]any)
		addAttrToMap(m, slog.Group("",
			slog.String("a", "1"),
			slog.String("b", "2"),
		))

		// Inline groups merge into parent
		if m["a"] != "1" {
			t.Errorf("expected a=1, got %v", m["a"])
		}
		if m["b"] != "2" {
			t.Errorf("expected b=2, got %v", m["b"])
		}
	})
}

func TestResolveProjectID(t *testing.T) {
	t.Run("SCION_GCP_PROJECT_ID takes priority", func(t *testing.T) {
		t.Setenv(EnvGCPProjectID, "scion-project")
		t.Setenv(EnvGoogleCloudProject, "google-project")

		got := resolveProjectID()
		if got != "scion-project" {
			t.Errorf("resolveProjectID() = %s, want scion-project", got)
		}
	})

	t.Run("falls back to GOOGLE_CLOUD_PROJECT", func(t *testing.T) {
		t.Setenv(EnvGCPProjectID, "")
		t.Setenv(EnvGoogleCloudProject, "google-project")

		got := resolveProjectID()
		if got != "google-project" {
			t.Errorf("resolveProjectID() = %s, want google-project", got)
		}
	})

	t.Run("returns empty when neither set", func(t *testing.T) {
		t.Setenv(EnvGCPProjectID, "")
		t.Setenv(EnvGoogleCloudProject, "")

		got := resolveProjectID()
		if got != "" {
			t.Errorf("resolveProjectID() = %s, want empty", got)
		}
	})
}

func TestResolveLogID(t *testing.T) {
	t.Run("custom log ID", func(t *testing.T) {
		t.Setenv(EnvCloudLoggingLogID, "custom-log")

		got := resolveLogID()
		if got != "custom-log" {
			t.Errorf("resolveLogID() = %s, want custom-log", got)
		}
	})

	t.Run("default log ID", func(t *testing.T) {
		t.Setenv(EnvCloudLoggingLogID, "")

		got := resolveLogID()
		if got != "scion-server" {
			t.Errorf("resolveLogID() = %s, want scion-server", got)
		}
	})
}

func TestIsCloudLoggingEnabled(t *testing.T) {
	tests := []struct {
		envVal   string
		expected bool
	}{
		{"true", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"", false},
		{"0", false},
		{"no", false},
		{"TRUE", false}, // Case sensitive
	}

	for _, tt := range tests {
		t.Run("value="+tt.envVal, func(t *testing.T) {
			t.Setenv(EnvCloudLogging, tt.envVal)
			got := isCloudLoggingEnabled()
			if got != tt.expected {
				t.Errorf("isCloudLoggingEnabled() = %v for %q, want %v", got, tt.envVal, tt.expected)
			}
		})
	}
}

func TestCloudHandler_Enabled(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
	}

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("should be enabled for Info level")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("should be enabled for Error level")
	}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("should not be enabled for Debug level when level is Info")
	}
}

func TestCloudHandler_WithAttrs(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
	}

	newH := h.WithAttrs([]slog.Attr{slog.String("key", "value")})

	ch, ok := newH.(*CloudHandler)
	if !ok {
		t.Fatal("WithAttrs should return a *CloudHandler")
	}
	if len(ch.attrs) != 1 {
		t.Errorf("expected 1 attr, got %d", len(ch.attrs))
	}
	if ch.component != "test" {
		t.Error("component should be preserved")
	}
	if ch.level != slog.LevelInfo {
		t.Error("level should be preserved")
	}

	// Original should be unchanged
	if len(h.attrs) != 0 {
		t.Error("original handler should not be modified")
	}
}

func TestCloudHandler_WithGroup(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
	}

	newH := h.WithGroup("mygroup")

	ch, ok := newH.(*CloudHandler)
	if !ok {
		t.Fatal("WithGroup should return a *CloudHandler")
	}
	if len(ch.groups) != 1 {
		t.Errorf("expected 1 group, got %d", len(ch.groups))
	}
	if ch.groups[0] != "mygroup" {
		t.Errorf("expected group 'mygroup', got %s", ch.groups[0])
	}

	// Original should be unchanged
	if len(h.groups) != 0 {
		t.Error("original handler should not be modified")
	}
}

func TestCloudHandler_WithAttrsChain(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
		attrs:     []slog.Attr{slog.String("existing", "attr")},
	}

	newH := h.WithAttrs([]slog.Attr{slog.String("new", "attr")}).(*CloudHandler)

	if len(newH.attrs) != 2 {
		t.Errorf("expected 2 attrs after chain, got %d", len(newH.attrs))
	}
}

func TestCloudHandler_WithGroupChain(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
		groups:    []string{"outer"},
	}

	newH := h.WithGroup("inner").(*CloudHandler)

	if len(newH.groups) != 2 {
		t.Errorf("expected 2 groups after chain, got %d", len(newH.groups))
	}
	if newH.groups[0] != "outer" || newH.groups[1] != "inner" {
		t.Errorf("expected [outer, inner], got %v", newH.groups)
	}
}

func TestEnvVarCloudLoggingConstants(t *testing.T) {
	if EnvCloudLogging != "SCION_CLOUD_LOGGING" {
		t.Errorf("EnvCloudLogging = %s, want SCION_CLOUD_LOGGING", EnvCloudLogging)
	}
	if EnvCloudLoggingLogID != "SCION_CLOUD_LOGGING_LOG_ID" {
		t.Errorf("EnvCloudLoggingLogID = %s, want SCION_CLOUD_LOGGING_LOG_ID", EnvCloudLoggingLogID)
	}
	if EnvGCPProjectID != "SCION_GCP_PROJECT_ID" {
		t.Errorf("EnvGCPProjectID = %s, want SCION_GCP_PROJECT_ID", EnvGCPProjectID)
	}
	if EnvGoogleCloudProject != "GOOGLE_CLOUD_PROJECT" {
		t.Errorf("EnvGoogleCloudProject = %s, want GOOGLE_CLOUD_PROJECT", EnvGoogleCloudProject)
	}
}

func TestResolveLogLevel(t *testing.T) {
	t.Run("debug flag", func(t *testing.T) {
		if ResolveLogLevel(true) != slog.LevelDebug {
			t.Error("expected LevelDebug when debug=true")
		}
	})

	t.Run("env var debug", func(t *testing.T) {
		t.Setenv("SCION_LOG_LEVEL", "debug")
		if ResolveLogLevel(false) != slog.LevelDebug {
			t.Error("expected LevelDebug when SCION_LOG_LEVEL=debug")
		}
	})

	t.Run("default info", func(t *testing.T) {
		t.Setenv("SCION_LOG_LEVEL", "")
		if ResolveLogLevel(false) != slog.LevelInfo {
			t.Error("expected LevelInfo by default")
		}
	})
}

func TestPromoteAttrToLabels(t *testing.T) {
	t.Run("agent_id promoted", func(t *testing.T) {
		labels := map[string]string{}
		promoteAttrToLabels(labels, slog.String(AttrAgentID, "agent-123"))
		if labels[AttrAgentID] != "agent-123" {
			t.Errorf("expected agent_id=agent-123, got %v", labels[AttrAgentID])
		}
	})

	t.Run("grove_id promoted", func(t *testing.T) {
		labels := map[string]string{}
		promoteAttrToLabels(labels, slog.String(AttrProjectID, "grove-456"))
		if labels[AttrProjectID] != "grove-456" {
			t.Errorf("expected grove_id=grove-456, got %v", labels[AttrProjectID])
		}
	})

	t.Run("empty values not promoted", func(t *testing.T) {
		labels := map[string]string{}
		promoteAttrToLabels(labels, slog.String(AttrAgentID, ""))
		if _, ok := labels[AttrAgentID]; ok {
			t.Error("empty agent_id should not be promoted")
		}
	})

	t.Run("broker_id promoted", func(t *testing.T) {
		labels := map[string]string{}
		promoteAttrToLabels(labels, slog.String(AttrBrokerID, "broker-west-1"))
		if labels[AttrBrokerID] != "broker-west-1" {
			t.Errorf("expected broker_id=broker-west-1, got %v", labels[AttrBrokerID])
		}
	})

	t.Run("unrelated attrs ignored", func(t *testing.T) {
		labels := map[string]string{}
		promoteAttrToLabels(labels, slog.String("other_key", "value"))
		if len(labels) != 0 {
			t.Errorf("expected no labels, got %v", labels)
		}
	})
}

func TestNewCloudHandler_NoProject(t *testing.T) {
	t.Setenv(EnvGCPProjectID, "")
	t.Setenv(EnvGoogleCloudProject, "")

	_, _, err := NewCloudHandler(context.Background(), CloudLoggingConfig{}, slog.LevelInfo)
	if err == nil {
		t.Error("expected error when no project ID available")
	}
}

func TestMapToCloudHTTPRequest(t *testing.T) {
	m := map[string]any{
		"requestMethod": "POST",
		"requestUrl":    "/api/v1/groves",
		"requestSize":   int64(256),
		"status":        int64(201),
		"responseSize":  int64(128),
		"userAgent":     "scion-cli/0.1.0",
		"remoteIp":      "10.0.0.1:54321",
		"referer":       "https://example.com",
		"latency":       "0.042s",
		"protocol":      "HTTP/1.1",
	}

	req := mapToCloudHTTPRequest(m)
	if req == nil {
		t.Fatal("expected non-nil HTTPRequest")
	}
	if req.Request.Method != "POST" {
		t.Errorf("expected method POST, got %s", req.Request.Method)
	}
	if req.Request.URL.String() != "/api/v1/groves" {
		t.Errorf("expected URL /api/v1/groves, got %s", req.Request.URL.String())
	}
	if req.RequestSize != 256 {
		t.Errorf("expected requestSize=256, got %d", req.RequestSize)
	}
	if req.Status != 201 {
		t.Errorf("expected status=201, got %d", req.Status)
	}
	if req.ResponseSize != 128 {
		t.Errorf("expected responseSize=128, got %d", req.ResponseSize)
	}
	if req.Request.UserAgent() != "scion-cli/0.1.0" {
		t.Errorf("expected userAgent=scion-cli/0.1.0, got %s", req.Request.UserAgent())
	}
	if req.RemoteIP != "10.0.0.1:54321" {
		t.Errorf("expected remoteIp=10.0.0.1:54321, got %s", req.RemoteIP)
	}
	if req.Request.Referer() != "https://example.com" {
		t.Errorf("expected referer=https://example.com, got %s", req.Request.Referer())
	}
	if req.Latency.Milliseconds() != 42 {
		t.Errorf("expected latency=42ms, got %v", req.Latency)
	}
	if req.Request.Proto != "HTTP/1.1" {
		t.Errorf("expected protocol=HTTP/1.1, got %s", req.Request.Proto)
	}
}

func TestMapToCloudHTTPRequest_EmptyMap(t *testing.T) {
	req := mapToCloudHTTPRequest(map[string]any{})
	if req == nil {
		t.Fatal("expected non-nil HTTPRequest even for empty map")
	}
	if req.Request == nil {
		t.Fatal("expected non-nil underlying Request")
	}
	if req.Status != 0 {
		t.Errorf("expected status=0, got %d", req.Status)
	}
}

func TestCloudHandler_HostnameField(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
		hostname:  "my-workstation",
	}

	// Verify hostname is preserved through WithAttrs
	newH := h.WithAttrs([]slog.Attr{slog.String("key", "value")}).(*CloudHandler)
	if newH.hostname != "my-workstation" {
		t.Errorf("expected hostname=my-workstation, got %s", newH.hostname)
	}

	// Verify hostname is preserved through WithGroup
	newH = h.WithGroup("grp").(*CloudHandler)
	if newH.hostname != "my-workstation" {
		t.Errorf("expected hostname=my-workstation, got %s", newH.hostname)
	}
}
