/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"testing"
)

// Ambient agent identity values must not leak into a handler test's result
// once scrubHubEnv has run. The values are synthetic, standing in for what
// an agent container sets in its real environment.
func TestScrubHubEnv_ClearsAmbientAgentAndProject(t *testing.T) {
	ambient := map[string]string{
		"SCION_AGENT_ID":   "ambient-agent-id",
		"SCION_PROJECT":    "ambient-project",
		"SCION_PROJECT_ID": "ambient-project-id",
	}
	for k, v := range ambient {
		t.Setenv(k, v)
	}

	scrubHubEnv(t)

	for k := range ambient {
		if got := os.Getenv(k); got != "" {
			t.Errorf("after scrubHubEnv, %s = %q, want empty", k, got)
		}
	}

	h := NewTelemetryHandler(nil, nil, nil)
	for _, attr := range h.metricAttrs() {
		switch string(attr.Key) {
		case "agent_id", "project_id":
			t.Errorf("metricAttrs() carries ambient %s = %q after scrubHubEnv", attr.Key, attr.Value.AsString())
		}
	}
}
