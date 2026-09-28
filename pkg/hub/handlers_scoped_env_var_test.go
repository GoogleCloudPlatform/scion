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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestScopedEnvVarByKeyLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		setup func(*testing.T, *Server, store.Store) (string, string)
	}{
		{
			name:  "project",
			scope: store.ScopeProject,
			setup: func(t *testing.T, _ *Server, s store.Store) (string, string) {
				t.Helper()
				project := &store.Project{
					ID:      tid("scoped-env-project"),
					Name:    "Scoped Env Project",
					Slug:    "scoped-env-project",
					OwnerID: DevUserID,
					Created: time.Now(),
					Updated: time.Now(),
				}
				if err := s.CreateProject(context.Background(), project); err != nil {
					t.Fatalf("CreateProject: %v", err)
				}
				return project.ID, "/api/v1/projects/" + project.ID + "/env/SCOPED_KEY"
			},
		},
		{
			name:  "runtime broker",
			scope: store.ScopeRuntimeBroker,
			setup: func(t *testing.T, _ *Server, s store.Store) (string, string) {
				t.Helper()
				grantDevUserRuntimeBrokerAccess(t, s)
				broker := &store.RuntimeBroker{
					ID:      tid("scoped-env-broker"),
					Name:    "Scoped Env Broker",
					Slug:    "scoped-env-broker",
					Status:  store.BrokerStatusOnline,
					Created: time.Now(),
					Updated: time.Now(),
				}
				if err := s.CreateRuntimeBroker(context.Background(), broker); err != nil {
					t.Fatalf("CreateRuntimeBroker: %v", err)
				}
				return broker.ID, "/api/v1/runtime-brokers/" + broker.ID + "/env/SCOPED_KEY"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
			scopeID, path := tc.setup(t, srv, s)

			rec := doRequest(t, srv, http.MethodPut, path, SetEnvVarRequest{
				Value:     "plain-value",
				Sensitive: true,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("plain PUT status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var setResp SetEnvVarResponse
			if err := json.NewDecoder(rec.Body).Decode(&setResp); err != nil {
				t.Fatalf("decode plain PUT response: %v", err)
			}
			if !setResp.Created || setResp.EnvVar.Scope != tc.scope || setResp.EnvVar.ScopeID != scopeID {
				t.Fatalf("plain PUT response = %+v", setResp)
			}
			if setResp.EnvVar.Value != "********" || setResp.EnvVar.InjectionMode != store.InjectionModeAsNeeded {
				t.Fatalf("plain PUT value/mode = %q/%q", setResp.EnvVar.Value, setResp.EnvVar.InjectionMode)
			}

			rec = doRequest(t, srv, http.MethodGet, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("plain GET status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var envVar store.EnvVar
			if err := json.NewDecoder(rec.Body).Decode(&envVar); err != nil {
				t.Fatalf("decode plain GET response: %v", err)
			}
			if envVar.Value != "********" || envVar.Scope != tc.scope || envVar.ScopeID != scopeID {
				t.Fatalf("plain GET response = %+v", envVar)
			}

			rec = doRequest(t, srv, http.MethodPut, path, SetEnvVarRequest{
				Value:         "secret-value",
				Secret:        true,
				InjectionMode: store.InjectionModeAlways,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("secret PUT status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if _, err := s.GetEnvVar(context.Background(), "SCOPED_KEY", tc.scope, scopeID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("plain env var remains after promotion: %v", err)
			}

			rec = doRequest(t, srv, http.MethodGet, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("secret GET status = %d, body = %s", rec.Code, rec.Body.String())
			}
			envVar = store.EnvVar{}
			if err := json.NewDecoder(rec.Body).Decode(&envVar); err != nil {
				t.Fatalf("decode secret GET response: %v", err)
			}
			if !envVar.Secret || envVar.Value != "********" || envVar.Scope != tc.scope || envVar.ScopeID != scopeID {
				t.Fatalf("secret GET response = %+v", envVar)
			}

			rec = doRequest(t, srv, http.MethodDelete, path, nil)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE status = %d, body = %s", rec.Code, rec.Body.String())
			}
			rec = doRequest(t, srv, http.MethodGet, path, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET after DELETE status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
