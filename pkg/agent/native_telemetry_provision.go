// Copyright 2026 The Scion Authors.

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Only enabled Claude selects a native metrics exporter by receiver backend.
// Other provisioners and disabled Claude keep the original staged environment.
func nativeTelemetryProvisionEnvForHarness(name, home string, telemetry *api.TelemetryConfig, env map[string]string, secrets []api.ResolvedSecret) (map[string]string, error) {
	if name != "claude" || telemetry == nil || (telemetry.Enabled != nil && !*telemetry.Enabled) {
		return env, nil
	}
	return nativeTelemetryProvisionEnv(home, telemetry, env, secrets)
}

// nativeTelemetryProvisionEnv stages the effective backend for native harness
// provisioning. Runtime GCP credential projection happens after provisioners
// run, so a nonsecret hint is needed to prevent a logs-first harness from
// accidentally enabling metrics against a GCP receiver.
func nativeTelemetryProvisionEnv(home string, telemetry *api.TelemetryConfig, env map[string]string, secrets []api.ResolvedSecret) (map[string]string, error) {
	for _, secret := range secrets {
		if secret.Type == "file" && strings.HasSuffix(filepath.Clean(secret.Target), filepath.Join(".scion", "telemetry-gcp-credentials.json")) && secret.Name != "scion-telemetry-gcp-credentials" {
			// A differently named file secret can materialize the receiver's
			// well-known credential path after provisioner execution.
			return nil, fmt.Errorf("late telemetry credential file target")
		}
		if secret.Type != "environment" && secret.Type != "" {
			continue
		}
		if secret.Target == "SCION_TELEMETRY_CLOUD_PROVIDER" || secret.Target == "SCION_OTEL_GCP_CREDENTIALS" {
			// Runtime projects these values only after provisioning. Their
			// values cannot be used to choose a native exporter safely.
			return nil, fmt.Errorf("late telemetry backend secret target: %s", secret.Target)
		}
	}
	staged := make(map[string]string, len(env)+1)
	for key, value := range env {
		staged[key] = value
	}
	const providerKey = "SCION_TELEMETRY_CLOUD_PROVIDER"
	configured := ""
	if telemetry != nil && telemetry.Cloud != nil {
		configured = telemetry.Cloud.Provider
	}
	provided := staged[providerKey]
	if configured != "" && provided != "" && configured != provided {
		return nil, fmt.Errorf("conflicting telemetry cloud provider")
	}
	if provided != "" {
		return staged, nil
	}
	if configured != "" {
		staged[providerKey] = configured
		return staged, nil
	}
	for _, secret := range secrets {
		if secret.Name == "scion-telemetry-gcp-credentials" && secret.Type == "file" {
			staged[providerKey] = "gcp"
			return staged, nil
		}
	}
	_, credentialErr := os.Stat(filepath.Join(home, ".scion", "telemetry-gcp-credentials.json"))
	if credentialErr != nil && !os.IsNotExist(credentialErr) {
		return nil, fmt.Errorf("cannot inspect telemetry credential indicator: %w", credentialErr)
	}
	if staged["SCION_OTEL_GCP_CREDENTIALS"] != "" || credentialErr == nil {
		staged[providerKey] = "gcp"
		return staged, nil
	}
	if (telemetry != nil && telemetry.Cloud != nil && telemetry.Cloud.Endpoint != "") || staged["SCION_OTEL_ENDPOINT"] != "" {
		staged[providerKey] = "otlp"
	}
	return staged, nil
}
