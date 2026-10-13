/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// telemetryConfigEnvKeys lists the telemetry configuration variables, beyond
// the keys scrubHubEnv clears, that the telemetry handler
// reads from the environment. An agent container may set them ambiently.
var telemetryConfigEnvKeys = []string{
	"SCION_USAGE_SOURCE",
	"SCION_MODEL",
}

// scrubTelemetryEnv clears every environment variable the telemetry handler
// reads, so a telemetry test gives the same result wherever it runs. Tests
// that need a value set it with t.Setenv after calling this.
func scrubTelemetryEnv(t *testing.T) {
	t.Helper()
	scrubHubEnv(t)
	for _, key := range telemetryConfigEnvKeys {
		t.Setenv(key, "")
	}
}

// Ambient agent identity values must not leak into a handler test's result
// once scrubHubEnv has run. The values are synthetic, standing in for what
// an agent container sets in its real environment.
func TestScrubHubEnv_ClearsAmbientAgentAndProject(t *testing.T) {
	ambient := map[string]string{
		"SCION_AGENT_ID":   "ambient-agent-id",
		"SCION_HARNESS":    "ambient-harness",
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
		case "agent_id", "project_id", telemetrycontract.HarnessLabel:
			t.Errorf("metricAttrs() carries ambient %s = %q after scrubHubEnv", attr.Key, attr.Value.AsString())
		}
	}
}

// scrubTelemetryEnv clears every key it covers: the shared Hub keys, the
// agent identity keys and the telemetry configuration keys.
func TestScrubTelemetryEnv_ClearsAllScrubbedKeys(t *testing.T) {
	var keys []string
	keys = append(keys, testutil.HubEnvKeys()...)
	keys = append(keys, agentIdentityEnvKeys...)
	keys = append(keys, telemetryConfigEnvKeys...)
	for _, k := range keys {
		t.Setenv(k, "ambient-value")
	}

	scrubTelemetryEnv(t)

	for _, k := range keys {
		if got := os.Getenv(k); got != "" {
			t.Errorf("after scrubTelemetryEnv, %s = %q, want empty", k, got)
		}
	}
}
