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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3335: `scion start --service-account <sa>` gives a uniform
// response regardless of cause, and its wording covers both causes: the
// account is not registered in this project, or the caller is not authorized
// to use it. That both causes give the same response is pinned per surface in
// sa_existence_oracle_test.go; this file pins the exact text.

// wantSAUnavailableText is spelled out rather than read from
// msgSANotAvailableInProject, so an edit to the const fails here.
const wantSAUnavailableText = "GCP service account is not available; it is not registered in this project or you are not authorized to use it"

func requireErrorText(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, wantCode, resp.Error.Code)
	assert.Equal(t, wantSAUnavailableText, resp.Error.Message)
}

// TestSAUnavailableMessage_ExactTextOnAllThreeSurfaces pins the exact message
// on agent create, agent PATCH and the project default setting. Create and
// PATCH answer validation_error; the project default setting answers
// invalid_request. The message text is the same on all three.
func TestSAUnavailableMessage_ExactTextOnAllThreeSurfaces(t *testing.T) {
	assert.Equal(t, wantSAUnavailableText, msgSANotAvailableInProject)

	t.Run("agent create", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := createAgentAsOwner(t, f, CreateAgentRequest{
			Name: "sa-text-create",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: uuid.New().String(),
			},
		})
		requireErrorText(t, rec, ErrCodeValidationError)
	})

	t.Run("agent patch", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		a := pendingAgentForPatch(t, f, "sa-text-patch")
		rec := patchAgentSAAsOwner(t, f, a.ID, uuid.New().String())
		requireErrorText(t, rec, ErrCodeValidationError)
	})

	t.Run("project default setting", func(t *testing.T) {
		srv, s := testServer(t)
		project := createTestProjectForSettings(t, s)
		rec := putProjectDefaultSA(t, srv, project.ID, uuid.New().String())
		requireErrorText(t, rec, ErrCodeInvalidRequest)
	})
}

func putProjectDefaultSA(t *testing.T, srv *Server, projectID, saID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/settings",
		hubclient.ProjectSettings{
			DefaultGCPIdentityMode:             string(store.GCPMetadataModeAssign),
			DefaultGCPIdentityServiceAccountID: saID,
		})
}

// wrappedNotFoundSAStore answers every service account lookup with a wrapped
// store.ErrNotFound, as a store layer that adds context to its errors would.
type wrappedNotFoundSAStore struct {
	store.Store
}

func (w *wrappedNotFoundSAStore) GetGCPServiceAccount(_ context.Context, id string) (*store.GCPServiceAccount, error) {
	return nil, fmt.Errorf("get gcp service account %s: %w", id, store.ErrNotFound)
}

// TestProjectDefaultSA_WrappedNotFoundMatchesUnreachable checks that a wrapped
// not-found from the store gets the same response on the project default
// setting as an account registered in another project, and not a 404.
func TestProjectDefaultSA_WrappedNotFoundMatchesUnreachable(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	elsewhere := &store.Project{
		ID:   tid("sa-wrapped-other-project"),
		Name: "Wrapped Other",
		Slug: "sa-wrapped-other-project",
	}
	require.NoError(t, s.CreateProject(t.Context(), elsewhere))
	unreachable := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   elsewhere.ID,
		Email:     "sa-wrapped-elsewhere@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), unreachable))

	other := probe(putProjectDefaultSA(t, srv, project.ID, unreachable.ID))

	srv.store = &wrappedNotFoundSAStore{Store: s}
	wrapped := probe(putProjectDefaultSA(t, srv, project.ID, uuid.New().String()))

	assert.NotEqual(t, http.StatusNotFound, wrapped.status, "body: %s", wrapped.body)
	require.Equal(t, http.StatusBadRequest, other.status, "body: %s", other.body)
	requireIndistinguishable(t, wrapped, other)
}

// TestProfileDefaultSA_WrappedNotFoundMatchesUnreachable checks the same for
// the per-profile default map: a wrapped not-found from the store on a
// defaultGCPIdentityServiceAccountIDByProfile entry gets the same status and
// body as an entry naming an account registered in another project, and not
// a 404.
func TestProfileDefaultSA_WrappedNotFoundMatchesUnreachable(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	elsewhere := &store.Project{
		ID:   tid("sa-wrapped-profile-other-project"),
		Name: "Wrapped Profile Other",
		Slug: "sa-wrapped-profile-other-project",
	}
	require.NoError(t, s.CreateProject(t.Context(), elsewhere))
	unreachable := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   elsewhere.ID,
		Email:     "sa-wrapped-profile-elsewhere@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), unreachable))

	putProfileDefault := func(saID string) *httptest.ResponseRecorder {
		t.Helper()
		return doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
			hubclient.ProjectSettings{
				DefaultGCPIdentityServiceAccountIDByProfile: map[string]string{"gpu": saID},
			})
	}

	other := putProfileDefault(unreachable.ID)

	srv.store = &wrappedNotFoundSAStore{Store: s}
	wrapped := putProfileDefault(uuid.New().String())

	assert.NotEqual(t, http.StatusNotFound, wrapped.Code, "body: %s", wrapped.Body.String())
	requireErrorText(t, other, ErrCodeInvalidRequest)
	assert.Equal(t, other.Code, wrapped.Code, "status: wrapped not-found vs other-project account")
	assert.Equal(t, other.Body.String(), wrapped.Body.String(), "body: wrapped not-found vs other-project account")
}
