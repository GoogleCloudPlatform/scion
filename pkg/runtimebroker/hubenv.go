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
	"fmt"
	"net"
	"net/url"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
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
	"SCION_GROVE_PATH":        {},
	"SCION_PROJECT_ID":        {},
	"SCION_PROJECT_PATH":      {},
	"SCION_HUB_ENDPOINT":      {},
	"SCION_HUB_URL":           {},
	"SCION_TELEMETRY_ENABLED": {},
}

func resolveHubEndpointForCreate(reqHubEndpoint, connectionHubEndpoint, brokerHubEndpoint string, resolvedEnv map[string]string, projectPath, containerHubEndpoint, runtimeName string) string {
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
		hubEndpoint = hubEndpointFromProjectSettings(projectPath)
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

func resolveHubEndpointForStart(brokerHubEndpoint string, resolvedEnv map[string]string, projectPath, containerHubEndpoint, runtimeName string) string {
	// Prefer the Hub-dispatched endpoint from resolved env — the Hub knows
	// its own public URL and injects it via SCION_HUB_ENDPOINT. The broker's
	// own HubEndpoint config may be a localhost address (e.g. combo server)
	// which would incorrectly trigger the container bridge override.
	hubEndpoint := hubEndpointFromResolvedEnv(resolvedEnv)
	if hubEndpoint == "" {
		hubEndpoint = brokerHubEndpoint
	}
	if hubEndpoint == "" {
		hubEndpoint = hubEndpointFromProjectSettings(projectPath)
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

func hubEndpointFromProjectSettings(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	settingsDir := resolveProjectSettingsDir(projectPath)
	projectSettings, err := config.LoadSettingsFromDir(settingsDir)
	if err != nil || projectSettings.IsHubExplicitlyDisabled() {
		return ""
	}
	return projectSettings.GetHubEndpoint()
}

// bridgeHostnames are the special Docker/Podman hostnames that resolve to the
// host's gateway. When the ContainerHubEndpoint uses one of these, the localhost
// endpoint's port must be grafted onto it; a real public domain is used as-is.
var bridgeHostnames = map[string]struct{}{
	"host.docker.internal":     {},
	"host.containers.internal": {},
}

func applyContainerBridgeOverride(endpoint, containerHubEndpoint, runtimeName string) string {
	if containerHubEndpoint == "" || runtimeName == "kubernetes" || !isLocalhostEndpoint(endpoint) {
		return endpoint
	}
	bridgeURL, err := url.Parse(containerHubEndpoint)
	if err != nil {
		return containerHubEndpoint
	}
	// When the override target is a public domain (colocated Docker routing
	// agents at the Caddy domain) rather than a bridge hostname, use it
	// wholesale. The domain's scheme/port (https, implicit 443) must be
	// preserved, not replaced with the localhost endpoint's port (e.g. combo
	// web port 8080).
	if _, isBridge := bridgeHostnames[bridgeURL.Hostname()]; !isBridge {
		return containerHubEndpoint
	}
	// Otherwise the override target is a bridge hostname (host.docker.internal).
	// Preserve the port from the actual endpoint rather than using the
	// pre-computed containerHubEndpoint wholesale. The containerHubEndpoint
	// is computed once at server startup and may have a different port
	// (e.g. standalone hub port 9810) than the endpoint being overridden
	// (e.g. combo-mode web port 8080).
	epURL, err := url.Parse(endpoint)
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

// colocatedExtraHosts returns --add-host entries needed when the hub and
// broker are co-located on the same machine. Docker bridge containers cannot
// reach the host's own public domain via hairpin NAT (e.g. on GCE), so we
// map the domain to host-gateway to route through the Docker bridge.
func colocatedExtraHosts(hubEndpoint string, isColocated bool, runtimeName string) []string {
	if !isColocated || runtimeName == "kubernetes" || hubEndpoint == "" || isLocalhostEndpoint(hubEndpoint) {
		return nil
	}
	u, err := url.Parse(hubEndpoint)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	return []string{host + ":host-gateway"}
}

// cloudrunSandboxHubEndpoint computes the hub endpoint for a sandbox on the
// cloudrun-sandbox runtime. Sandboxes cannot reach the hub's public IAP-fronted
// URL (no IAP credential), but the hub is on the same Instance, listening on
// 0.0.0.0, and reachable from the sandbox via the launcher's link-local address.
//
// When SCION_METADATA_BIND_ADDRESS is set to an explicit IP, that value is used
// directly instead of scanning interfaces. This is required on Cloud Run
// Instances that have multiple 169.254.x.x addresses, where auto-discovery is
// ambiguous. The value "link-local" triggers auto-discovery, same as empty.
//
// hubListenPort is the port the co-located hub HTTP server is listening on,
// plumbed from the server's configuration (--web-port or --port). This is the
// actual listen port, not parsed from an external URL which may have an
// implicit port (443 via HTTPS).
//
// Returns an error if link-local discovery fails (and no explicit address is
// set) or hubListenPort is zero (non-colocated broker) — the agent start must
// fail rather than fall back to a URL that will 302 from the IAP edge.
func cloudrunSandboxHubEndpoint(hubListenPort int) (string, error) {
	if hubListenPort == 0 {
		return "", fmt.Errorf("cloudrun-sandbox hub endpoint: hub listen port is not " +
			"configured (non-colocated broker cannot serve sandboxes)")
	}

	// SCION_METADATA_BIND_ADDRESS may specify an explicit link-local IP
	// when auto-discovery is ambiguous (multiple 169.254.x.x addresses on
	// Cloud Run Instances). The same address the metadata server binds to
	// is reachable by sandboxes — use it as the hub endpoint host.
	//
	// "link-local" means "auto-discover", same as empty. Any other non-empty
	// value is treated as an explicit IP.
	var linkLocal string
	if addr := os.Getenv("SCION_METADATA_BIND_ADDRESS"); addr != "" && addr != "link-local" {
		linkLocal = addr
	} else {
		var err error
		linkLocal, err = metadata.DiscoverLinkLocalAddress()
		if err != nil {
			return "", fmt.Errorf("cloudrun-sandbox hub endpoint: %w", err)
		}
	}

	return fmt.Sprintf("http://%s", net.JoinHostPort(linkLocal, fmt.Sprintf("%d", hubListenPort))), nil
}

func redactEnvValueForLog(key, value string) string {
	if _, ok := safeEnvLogKeys[key]; ok {
		return value
	}
	return redactedEnvValue
}
