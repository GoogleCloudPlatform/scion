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

	"github.com/stretchr/testify/require"
)

// TestRunRemoteBrokerStatus_ProjectsErrorSurfaced verifies that a failed
// provider-list fetch is shown as an error, not silently rendered the same
// as a confirmed-empty list ("(none)" plus the misleading "run
// runtime-broker provide" hint).
func TestRunRemoteBrokerStatus_ProjectsErrorSurfaced(t *testing.T) {
	const brokerID = "11111111-1111-1111-1111-111111111111"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/"+brokerID:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     brokerID,
				"name":   "second-broker",
				"status": "online",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/"+brokerID+"/projects":
			// Simulate an authz gate (or any other transient Hub error)
			// failing on the provider-list call specifically, while the
			// broker record itself is readable.
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "forbidden",
					"message": "Insufficient permissions",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	savedProjectPath := projectPath
	savedOutputFormat := outputFormat
	t.Cleanup(func() {
		projectPath = savedProjectPath
		outputFormat = savedOutputFormat
	})
	outputFormat = ""
	projectPath = setupSecretProject(t, home, server.URL)

	output := captureStdout(t, func() {
		require.NoError(t, runRemoteBrokerStatus(brokerID))
	})

	require.Contains(t, output, "unknown - failed to fetch provider list",
		"a failed provider-list fetch must be labeled unknown, not (none); got: %s", output)
	require.Contains(t, output, "forbidden", "the underlying error should be visible; got: %s", output)
	require.NotContains(t, output, "(none)",
		"a failed fetch must never render identically to a confirmed-empty list; got: %s", output)
	require.NotContains(t, output, "runtime-broker provide",
		"the 'run provide' hint is misleading when the fetch itself failed; got: %s", output)
}
