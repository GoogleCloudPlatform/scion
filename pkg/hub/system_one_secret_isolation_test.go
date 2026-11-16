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
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

func TestSystemOneSecretLifecycleAndGenericIsolation(t *testing.T) {
	ctx := context.Background()
	srv, s := testServerWithOps(t, nil)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret-signing-key")
	srv.SetSecretBackend(localBackend)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "s1-wire-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const (
		instID         = "22222222-2222-2222-2222-222222222222"
		sentinelSecret = "SENTINEL_WIRING_INTERNAL_KEY_4400"
	)

	secretKey, err := srv.setSystemOneInstanceSecret(ctx, instID, sentinelSecret, admin.ID)
	require.NoError(t, err)
	require.Equal(t, "systemone.instance."+instID, secretKey)

	sv, err := localBackend.Get(ctx, secretKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, sv.SecretType)
	assert.Equal(t, store.ScopeHub, sv.Scope)
	assert.Equal(t, sentinelSecret, sv.Value)

	// Generic /api/v1/secrets and /api/v1/env endpoints must neither list nor
	// read, overwrite, patch, or delete systemone.instance.<id> OR any
	// backend-sanitized collision form (e.g. systemone-instance-<id> or
	// SystemOne_Instance_<id> on GCP Secret Manager).
	listRec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.NotContains(t, listRec.Body.String(), secretKey)
	assert.NotContains(t, listRec.Body.String(), sentinelSecret)

	proj := &store.Project{
		ID:      tid("s1-wire-proj"),
		Name:    "Wire Proj",
		Slug:    "s1-wire-proj",
		OwnerID: admin.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	reservedForms := []string{
		secretKey,                                // canonical dotted: systemone.instance.<id>
		"systemone-instance-" + instID,           // GCP SM sanitized dashed form
		"SystemOne_Instance_" + instID,           // mixed-case / underscore form
		"systemone\u0130instance\u0130" + instID, // U+0130 (İ) separator collision on GCP SM
		"systemone\u212ainstance." + instID,      // U+212A (K) separator collision on GCP SM
	}
	newDesc := "tamper"
	for _, candidateKey := range reservedForms {
		esc := url.PathEscape(candidateKey)
		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/secrets/"+esc+"?scope=hub", nil).Code, "GET secret %q", candidateKey)
		assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/secrets/"+esc+"?scope=hub", SetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("OVERWRITE_ATTEMPT")),
		}).Code, "PUT secret %q", candidateKey)
		assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/secrets/"+esc+"?scope=hub", PatchSecretRequest{
			Description: &newDesc,
		}).Code, "PATCH secret %q", candidateKey)
		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/secrets/"+esc+"?scope=hub", nil).Code, "DELETE secret %q", candidateKey)

		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/env/"+esc+"?scope=hub", nil).Code, "GET env %q", candidateKey)
		assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/env/"+esc+"?scope=hub", map[string]any{"value": "x"}).Code, "PUT env %q", candidateKey)
		assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/env/"+esc+"?scope=hub", nil).Code, "DELETE env %q", candidateKey)

		assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/secrets/"+esc, SetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("PROJ_OVERWRITE")),
		}).Code, "PUT project secret %q", candidateKey)
	}

	// Stored internal secret remains intact.
	svAfter, err := localBackend.Get(ctx, secretKey, store.ScopeHub, srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, sentinelSecret, svAfter.Value)

	// Delete the underlying secret and verify Runtime classifies missing secret as FailAuth ("auth").
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider must not be contacted when credential resolution fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(tlsSrv.Close)
	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	require.NoError(t, srv.deleteSystemOneInstanceSecret(ctx, secretKey))
	_, err = localBackend.Get(ctx, secretKey, store.ScopeHub, srv.hubID)
	require.ErrorIs(t, err, store.ErrNotFound)

	inst := systemoneruntime.Instance{
		ID:       instID,
		Name:     "typesafe-missing-secret",
		Adapter:  "typesafe",
		Model:    "jev-1.14.0",
		Endpoint: tlsSrv.URL,
		Auth: systemoneruntime.InstanceAuth{
			Kind:       store.SystemOneAuthKindAPIKey,
			SecretName: secretKey,
		},
		Enabled:  true,
		Revision: 1,
	}
	_, listErr := srv.SystemOneRuntime().ListModels(ctx, inst)
	require.Error(t, listErr)
	pe, ok := systemoneprovider.AsProviderError(listErr)
	require.True(t, ok, "expected ProviderError, got %T (%v)", listErr, listErr)
	assert.Equal(t, systemoneprovider.FailAuth, pe.Reason)
	assert.NotContains(t, listErr.Error(), sentinelSecret)
}
