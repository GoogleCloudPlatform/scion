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
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	gcemetadata "cloud.google.com/go/compute/metadata"
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

// startOperation identifies which dispatch path is asking for a hub
// endpoint, so resolveEffectiveHubEndpoint selects its ranking explicitly
// rather than inferring one from request shape. There is no usable zero
// value: buildStartContext rejects an empty Operation, and
// resolveEffectiveHubEndpoint rejects any value it does not recognize.
type startOperation string

const (
	// opCreate: the request-level HubEndpoint (the Hub's endpoint, carried
	// on the create request) ranks first.
	opCreate startOperation = "create"
	// opHTTPStart and opHTTPRestart: the broker's HTTP startAgent and
	// restartAgent handlers. The Hub sends its endpoint as the same
	// request-level HubEndpoint field these requests carry, resolved with
	// the same ranking as opCreate.
	opHTTPStart   startOperation = "http-start"
	opHTTPRestart startOperation = "http-restart"
)

// valid reports whether op is one of the defined operations, so a caller can
// reject an empty or unrecognized value before any side effect rather than
// only inside resolveEffectiveHubEndpoint.
func (op startOperation) valid() bool {
	switch op {
	case opCreate, opHTTPStart, opHTTPRestart:
		return true
	default:
		return false
	}
}

// hubEndpointInputs bundles resolveEffectiveHubEndpoint's inputs by name, so
// each value is bound to a named field at the call site rather than by
// position.
type hubEndpointInputs struct {
	Op startOperation
	// ReqHubEndpoint is the request-level HubEndpoint field, ranked first by
	// opCreate, opHTTPStart, and opHTTPRestart alike.
	ReqHubEndpoint string
	// ConnectionHubEndpoint is the endpoint named by the request's
	// X-Scion-Hub-Connection header, when present.
	ConnectionHubEndpoint string
	// BrokerHubEndpoint is this broker's own configured HubEndpoint.
	BrokerHubEndpoint    string
	ResolvedEnv          map[string]string
	ProjectPath          string
	ContainerHubEndpoint string
	RuntimeName          string
	HubListenPort        int
}

// resolveEffectiveHubEndpoint resolves the hub endpoint to stamp into a
// dispatched agent for the given operation, then applies the cloudrun-family
// runtime overrides that follow endpoint resolution on every operation.
func resolveEffectiveHubEndpoint(ctx context.Context, in hubEndpointInputs) (string, error) {
	var hubEndpoint string
	switch in.Op {
	case opCreate, opHTTPStart, opHTTPRestart:
		hubEndpoint = resolveHubEndpointForCreate(
			in.ReqHubEndpoint,
			in.ConnectionHubEndpoint,
			in.BrokerHubEndpoint,
			in.ResolvedEnv,
			in.ProjectPath,
			in.ContainerHubEndpoint,
			in.RuntimeName,
		)
	default:
		return "", fmt.Errorf("resolveEffectiveHubEndpoint: unknown start operation %q", in.Op)
	}

	// On the cloudrun-sandbox runtime, sandboxes cannot reach the hub's
	// public IAP-fronted URL (they hold no IAP credential). The hub is on the
	// same Instance, listening on 0.0.0.0, and reachable via the launcher's
	// link-local address. Override the endpoint so the sandbox's sciontool
	// init (and the metadata emulator's FetchGCPToken) can reach the hub.
	// This overrides whatever the resolution above produced, on every
	// operation.
	if in.RuntimeName == "cloudrun-sandbox" {
		sandboxEndpoint, err := cloudrunSandboxHubEndpoint(in.HubListenPort)
		if err != nil {
			return "", fmt.Errorf("cannot resolve hub endpoint for sandbox: %w", err)
		}
		return sandboxEndpoint, nil
	}
	// On the cloudrun (CRI) runtime, standalone instances run on separate VMs
	// (potentially in different regions) and cannot reach the broker's
	// localhost endpoint. Unlike cloudrun-sandbox (co-located with the hub,
	// reachable via link-local), CRI instances need the hub's public Cloud
	// Run service URL. Resolve it from the K_SERVICE env var and GCE metadata.
	if in.RuntimeName == "cloudrun" && isLocalhostEndpoint(hubEndpoint) {
		criEndpoint, err := cloudrunInstancesHubEndpoint(ctx)
		if err != nil {
			return "", fmt.Errorf("cannot resolve hub endpoint for Cloud Run instance: %w", err)
		}
		return criEndpoint, nil
	}
	return hubEndpoint, nil
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

// cloudrunInstancesHubEndpoint computes the external hub endpoint for agents
// running on the cloudrun (Cloud Run Instances) runtime. Unlike cloudrun-sandbox
// (where the sandbox is co-located on the same Instance and can use a link-local
// address), CRI agents run on standalone VMs in potentially different regions
// and need the hub's public Cloud Run service URL.
//
// The URL is constructed from:
//   - K_SERVICE (env var set by Cloud Run, giving the service name)
//   - Numeric project ID (from GCE metadata server)
//   - Zone → region (from GCE metadata server, stripped to region)
//
// Returns an error if any of these cannot be resolved — the agent start must
// fail rather than fall back to a localhost URL that will never be reachable
// from a standalone CRI instance.
func cloudrunInstancesHubEndpoint(ctx context.Context) (string, error) {
	return resolveCloudRunServiceURL(
		os.Getenv("K_SERVICE"),
		func() (string, error) { return gcemetadata.NumericProjectIDWithContext(ctx) },
		func() (string, error) { return gcemetadata.ZoneWithContext(ctx) },
	)
}

// resolveCloudRunServiceURL constructs a Cloud Run service URL from the service
// name, numeric project ID, and zone. The zone is converted to a region by
// stripping the trailing segment (e.g. "us-central1-1" → "us-central1").
// Extracted from cloudrunInstancesHubEndpoint for testability.
func resolveCloudRunServiceURL(kService string, numericProjectIDFn func() (string, error), zoneFn func() (string, error)) (string, error) {
	if kService == "" {
		return "", fmt.Errorf("cloudrun hub endpoint: K_SERVICE not set (not running on Cloud Run)")
	}
	numericProjectID, err := numericProjectIDFn()
	if err != nil {
		return "", fmt.Errorf("cloudrun hub endpoint: numeric project ID: %w", err)
	}
	zone, err := zoneFn()
	if err != nil {
		return "", fmt.Errorf("cloudrun hub endpoint: zone: %w", err)
	}
	region := zone
	if idx := strings.LastIndex(zone, "-"); idx > 0 {
		region = zone[:idx]
	}
	return fmt.Sprintf("https://%s-%s.%s.run.app", kService, numericProjectID, region), nil
}

func redactEnvValueForLog(key, value string) string {
	if _, ok := safeEnvLogKeys[key]; ok {
		return value
	}
	return redactedEnvValue
}
