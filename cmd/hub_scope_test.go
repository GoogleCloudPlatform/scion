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
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestScopedGetWithoutKeyResolvesScopeOnce(t *testing.T) {
	const (
		projectID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		projectName = "named-project"
	)

	tests := []struct {
		name       string
		listPath   string
		listResult map[string]interface{}
		configure  func(*testing.T) *cobra.Command
		run        func(*cobra.Command) error
	}{
		{
			name:       "environment",
			listPath:   "/api/v1/env",
			listResult: map[string]interface{}{"envVars": []interface{}{}, "scope": "project", "scopeId": projectID},
			configure: func(t *testing.T) *cobra.Command {
				state := saveEnvTestState()
				t.Cleanup(state.restore)
				cmd := newTestScopeCommand(&envScope, &envProjectScope, &envBrokerScope)
				require.NoError(t, cmd.Flags().Set("project", projectName))
				return cmd
			},
			run: func(cmd *cobra.Command) error { return runEnvGet(cmd, nil) },
		},
		{
			name:       "secret",
			listPath:   "/api/v1/secrets",
			listResult: map[string]interface{}{"secrets": []interface{}{}, "scope": "project", "scopeId": projectID},
			configure: func(t *testing.T) *cobra.Command {
				state := saveSecretTestState()
				t.Cleanup(state.restore)
				cmd := newTestScopeCommand(&secretScope, &secretProjectScope, &secretBrokerScope)
				require.NoError(t, cmd.Flags().Set("project", projectName))
				return cmd
			},
			run: func(cmd *cobra.Command) error { return runSecretGet(cmd, nil) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.configure(t)

			var projectLookups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
					projectLookups.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"projects": []map[string]interface{}{{"id": projectID, "name": projectName, "slug": projectName}},
					})
				case r.Method == http.MethodGet && r.URL.Path == tt.listPath:
					_ = json.NewEncoder(w).Encode(tt.listResult)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_HUB_ENDPOINT", server.URL)
			projectPath = setupSecretProject(t, home, server.URL)

			require.NoError(t, tt.run(cmd))
			require.EqualValues(t, 1, projectLookups.Load())
		})
	}
}

func newTestScopeCommand(scope, projectScope, brokerScope *string) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVar(scope, "scope", "", "")
	cmd.Flags().StringVar(projectScope, "project", "", "")
	cmd.Flags().StringVar(brokerScope, "broker", "", "")
	return cmd
}
