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
	"os"
	"path/filepath"
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

func TestResolveHubEndpointForStartPrecedence(t *testing.T) {
	groveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	tests := []struct {
		name                 string
		broker               string
		resolved             map[string]string
		projectPath            string
		containerHubEndpoint string
		want                 string
	}{
		{
			name:      "resolved env wins over broker",
			broker:    "https://broker.example.com",
			resolved:  map[string]string{"SCION_HUB_ENDPOINT": "https://resolved.example.com"},
			projectPath: groveDir,
			want:      "https://resolved.example.com",
		},
		{
			name:      "broker fallback when resolved env absent",
			broker:    "https://broker.example.com",
			resolved:  map[string]string{"UNRELATED": "x"},
			projectPath: groveDir,
			want:      "https://broker.example.com",
		},
		{
			name:      "resolved env wins over settings",
			resolved:  map[string]string{"SCION_HUB_URL": "https://resolved-legacy.example.com"},
			projectPath: groveDir,
			want:      "https://resolved-legacy.example.com",
		},
		{
			name:      "settings fallback when others absent",
			resolved:  map[string]string{"UNRELATED": "x"},
			projectPath: groveDir,
			want:      "https://settings.example.com",
		},
		{
			name:                 "production combo: resolved public URL prevents bridge override over localhost broker",
			broker:               "http://localhost:8080",
			resolved:             map[string]string{"SCION_HUB_ENDPOINT": "https://hub.production.example.com"},
			containerHubEndpoint: "http://host.docker.internal:8080",
			want:                 "https://hub.production.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveHubEndpointForStart(tt.broker, tt.resolved, tt.projectPath, tt.containerHubEndpoint, "docker")
			if got != tt.want {
				t.Fatalf("resolveHubEndpointForStart() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHubEndpointForCreatePrecedence(t *testing.T) {
	groveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	tests := []struct {
		name                 string
		req                  string
		connection           string
		broker               string
		resolved             map[string]string
		projectPath            string
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
			name:      "resolved env fallback",
			resolved:  map[string]string{"SCION_HUB_ENDPOINT": "https://resolved.example.com"},
			projectPath: groveDir,
			want:      "https://resolved.example.com",
		},
		{
			name:      "settings fallback when others absent",
			projectPath: groveDir,
			want:      "https://settings.example.com",
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
