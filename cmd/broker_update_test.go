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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrokerUpdate_ForwardsAdminManagedFlag(t *testing.T) {
	for _, want := range []bool{true, false} {
		name := "approve"
		if !want {
			name = "revoke"
		}
		t.Run(name, func(t *testing.T) {
			var gotMethod, gotPath string
			var gotBody map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/broker-123":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
						ID:   "broker-123",
						Name: "build-host",
					})
				case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/runtime-brokers/broker-123":
					gotMethod = r.Method
					gotPath = r.URL.Path
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
					w.Header().Set("Content-Type", "application/json")
					eligible := want
					_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
						ID:             "broker-123",
						Name:           "build-host",
						AdminManaged:   want,
						PolicyEligible: &eligible,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			var out bytes.Buffer
			c := &cobra.Command{}
			c.SetOut(&out)

			err = runBrokerUpdateWithClient(context.Background(), c, client, "broker-123", want)
			require.NoError(t, err)
			assert.Equal(t, http.MethodPatch, gotMethod)
			assert.Equal(t, "/api/v1/runtime-brokers/broker-123", gotPath)
			require.Contains(t, gotBody, "adminManaged")
			assert.Equal(t, want, gotBody["adminManaged"])
			assert.Contains(t, out.String(), "build-host")
		})
	}
}

func TestBrokerUpdate_AgentAllowedFalse(t *testing.T) {
	assert.False(t, agentAllowed["runtime-broker.update"], "runtime-broker.update must not be allowed in agent mode")
}
