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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
)

func TestCheckDoctorHubConnectivity_SkipWhenEmpty(t *testing.T) {
	res := checkDoctorHubConnectivity("", nil)
	assert.Equal(t, "skip", res.Status)
	assert.Contains(t, res.Message, "No Hub endpoint configured")
}

func TestCheckDoctorHubConnectivity_AuthenticatedAndTrailingSlash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no double slash //healthz
		assert.Equal(t, "/healthz", r.URL.Path)
		assert.Equal(t, "Bearer test-user-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.HealthResponse{
			Status:  "healthy",
			Version: "0.1.0",
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL+"/", hubclient.WithBearerToken("test-user-token"))
	assert.NoError(t, err)

	// Pass URL with trailing slash and authenticated client
	res := checkDoctorHubConnectivity(server.URL+"/", client)
	assert.Equal(t, "pass", res.Status)
	assert.Contains(t, res.Message, "is healthy")
}

func TestCheckDoctorHubConnectivity_DegradedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.HealthResponse{
			Status: "degraded",
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	assert.NoError(t, err)

	res := checkDoctorHubConnectivity(server.URL, client)
	assert.Equal(t, "warn", res.Status)
	assert.Contains(t, res.Message, "is degraded")
}

// TestCheckDoctorHubConnectivity_Severity pins doctor to the hub's severity
// semantics (ptone/scion#1094): an unhealthy hub (critical check failing)
// must fail, not fall through to "healthy", on both the hubclient path and
// the raw HTTP path, and non-healthy checks are named.
func TestCheckDoctorHubConnectivity_Severity(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
		wantMsg    []string
	}{
		{"healthy", `{"status":"healthy","checks":{"database":"healthy"}}`, "pass", []string{"is healthy"}},
		{"degraded standalone", `{"status":"degraded","checks":{"database":"healthy","colocated_broker":"unhealthy: registration failed"}}`, "warn",
			[]string{"is degraded", "colocated_broker: unhealthy: registration failed"}},
		{"degraded composite", `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`, "warn",
			[]string{"is degraded", "colocated_broker: unhealthy: registration pending"}},
		{"unhealthy database", `{"status":"unhealthy","checks":{"database":"unhealthy"}}`, "fail",
			[]string{"is unhealthy", "database: unhealthy"}},
		{"unhealthy composite storage", `{"status":"unhealthy","web":{"status":"ok"},"hub":{"status":"unhealthy","checks":{"database":"healthy","workspace_storage":"unhealthy: mount not available"}}}`, "fail",
			[]string{"is unhealthy", "workspace_storage: unhealthy: mount not available"}},
		{"unknown status", `{"status":"weird"}`, "warn", []string{`reported status "weird"`}},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tt.body))
		}))

		client, err := hubclient.New(server.URL)
		assert.NoError(t, err)

		for _, path := range []struct {
			name   string
			client hubclient.Client
		}{{"hubclient", client}, {"raw", nil}} {
			t.Run(tt.name+"/"+path.name, func(t *testing.T) {
				res := checkDoctorHubConnectivity(server.URL, path.client)
				assert.Equal(t, tt.wantStatus, res.Status)
				for _, m := range tt.wantMsg {
					assert.Contains(t, res.Message, m)
				}
				if tt.wantStatus == "fail" {
					assert.Contains(t, res.Remediation, "critical check failing")
				}
			})
		}
		server.Close()
	}
}
