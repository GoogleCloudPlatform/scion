// Copyright 2026 The Scion Authors.

package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// nativeTelemetryProvisionEnv stages the effective backend for native harness
// provisioning. Runtime GCP credential projection happens after provisioners
// run, so a nonsecret hint is needed to prevent a logs-first harness from
// accidentally enabling metrics against a GCP receiver.
func nativeTelemetryProvisionEnv(home string, telemetry *api.TelemetryConfig, env map[string]string, secrets []api.ResolvedSecret) (map[string]string, error) {
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
