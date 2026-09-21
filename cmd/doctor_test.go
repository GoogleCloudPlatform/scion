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
