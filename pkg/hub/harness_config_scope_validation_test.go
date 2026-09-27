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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCreateHarnessConfig_ScopeValidation verifies that createHarnessConfig
// accepts only the canonical scopes ("", "global", "project", "user") and
// rejects anything else — including the removed legacy "grove" scope and
// arbitrary garbage — with 400 instead of silently storing it and flattening
// its storage path.
func TestCreateHarnessConfig_ScopeValidation(t *testing.T) {
	srv, _ := testServer(t)

	valid := []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"empty defaults to global", "", ""},
		{"global", store.HarnessConfigScopeGlobal, ""},
		{"project", store.HarnessConfigScopeProject, tid("hc-scope-validation-project")},
		{"user", store.HarnessConfigScopeUser, ""},
	}
	for _, tt := range valid {
		t.Run("accepts/"+tt.name, func(t *testing.T) {
			body := CreateHarnessConfigRequest{
				Name:    "hc-" + tt.name,
				Harness: "claude",
				Scope:   tt.scope,
				ScopeID: tt.scopeID,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", body)
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		})
	}

	invalid := []string{"grove", "bogus"}
	for _, scope := range invalid {
		t.Run("rejects/"+scope, func(t *testing.T) {
			body := CreateHarnessConfigRequest{
				Name:    "hc-invalid-" + scope,
				Harness: "claude",
				Scope:   scope,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid scope", "body: %s", rec.Body.String())
		})
	}
}

// TestCreateHarnessConfig_RejectsGroveScopeCollision verifies the rejection
// for two different projects POSTing a harness config with the removed
// "grove" scope: ResourceStoragePath has no arm for "grove", so a stored
// "grove" scope would fall through to the flattened default path shared by
// every project. Both requests are rejected with 400, and neither harness
// config is stored.
func TestCreateHarnessConfig_RejectsGroveScopeCollision(t *testing.T) {
	srv, s := testServer(t)

	projA := tid("hc-collision-project-a")
	projB := tid("hc-collision-project-b")

	for _, projectID := range []string{projA, projB} {
		body := CreateHarnessConfigRequest{
			Name:    "shared-hc",
			Harness: "claude",
			Scope:   "grove",
			ScopeID: projectID,
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "invalid scope", "body: %s", rec.Body.String())
	}

	result, err := s.ListHarnessConfigs(t.Context(), store.HarnessConfigFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "no harness config should have been stored for either project")
}

// TestHandleHarnessConfigClone_ScopeValidation verifies the same up-front
// scope validation on the harness-config clone path: "", "global", "project"
// and "user" are all accepted (the clone authorization switch has an arm for
// each), while anything else — including the removed "grove" scope and
// arbitrary garbage — is rejected with 400, echoing the value, before it
// ever reaches the authorization switch.
func TestHandleHarnessConfigClone_ScopeValidation(t *testing.T) {
	srv, _ := testServer(t)

	// Create a source harness config to clone from.
	createBody := CreateHarnessConfigRequest{
		Name:    "clone-source",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", createBody)
	require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	var created CreateHarnessConfigResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotNil(t, created.HarnessConfig)

	valid := []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"empty defaults to source scope", "", ""},
		{"global", store.HarnessConfigScopeGlobal, ""},
		{"project", store.HarnessConfigScopeProject, tid("hc-scope-validation-clone-project")},
		{"user", store.HarnessConfigScopeUser, ""},
	}
	for _, tt := range valid {
		t.Run("accepts/"+tt.name, func(t *testing.T) {
			body := CloneTemplateRequest{
				Name:    "clone-" + tt.name,
				Scope:   tt.scope,
				ScopeID: tt.scopeID,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+created.HarnessConfig.ID+"/clone", body)
			assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		})
	}

	invalid := []string{"grove", "bogus"}
	for _, scope := range invalid {
		t.Run("rejects/"+scope, func(t *testing.T) {
			body := CloneTemplateRequest{
				Name:  "clone-invalid-" + scope,
				Scope: scope,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+created.HarnessConfig.ID+"/clone", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid scope", "body: %s", rec.Body.String())
		})
	}
}

// TestUpdateHarnessConfig_PreservesScopeAndOwnership verifies that
// updateHarnessConfig (PUT /api/v1/harness-configs/{id}) keeps the stored
// record's scope, scope ID, owner and storage location regardless of what
// the request body carries, the same way updateTemplateV2 does. A PUT is
// accepted (200) even when its body names a different scope, scope ID,
// owner, storage path, storage URI and storage bucket, and both the stored
// and the returned record keep the original values.
func TestUpdateHarnessConfig_PreservesScopeAndOwnership(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:            tid("hc-update-preserve"),
		Slug:          "hc-update-preserve",
		Name:          "Update Preserve Test",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeProject,
		ScopeID:       tid("hc-scope-update-project"),
		OwnerID:       tid("hc-scope-update-owner"),
		StoragePath:   "harness-configs/original-path/",
		StorageURI:    "gs://original-bucket/harness-configs/original-path/",
		StorageBucket: "original-bucket",
		Status:        store.HarnessConfigStatusActive,
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	body := store.HarnessConfig{
		Name:          "Updated Name",
		Slug:          hc.Slug,
		Harness:       hc.Harness,
		Scope:         store.HarnessConfigScopeUser,
		ScopeID:       tid("hc-scope-update-other-project"),
		OwnerID:       tid("hc-scope-update-other-owner"),
		StoragePath:   "harness-configs/attacker-path/",
		StorageURI:    "gs://attacker-bucket/harness-configs/attacker-path/",
		StorageBucket: "attacker-bucket",
		Status:        store.HarnessConfigStatusActive,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+hc.ID, body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var returned store.HarnessConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &returned))
	assert.Equal(t, hc.Scope, returned.Scope, "returned scope must be unchanged")
	assert.Equal(t, hc.ScopeID, returned.ScopeID, "returned scope ID must be unchanged")
	assert.Equal(t, hc.OwnerID, returned.OwnerID, "returned owner must be unchanged")
	assert.Equal(t, hc.StoragePath, returned.StoragePath, "returned storage path must be unchanged")
	assert.Equal(t, hc.StorageURI, returned.StorageURI, "returned storage URI must be unchanged")
	assert.Equal(t, hc.StorageBucket, returned.StorageBucket, "returned storage bucket must be unchanged")
	assert.Equal(t, "Updated Name", returned.Name)

	stored, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, hc.Scope, stored.Scope, "stored scope must be unchanged")
	assert.Equal(t, hc.StoragePath, stored.StoragePath, "stored storage path must be unchanged")
	assert.Equal(t, hc.StorageURI, stored.StorageURI, "stored storage URI must be unchanged")
	assert.Equal(t, hc.StorageBucket, stored.StorageBucket, "stored storage bucket must be unchanged")
	assert.Equal(t, hc.ScopeID, stored.ScopeID, "stored scope ID must be unchanged")
	assert.Equal(t, hc.OwnerID, stored.OwnerID, "stored owner must be unchanged")
	assert.Equal(t, "Updated Name", stored.Name)
}
