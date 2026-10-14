//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetTemplate_HappyPath_MarkAsTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Template Test",
		Slug:      "template-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/set-template",
		SetTemplateRequest{IsTemplate: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", updated.Labels[store.LabelTemplate])
}

func TestSetTemplate_HappyPath_UnmarkTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Unmark Test",
		Slug:      "unmark-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Labels: map[string]string{
			store.LabelTemplate: "true",
		},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/set-template",
		SetTemplateRequest{IsTemplate: false})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.Labels[store.LabelTemplate])
}

func TestSetTemplate_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Method Test",
		Slug:      "method-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/set-template", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestSetTemplate_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Auth Test",
		Slug:      "auth-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequestNoAuth(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/set-template",
		SetTemplateRequest{IsTemplate: true})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSetTemplate_ConflictWithAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Conflict Test",
		Slug:      "conflict-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "test-agent",
		ProjectID: project.ID,
		Name:      "test-agent",
		Phase:     "running",
		CreatedBy: DevUserID,
	}))

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/set-template",
		SetTemplateRequest{IsTemplate: true})
	assert.Equal(t, http.StatusConflict, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "agents exist")
}

func TestSetTemplate_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+api.NewUUID()+"/set-template",
		SetTemplateRequest{IsTemplate: true})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSetTemplate_InvalidBody(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        api.NewUUID(),
		Name:      "Invalid Body Test",
		Slug:      "invalid-body-test",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequestRaw(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/set-template",
		[]byte("{not valid json}"), "application/json")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
