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

package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// portStatus represents the result of checking a port.
type portStatus struct {
	inUse         bool
	isScionServer bool
}

// checkPort checks if a port is already bound and if it's a scion server.
func checkPort(host string, port int) portStatus {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return portStatus{inUse: false}
	}

	// Port is in use - check if it's a scion server by hitting the health endpoint
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		return portStatus{inUse: true, isScionServer: false}
	}
	defer resp.Body.Close()

	// Check if the response looks like a scion health response
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Uptime  string `json:"uptime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return portStatus{inUse: true, isScionServer: false}
	}

	// If we got valid health response fields, it's a scion server
	if health.Status != "" && health.Uptime != "" {
		return portStatus{inUse: true, isScionServer: true}
	}

	return portStatus{inUse: true, isScionServer: false}
}

// isLocalhostURL returns true if the given URL refers to a loopback address.
func isLocalhostURL(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// containerBridgeEndpoint returns a container-accessible URL that replaces
// localhost in hubEndpoint with the appropriate bridge hostname for the given
// runtime. Returns "" if the endpoint is not localhost or the runtime does not
// need a bridge address (e.g. kubernetes).
func containerBridgeEndpoint(hubEndpoint, runtimeName string) string {
	var bridgeHost string
	switch runtimeName {
	case "podman", "container":
		bridgeHost = "host.containers.internal"
	case "docker":
		bridgeHost = "host.docker.internal"
	default:
		return ""
	}
	u, err := url.Parse(hubEndpoint)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return ""
	}
	u.Host = net.JoinHostPort(bridgeHost, u.Port())
	return u.String()
}

// logOAuthDebug logs OAuth configuration details for debugging.
// Secrets are redacted to only show whether they are set.
func logOAuthDebug(cfg *config.GlobalConfig) {
	slog.Debug("OAuth Configuration",
		"cli_google_client_id", redactForDebug(cfg.OAuth.CLI.Google.ClientID),
		"cli_google_client_secret", redactForDebug(cfg.OAuth.CLI.Google.ClientSecret),
		"cli_github_client_id", redactForDebug(cfg.OAuth.CLI.GitHub.ClientID),
		"cli_github_client_secret", redactForDebug(cfg.OAuth.CLI.GitHub.ClientSecret),
		"web_google_client_id", redactForDebug(cfg.OAuth.Web.Google.ClientID),
		"web_google_client_secret", redactForDebug(cfg.OAuth.Web.Google.ClientSecret),
		"web_github_client_id", redactForDebug(cfg.OAuth.Web.GitHub.ClientID),
		"web_github_client_secret", redactForDebug(cfg.OAuth.Web.GitHub.ClientSecret),
		"device_google_client_id", redactForDebug(cfg.OAuth.Device.Google.ClientID),
		"device_google_client_secret", redactForDebug(cfg.OAuth.Device.Google.ClientSecret),
		"device_github_client_id", redactForDebug(cfg.OAuth.Device.GitHub.ClientID),
		"device_github_client_secret", redactForDebug(cfg.OAuth.Device.GitHub.ClientSecret),
	)
}

// redactForDebug returns a redacted version of a secret for debug logging.
func redactForDebug(value string) string {
	if value == "" {
		return "(not set)"
	}
	if len(value) <= 8 {
		return "(set, " + fmt.Sprintf("%d", len(value)) + " chars)"
	}
	return value[:4] + "..." + value[len(value)-4:] + " (" + fmt.Sprintf("%d", len(value)) + " chars)"
}
