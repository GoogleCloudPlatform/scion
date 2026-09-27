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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubEndpointFromResolvedEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "prefers endpoint key",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "https://primary.example.com",
				"SCION_HUB_URL":      "https://legacy.example.com",
			},
			want: "https://primary.example.com",
		},
		{
			name: "falls back to legacy url key",
			env: map[string]string{
				"SCION_HUB_URL": "https://legacy.example.com",
			},
			want: "https://legacy.example.com",
		},
		{
			name: "empty when neither key exists",
			env:  map[string]string{"UNRELATED": "x"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hubEndpointFromResolvedEnv(tt.env); got != tt.want {
				t.Fatalf("hubEndpointFromResolvedEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHubEndpointForCreatePrecedence(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	tests := []struct {
		name                 string
		req                  string
		connection           string
		broker               string
		resolved             map[string]string
		projectPath          string
		containerHubEndpoint string
		runtimeName          string
		want                 string
	}{
		{
			name:       "req endpoint takes priority",
			req:        "https://req.example.com",
			connection: "https://conn.example.com",
			broker:     "https://broker.example.com",
			want:       "https://req.example.com",
		},
		{
			name:       "connection fallback when req absent",
			connection: "https://conn.example.com",
			broker:     "https://broker.example.com",
			want:       "https://conn.example.com",
		},
		{
			name:   "broker fallback when req and connection absent",
			broker: "https://broker.example.com",
			want:   "https://broker.example.com",
		},
		{
			name:        "resolved env fallback",
			resolved:    map[string]string{"SCION_HUB_ENDPOINT": "https://resolved.example.com"},
			projectPath: projectDir,
			want:        "https://resolved.example.com",
		},
		{
			name:        "settings fallback when others absent",
			projectPath: projectDir,
			want:        "https://settings.example.com",
		},
		{
			name:                 "localhost req overridden by non-localhost connection",
			req:                  "http://localhost:8080",
			connection:           "https://hub.remote.example.com",
			broker:               "http://localhost:8080",
			containerHubEndpoint: "http://host.containers.internal:8080",
			runtimeName:          "podman",
			want:                 "https://hub.remote.example.com",
		},
		{
			name:                 "localhost req kept when connection is also localhost",
			req:                  "http://localhost:8080",
			connection:           "http://localhost:9090",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "podman",
			want:                 "http://host.containers.internal:8080",
		},
		{
			name:                 "localhost req kept when connection is empty",
			req:                  "http://localhost:8080",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "podman",
			want:                 "http://host.containers.internal:8080",
		},
		{
			name:                 "127.0.0.1 req overridden by non-localhost connection",
			req:                  "http://127.0.0.1:8080",
			connection:           "https://hub.remote.example.com",
			containerHubEndpoint: "http://host.docker.internal:8080",
			runtimeName:          "docker",
			want:                 "https://hub.remote.example.com",
		},
		{
			name:                 "non-localhost req preserved even with different connection",
			req:                  "https://hub1.example.com",
			connection:           "https://hub2.example.com",
			containerHubEndpoint: "http://host.containers.internal:8080",
			runtimeName:          "podman",
			want:                 "https://hub1.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rn := tt.runtimeName
			if rn == "" {
				rn = "docker"
			}
			got := resolveHubEndpointForCreate(tt.req, tt.connection, tt.broker, tt.resolved, tt.projectPath, tt.containerHubEndpoint, rn)
			if got != tt.want {
				t.Fatalf("resolveHubEndpointForCreate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyContainerBridgeOverride(t *testing.T) {
	tests := []struct {
		name                 string
		endpoint             string
		containerHubEndpoint string
		runtimeName          string
		want                 string
	}{
		{
			name:                 "localhost endpoint is rewritten for docker",
			endpoint:             "http://localhost:9810",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "docker",
			want:                 "http://host.containers.internal:9810",
		},
		{
			name:                 "kubernetes keeps localhost endpoint",
			endpoint:             "http://localhost:9810",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "kubernetes",
			want:                 "http://localhost:9810",
		},
		{
			name:                 "remote endpoint is unchanged",
			endpoint:             "https://hub.example.com",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "docker",
			want:                 "https://hub.example.com",
		},
		{
			name:                 "port preserved from endpoint when bridge port differs",
			endpoint:             "http://localhost:8080",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "podman",
			want:                 "http://host.containers.internal:8080",
		},
		{
			name:                 "same port preserved correctly",
			endpoint:             "http://localhost:9810",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "podman",
			want:                 "http://host.containers.internal:9810",
		},
		{
			name:                 "127.0.0.1 endpoint port preserved",
			endpoint:             "http://127.0.0.1:3000",
			containerHubEndpoint: "http://host.docker.internal:9810",
			runtimeName:          "docker",
			want:                 "http://host.docker.internal:3000",
		},
		{
			name:                 "no explicit port falls back to pre-computed",
			endpoint:             "http://localhost",
			containerHubEndpoint: "http://host.containers.internal:9810",
			runtimeName:          "podman",
			want:                 "http://host.containers.internal:9810",
		},
		{
			name:                 "domain container endpoint used wholesale, no port graft",
			endpoint:             "http://localhost:8080",
			containerHubEndpoint: "https://hub.example.com",
			runtimeName:          "docker",
			want:                 "https://hub.example.com",
		},
		{
			name:                 "domain container endpoint preserves its own explicit port",
			endpoint:             "http://localhost:8080",
			containerHubEndpoint: "https://hub.example.com:8443",
			runtimeName:          "docker",
			want:                 "https://hub.example.com:8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyContainerBridgeOverride(tt.endpoint, tt.containerHubEndpoint, tt.runtimeName)
			if got != tt.want {
				t.Fatalf("applyContainerBridgeOverride() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestColocatedExtraHosts(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		colocated bool
		runtime   string
		wantLen   int
		wantFirst string
	}{
		{
			name:      "colocated docker with public domain",
			endpoint:  "https://hub.example.com",
			colocated: true,
			runtime:   "docker",
			wantLen:   1,
			wantFirst: "hub.example.com:host-gateway",
		},
		{
			name:      "colocated docker with localhost",
			endpoint:  "http://localhost:8080",
			colocated: true,
			runtime:   "docker",
			wantLen:   0,
		},
		{
			name:      "colocated kubernetes",
			endpoint:  "https://hub.example.com",
			colocated: true,
			runtime:   "kubernetes",
			wantLen:   0,
		},
		{
			name:      "not colocated",
			endpoint:  "https://hub.example.com",
			colocated: false,
			runtime:   "docker",
			wantLen:   0,
		},
		{
			name:      "colocated docker with IP address",
			endpoint:  "https://34.30.80.76:443",
			colocated: true,
			runtime:   "docker",
			wantLen:   0,
		},
		{
			name:      "empty endpoint",
			endpoint:  "",
			colocated: true,
			runtime:   "docker",
			wantLen:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := colocatedExtraHosts(tt.endpoint, tt.colocated, tt.runtime)
			if len(got) != tt.wantLen {
				t.Fatalf("colocatedExtraHosts() returned %d entries, want %d: %v", len(got), tt.wantLen, got)
			}
			if tt.wantLen > 0 && got[0] != tt.wantFirst {
				t.Errorf("colocatedExtraHosts()[0] = %q, want %q", got[0], tt.wantFirst)
			}
		})
	}
}

func TestCloudrunSandboxHubEndpoint_ZeroPort(t *testing.T) {
	// Zero listen port means the hub's listen port is not known
	// (non-colocated broker). Must error, not guess.
	_, err := cloudrunSandboxHubEndpoint(0)
	if err == nil {
		t.Fatal("expected error when hub listen port is zero")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected 'not configured' error, got: %v", err)
	}
}

func TestCloudrunSandboxHubEndpoint_NeverRunApp(t *testing.T) {
	// Even if DiscoverLinkLocalAddress fails (no link-local in CI), the
	// function must never return a public *.run.app URL.
	ep, err := cloudrunSandboxHubEndpoint(8080)
	if err != nil {
		// Expected in CI — no link-local address.
		return
	}
	if strings.Contains(ep, "run.app") {
		t.Fatalf("cloudrunSandboxHubEndpoint must never return a run.app URL, got %q", ep)
	}
	if !strings.HasPrefix(ep, "http://169.254.") {
		t.Fatalf("expected http://169.254.x.x:<port>, got %q", ep)
	}
	if !strings.HasSuffix(ep, ":8080") {
		t.Fatalf("expected port 8080, got %q", ep)
	}
}

func TestCloudrunSandboxHubEndpoint_ExplicitBindAddress(t *testing.T) {
	// When SCION_METADATA_BIND_ADDRESS is set to an explicit IP, the function
	// must use it directly instead of calling DiscoverLinkLocalAddress. This
	// is required on Cloud Run Instances with multiple link-local addresses.
	t.Setenv("SCION_METADATA_BIND_ADDRESS", "169.254.8.1")

	ep, err := cloudrunSandboxHubEndpoint(8080)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "http://169.254.8.1:8080" {
		t.Fatalf("expected http://169.254.8.1:8080, got %q", ep)
	}
}

func TestCloudrunSandboxHubEndpoint_LinkLocalFallback(t *testing.T) {
	// "link-local" means auto-discover, same as empty. The function should
	// fall back to DiscoverLinkLocalAddress (which may fail in CI).
	t.Setenv("SCION_METADATA_BIND_ADDRESS", "link-local")

	ep, err := cloudrunSandboxHubEndpoint(8080)
	if err != nil {
		// Expected in CI — no link-local address.
		return
	}
	if !strings.HasPrefix(ep, "http://169.254.") {
		t.Fatalf("expected http://169.254.x.x:8080, got %q", ep)
	}
}

func TestResolveCloudRunServiceURL(t *testing.T) {
	okProjectID := func() (string, error) { return "721899303052", nil }
	okZone := func() (string, error) { return "us-central1-1", nil }

	tests := []struct {
		name       string
		kService   string
		projectFn  func() (string, error)
		zoneFn     func() (string, error)
		want       string
		wantErrSub string
	}{
		{
			name:      "constructs correct URL",
			kService:  "scion-hub",
			projectFn: okProjectID,
			zoneFn:    okZone,
			want:      "https://scion-hub-721899303052.us-central1.run.app",
		},
		{
			name:      "different region",
			kService:  "scion-hub",
			projectFn: okProjectID,
			zoneFn:    func() (string, error) { return "europe-west1-b", nil },
			want:      "https://scion-hub-721899303052.europe-west1.run.app",
		},
		{
			name:       "empty K_SERVICE",
			kService:   "",
			projectFn:  okProjectID,
			zoneFn:     okZone,
			wantErrSub: "K_SERVICE not set",
		},
		{
			name:       "numeric project ID error",
			kService:   "scion-hub",
			projectFn:  func() (string, error) { return "", fmt.Errorf("metadata unavailable") },
			zoneFn:     okZone,
			wantErrSub: "numeric project ID",
		},
		{
			name:       "zone error",
			kService:   "scion-hub",
			projectFn:  okProjectID,
			zoneFn:     func() (string, error) { return "", fmt.Errorf("metadata unavailable") },
			wantErrSub: "zone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCloudRunServiceURL(tt.kService, tt.projectFn, tt.zoneFn)
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveCloudRunServiceURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloudrunInstancesHubEndpoint_NoKService(t *testing.T) {
	// When K_SERVICE is not set, cloudrunInstancesHubEndpoint must fail.
	t.Setenv("K_SERVICE", "")
	_, err := cloudrunInstancesHubEndpoint(context.Background())
	if err == nil {
		t.Fatal("expected error when K_SERVICE is empty")
	}
	if !strings.Contains(err.Error(), "K_SERVICE not set") {
		t.Fatalf("expected 'K_SERVICE not set' error, got: %v", err)
	}
}

func TestRedactEnvValueForLog(t *testing.T) {
	if got := redactEnvValueForLog("SCION_AUTH_TOKEN", "secret-token"); got != redactedEnvValue {
		t.Fatalf("SCION_AUTH_TOKEN should be redacted, got %q", got)
	}
	if got := redactEnvValueForLog("SCION_BROKER_ID", "broker-1"); got != "broker-1" {
		t.Fatalf("SCION_BROKER_ID should remain visible, got %q", got)
	}
	if got := redactEnvValueForLog("SCION_HUB_ENDPOINT", "https://hub.example.com"); got != "https://hub.example.com" {
		t.Fatalf("SCION_HUB_ENDPOINT should remain visible, got %q", got)
	}
	if got := redactEnvValueForLog("SCION_HUB_URL", "https://hub.example.com"); got != "https://hub.example.com" {
		t.Fatalf("SCION_HUB_URL should remain visible, got %q", got)
	}
}

// TestResolveEffectiveHubEndpoint_AnchorRows pins a small set of
// human-readable, named scenarios for GoogleCloudPlatform/scion#1931: on the
// HTTP start and restart operations, the request-level HubEndpoint field
// ranks the same way it does on create — above the broker's own HubEndpoint
// and above a differing value in ResolvedEnv — while every other create-path
// behaviour (the connection-endpoint localhost rescue, the container bridge
// override, and the cloudrun-family runtime overrides) still applies. The
// full cross product lives in TestResolveEffectiveHubEndpoint_CrossProduct.
func TestResolveEffectiveHubEndpoint_AnchorRows(t *testing.T) {
	const (
		brokerLocalhost = "http://localhost:8080"
		dispatchPublic  = "https://hub.dispatched.example.com"
		dispatchLocal   = "http://localhost:9090"
		staleResolved   = "https://stale-in-resolved-env.example.com"
		connPublic      = "https://hub.connection.example.com"
	)

	tests := []struct {
		name string
		in   hubEndpointInputs
		want string
	}{
		{
			name: "http-start: the request-level endpoint ranks above the broker's own",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchPublic, BrokerHubEndpoint: brokerLocalhost, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-restart: the request-level endpoint ranks above the broker's own",
			in: hubEndpointInputs{
				Op: opHTTPRestart, ReqHubEndpoint: dispatchPublic, BrokerHubEndpoint: brokerLocalhost, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "create: the request-level endpoint ranks above the broker's own",
			in: hubEndpointInputs{
				Op: opCreate, ReqHubEndpoint: dispatchPublic, BrokerHubEndpoint: brokerLocalhost, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-start: the request-level endpoint ranks above a differing resolvedEnv value",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchPublic, BrokerHubEndpoint: brokerLocalhost,
				ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": staleResolved}, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-restart: the request-level endpoint ranks above a differing resolvedEnv value",
			in: hubEndpointInputs{
				Op: opHTTPRestart, ReqHubEndpoint: dispatchPublic, BrokerHubEndpoint: brokerLocalhost,
				ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": staleResolved}, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-start: resolvedEnv supplies the endpoint when the request field is absent",
			in: hubEndpointInputs{
				Op: opHTTPStart, ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": dispatchPublic}, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-restart: resolvedEnv supplies the endpoint when the request field is absent",
			in: hubEndpointInputs{
				Op: opHTTPRestart, ResolvedEnv: map[string]string{"SCION_HUB_ENDPOINT": dispatchPublic}, RuntimeName: "kubernetes",
			},
			want: dispatchPublic,
		},
		{
			name: "http-start: the connection endpoint rescues a localhost result",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchLocal, ConnectionHubEndpoint: connPublic, RuntimeName: "kubernetes",
			},
			want: connPublic,
		},
		{
			name: "http-start on docker: a localhost result gets the container bridge override",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchLocal,
				ContainerHubEndpoint: "http://host.docker.internal:9090", RuntimeName: "docker",
			},
			want: "http://host.docker.internal:9090",
		},
		{
			name: "http-start on kubernetes: the container bridge override never applies",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchLocal,
				ContainerHubEndpoint: "http://host.docker.internal:9090", RuntimeName: "kubernetes",
			},
			want: dispatchLocal,
		},
		{
			name: "http-start on cloudrun-sandbox: the sandbox override replaces the resolved value",
			in: hubEndpointInputs{
				Op: opHTTPStart, ReqHubEndpoint: dispatchPublic, RuntimeName: "cloudrun-sandbox", HubListenPort: 8080,
			},
			want: "http://203.0.113.5:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", "") // deterministic: no real Cloud Run environment in tests
			t.Setenv("SCION_METADATA_BIND_ADDRESS", "203.0.113.5")

			got, err := resolveEffectiveHubEndpoint(context.Background(), tt.in)
			if err != nil {
				t.Fatalf("resolveEffectiveHubEndpoint() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveEffectiveHubEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveEffectiveHubEndpoint_CrossProduct asserts, for every combination
// of broker endpoint, request-level endpoint, connection endpoint, and
// runtime, that http-start and http-restart resolve identically to create
// given the identical hubEndpointInputs (the same request field, the same
// ResolvedEnv, ...). This is a property (equality between two computations
// for every cell), not a table of expected strings, so it catches drift the
// anchor rows above cannot.
func TestResolveEffectiveHubEndpoint_CrossProduct(t *testing.T) {
	t.Setenv("K_SERVICE", "") // deterministic: no real Cloud Run environment in tests
	t.Setenv("SCION_METADATA_BIND_ADDRESS", "203.0.113.5")

	brokers := []string{"", "http://localhost:8080", "https://broker.example.com"}
	reqValues := []string{"", "http://localhost:9090", "https://hub.dispatched.example.com"}
	connections := []string{"", "http://localhost:7070", "https://hub.connection.example.com"}
	runtimes := []struct {
		name                 string
		containerHubEndpoint string
	}{
		{name: "docker", containerHubEndpoint: "http://host.docker.internal:9090"},
		{name: "kubernetes"},
		{name: "cloudrun"},
		{name: "cloudrun-sandbox"},
	}

	for _, broker := range brokers {
		for _, req := range reqValues {
			for _, connection := range connections {
				for _, rt := range runtimes {
					name := fmt.Sprintf("broker=%s/req=%s/connection=%s/runtime=%s", labelOrEmpty(broker), labelOrEmpty(req), labelOrEmpty(connection), rt.name)
					t.Run(name, func(t *testing.T) {
						// The same value fills both the request-level field and
						// ResolvedEnv, since a real Hub dispatch carries the
						// endpoint in both places.
						resolvedEnv := map[string]string{}
						if req != "" {
							resolvedEnv["SCION_HUB_ENDPOINT"] = req
						}
						base := hubEndpointInputs{
							ReqHubEndpoint:        req,
							BrokerHubEndpoint:     broker,
							ConnectionHubEndpoint: connection,
							ResolvedEnv:           resolvedEnv,
							ContainerHubEndpoint:  rt.containerHubEndpoint,
							RuntimeName:           rt.name,
							HubListenPort:         8080,
						}

						createIn := base
						createIn.Op = opCreate
						wantVal, wantErr := resolveEffectiveHubEndpoint(context.Background(), createIn)

						startIn := base
						startIn.Op = opHTTPStart
						gotStart, errStart := resolveEffectiveHubEndpoint(context.Background(), startIn)
						assertSameHubEndpointResult(t, "http-start vs create", gotStart, errStart, wantVal, wantErr)

						restartIn := base
						restartIn.Op = opHTTPRestart
						gotRestart, errRestart := resolveEffectiveHubEndpoint(context.Background(), restartIn)
						assertSameHubEndpointResult(t, "http-restart vs create", gotRestart, errRestart, wantVal, wantErr)
					})
				}
			}
		}
	}

	// The settings fallback (the loop above never sets ProjectPath) applies
	// to http-start and http-restart the same way it applies to create.
	t.Run("settings fallback supplies the endpoint for http-start and http-restart", func(t *testing.T) {
		projectDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}
		for _, op := range []startOperation{opHTTPStart, opHTTPRestart, opCreate} {
			t.Run(string(op), func(t *testing.T) {
				got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
					Op:          op,
					ProjectPath: projectDir,
					RuntimeName: "docker",
				})
				if err != nil {
					t.Fatalf("resolveEffectiveHubEndpoint() unexpected error: %v", err)
				}
				if got != "https://settings.example.com" {
					t.Errorf("resolveEffectiveHubEndpoint() = %q, want the settings-supplied endpoint", got)
				}
			})
		}
	})
}

// labelOrEmpty makes cross-product subtest names readable when an axis value
// is the empty string.
func labelOrEmpty(s string) string {
	if s == "" {
		return "<empty>"
	}
	return s
}

// assertSameHubEndpointResult asserts two resolveEffectiveHubEndpoint
// results (value and error) are identical, used to compare parity between
// operations that must resolve the same way for the same inputs.
func assertSameHubEndpointResult(t *testing.T, label string, got string, gotErr error, want string, wantErr error) {
	t.Helper()
	if (gotErr == nil) != (wantErr == nil) {
		t.Errorf("%s: error presence mismatch: got %v, want %v", label, gotErr, wantErr)
		return
	}
	if gotErr != nil {
		if gotErr.Error() != wantErr.Error() {
			t.Errorf("%s: error = %q, want %q", label, gotErr.Error(), wantErr.Error())
		}
		return
	}
	if got != want {
		t.Errorf("%s: got %q, want %q", label, got, want)
	}
}

// TestResolveEffectiveHubEndpoint_HTTPOpsResolvedEnvRanksBelowBroker verifies
// that on http-start and http-restart a ResolvedEnv SCION_HUB_URL ranks below
// the broker's own endpoint and is used only when broker, connection, and
// settings are all empty, as on create.
func TestResolveEffectiveHubEndpoint_HTTPOpsResolvedEnvRanksBelowBroker(t *testing.T) {
	const brokerPublic = "https://broker.example.com"
	const urlOnly = "https://from-scion-hub-url.example.com"

	for _, op := range []startOperation{opHTTPStart, opHTTPRestart} {
		t.Run(string(op)+": SCION_HUB_URL alone leaves the broker endpoint in place", func(t *testing.T) {
			got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
				Op:                op,
				BrokerHubEndpoint: brokerPublic,
				ResolvedEnv:       map[string]string{"SCION_HUB_URL": urlOnly},
				RuntimeName:       "docker",
			})
			if err != nil {
				t.Fatalf("resolveEffectiveHubEndpoint() unexpected error: %v", err)
			}
			if got != brokerPublic {
				t.Errorf("resolveEffectiveHubEndpoint() = %q, want the broker endpoint %q", got, brokerPublic)
			}
		})

		// The shared resolvedEnv fallback consults SCION_HUB_URL when
		// broker, connection and settings are all empty; pinned here so a
		// change to it is deliberate.
		t.Run(string(op)+": SCION_HUB_URL is the last resort when broker, connection and settings are all empty", func(t *testing.T) {
			got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
				Op:          op,
				ResolvedEnv: map[string]string{"SCION_HUB_URL": urlOnly},
				RuntimeName: "docker",
			})
			if err != nil {
				t.Fatalf("resolveEffectiveHubEndpoint() unexpected error: %v", err)
			}
			if got != urlOnly {
				t.Errorf("resolveEffectiveHubEndpoint() = %q, want %q (documented last-resort fallback)", got, urlOnly)
			}
		})
	}
}

// TestResolveEffectiveHubEndpoint_RequiresKnownOperation proves
// resolveEffectiveHubEndpoint has no usable default: the zero-value
// Operation and any operation it does not recognize both return an error
// rather than resolving as create.
func TestResolveEffectiveHubEndpoint_RequiresKnownOperation(t *testing.T) {
	tests := []struct {
		name string
		op   startOperation
	}{
		{name: "empty operation", op: ""},
		{name: "unknown operation", op: startOperation("bogus-operation")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
				Op:                tt.op,
				BrokerHubEndpoint: "https://broker.example.com",
				RuntimeName:       "docker",
			})
			if err == nil {
				t.Fatalf("resolveEffectiveHubEndpoint() with operation %q: expected an error, got nil", tt.op)
			}
			if !strings.Contains(err.Error(), "unknown start operation") {
				t.Errorf("resolveEffectiveHubEndpoint() error = %q, want it to contain %q", err.Error(), "unknown start operation")
			}
		})
	}
}

// TestResolveEffectiveHubEndpoint_CloudrunLocalhostOverride pins the
// cloudrun (CRI) runtime override at the attempted level: a localhost result
// is replaced only on the cloudrun runtime, and only when the result
// actually is localhost. K_SERVICE is set to empty, so the override itself
// always fails here (no real Cloud Run Instance metadata in tests) — an
// error return proves the override was attempted, and its absence proves it
// was skipped.
func TestResolveEffectiveHubEndpoint_CloudrunLocalhostOverride(t *testing.T) {
	t.Setenv("K_SERVICE", "")

	t.Run("cloudrun runtime with a localhost result: override is attempted", func(t *testing.T) {
		_, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
			Op: opCreate, BrokerHubEndpoint: "http://localhost:8080", RuntimeName: "cloudrun",
		})
		if err == nil {
			t.Fatal("expected an error: K_SERVICE is set to empty, so the cloudrun override cannot succeed")
		}
		if !strings.Contains(err.Error(), "Cloud Run instance") {
			t.Errorf("expected the cloudrun-instance override error, got: %v", err)
		}
	})

	t.Run("cloudrun runtime with a non-localhost result: override is skipped", func(t *testing.T) {
		got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
			Op: opCreate, BrokerHubEndpoint: "https://broker.example.com", RuntimeName: "cloudrun",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://broker.example.com" {
			t.Errorf("got %q, want the broker endpoint as-is (no override applies to a non-localhost result)", got)
		}
	})

	t.Run("non-cloudrun runtime with a localhost result: override is skipped", func(t *testing.T) {
		got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
			Op: opCreate, BrokerHubEndpoint: "http://localhost:8080", RuntimeName: "kubernetes",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "http://localhost:8080" {
			t.Errorf("got %q, want the localhost endpoint left alone on a non-cloudrun runtime", got)
		}
	})
}

// TestResolveEffectiveHubEndpoint_CloudrunLocalhostOverrideValue pins the
// cloudrun (CRI) override at the value level: a fake GCE metadata server lets
// the override actually succeed, so the replaced endpoint value itself is
// asserted, not just that the override was attempted.
func TestResolveEffectiveHubEndpoint_CloudrunLocalhostOverrideValue(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		switch r.URL.Path {
		case "/computeMetadata/v1/project/numeric-project-id":
			_, _ = w.Write([]byte("123456789"))
		case "/computeMetadata/v1/instance/zone":
			_, _ = w.Write([]byte("projects/123456789/zones/us-central1-1"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(md.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(md.URL, "http://"))
	t.Setenv("K_SERVICE", "scion-hub")

	const want = "https://scion-hub-123456789.us-central1.run.app"
	for _, tt := range []struct {
		name, runtime, broker, want string
	}{
		{"cloudrun, localhost: replaced", "cloudrun", "http://localhost:8080", want},
		{"cloudrun, non-localhost: kept", "cloudrun", "https://broker.example.com", "https://broker.example.com"},
		{"kubernetes, localhost: kept", "kubernetes", "http://localhost:8080", "http://localhost:8080"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
				Op: opCreate, BrokerHubEndpoint: tt.broker, RuntimeName: tt.runtime,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
