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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestScopedSecretByKeyLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		setup      func(*testing.T) (*Server, string)
		pathPrefix string
	}{
		{
			name:       "project",
			scope:      store.ScopeProject,
			setup:      setupProjectSecretTest,
			pathPrefix: "/api/v1/projects/",
		},
		{
			name:       "runtime broker",
			scope:      store.ScopeRuntimeBroker,
			setup:      setupBrokerSecretTest,
			pathPrefix: "/api/v1/runtime-brokers/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, scopeID := tc.setup(t)
			path := tc.pathPrefix + scopeID + "/secrets/SCOPED_SECRET"

			rec := doRequest(t, srv, http.MethodPut, path, SetSecretRequest{
				Value:         "secret-value",
				Encoding:      "raw",
				Description:   "scoped description",
				InjectionMode: store.InjectionModeAlways,
				Type:          store.SecretTypeVariable,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var setResp SetSecretResponse
			if err := json.NewDecoder(rec.Body).Decode(&setResp); err != nil {
				t.Fatalf("decode PUT response: %v", err)
			}
			if !setResp.Created || setResp.Secret == nil {
				t.Fatalf("PUT response = %+v", setResp)
			}
			assertScopedSecretMetadata(t, setResp.Secret, tc.scope, scopeID)

			rec = doRequest(t, srv, http.MethodGet, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var got store.Secret
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode GET response: %v", err)
			}
			assertScopedSecretMetadata(t, &got, tc.scope, scopeID)

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

func assertScopedSecretMetadata(t *testing.T, got *store.Secret, scope, scopeID string) {
	t.Helper()
	if got.Key != "SCOPED_SECRET" || got.Scope != scope || got.ScopeID != scopeID {
		t.Fatalf("secret identity = %+v", got)
	}
	if got.SecretType != store.SecretTypeVariable || got.Target != "SCOPED_SECRET" {
		t.Fatalf("secret type/target = %q/%q", got.SecretType, got.Target)
	}
	if got.Description != "scoped description" || got.InjectionMode != store.InjectionModeAlways {
		t.Fatalf("secret description/mode = %q/%q", got.Description, got.InjectionMode)
	}
	if got.CreatedBy != DevUserID || got.UpdatedBy != DevUserID {
		t.Fatalf("secret creator/updater = %q/%q", got.CreatedBy, got.UpdatedBy)
	}
}
