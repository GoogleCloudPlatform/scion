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

package runtimebroker

import (
	"net"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const redactedEnvValue = "<redacted>"

var safeEnvLogKeys = map[string]struct{}{
	"SCION_AGENT_ID":          {},
	"SCION_AGENT_SLUG":        {},
	"SCION_BROKER_ID":         {},
	"SCION_BROKER_NAME":       {},
	"SCION_CREATOR":           {},
	"SCION_DEBUG":             {},
	"SCION_GROVE_ID":          {},
	"SCION_HUB_ENDPOINT":      {},
	"SCION_HUB_URL":           {},
	"SCION_TELEMETRY_ENABLED": {},
	// SCION_AGENT_TOKEN is excluded (redacted) — it is a JWT credential.
}

func resolveHubEndpointForCreate(reqHubEndpoint, connectionHubEndpoint, brokerHubEndpoint string, resolvedEnv map[string]string, grovePath, containerHubEndpoint, runtimeName string) string {
	hubEndpoint := reqHubEndpoint
	if hubEndpoint == "" {
		hubEndpoint = connectionHubEndpoint
	}
	if hubEndpoint == "" {
		hubEndpoint = brokerHubEndpoint
	}
	if hubEndpoint == "" {
		hubEndpoint = hubEndpointFromResolvedEnv(resolvedEnv)
	}
	if hubEndpoint == "" {
		hubEndpoint = hubEndpointFromGroveSettings(grovePath)
	}
	// A localhost endpoint from a remote hub dispatch refers to the hub
	// machine's loopback, not this broker's. When we have a non-localhost
	// connection endpoint (the URL this broker used to reach the hub),
	// prefer it since it is known to be reachable from this broker.
	if isLocalhostEndpoint(hubEndpoint) && connectionHubEndpoint != "" && !isLocalhostEndpoint(connectionHubEndpoint) {
		hubEndpoint = connectionHubEndpoint
	}
	return applyContainerBridgeOverride(hubEndpoint, containerHubEndpoint, runtimeName)
}

func resolveHubEndpointForStart(brokerHubEndpoint string, resolvedEnv map[string]string, grovePath, containerHubEndpoint, runtimeName string) string {
	// Prefer the Hub-dispatched endpoint from resolved env — the Hub knows
	// its own public URL and injects it via SCION_HUB_ENDPOINT. The broker's
	// own HubEndpoint config may be a localhost address (e.g. combo server)
	// which would incorrectly trigger the container bridge override.
	hubEndpoint := hubEndpointFromResolvedEnv(resolvedEnv)
	if hubEndpoint == "" {
		hubEndpoint = brokerHubEndpoint
	}
	if hubEndpoint == "" {
		hubEndpoint = hubEndpointFromGroveSettings(grovePath)
	}
	return applyContainerBridgeOverride(hubEndpoint, containerHubEndpoint, runtimeName)
}

func hubEndpointFromResolvedEnv(resolvedEnv map[string]string) string {
	if ep, ok := resolvedEnv["SCION_HUB_ENDPOINT"]; ok && ep != "" {
		return ep
	}
	if ep, ok := resolvedEnv["SCION_HUB_URL"]; ok && ep != "" {
		return ep
	}
	return ""
}

func hubEndpointFromGroveSettings(grovePath string) string {
	if grovePath == "" {
		return ""
	}
	settingsDir := resolveGroveSettingsDir(grovePath)
	groveSettings, err := config.LoadSettingsFromDir(settingsDir)
	if err != nil || groveSettings.IsHubExplicitlyDisabled() {
		return ""
	}
	return groveSettings.GetHubEndpoint()
}

func applyContainerBridgeOverride(endpoint, containerHubEndpoint, runtimeName string) string {
	if containerHubEndpoint == "" || runtimeName == "kubernetes" || !isLocalhostEndpoint(endpoint) {
		return endpoint
	}
	// Preserve the port from the actual endpoint rather than using the
	// pre-computed containerHubEndpoint wholesale. The containerHubEndpoint
	// is computed once at server startup and may have a different port
	// (e.g. standalone hub port 9810) than the endpoint being overridden
	// (e.g. combo-mode web port 8080).
	epURL, err := url.Parse(endpoint)
	if err != nil {
		return containerHubEndpoint
	}
	bridgeURL, err := url.Parse(containerHubEndpoint)
	if err != nil {
		return containerHubEndpoint
	}
	port := epURL.Port()
	if port == "" {
		// No explicit port in endpoint; fall back to the pre-computed value.
		return containerHubEndpoint
	}
	bridgeURL.Host = net.JoinHostPort(bridgeURL.Hostname(), port)
	return bridgeURL.String()
}

func redactEnvValueForLog(key, value string) string {
	if _, ok := safeEnvLogKeys[key]; ok {
		return value
	}
	return redactedEnvValue
}
