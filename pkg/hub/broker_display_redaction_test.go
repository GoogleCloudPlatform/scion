//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const credentialRegistry = "user:pa/ss@registry.example/team"

// TestRuntimeBrokerList_ProjectFilterRedactsNodeImageRegistry: the
// projectId-filtered broker list shows a reported node image registry with
// its credentials redacted (ptone/scion#3916); the stored value is kept.
func TestRuntimeBrokerList_ProjectFilterRedactsNodeImageRegistry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("redact-list-project"), Name: "Redact List", Slug: "redact-list", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("redact-list-broker"), Name: "Redact List Broker", Slug: "redact-list-broker", Status: store.BrokerStatusOnline,
		LastHeartbeat: time.Now(), Created: time.Now(), Updated: time.Now(),
		Capabilities: &store.BrokerCapabilities{NodeImageRegistry: credentialRegistry},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/runtime-brokers?projectId=%s", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListRuntimeBrokersWithProviderResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Brokers, 1)
	require.NotNil(t, resp.Brokers[0].Capabilities)
	assert.Equal(t, "***@registry.example/team", resp.Brokers[0].Capabilities.NodeImageRegistry)

	stored, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, credentialRegistry, stored.Capabilities.NodeImageRegistry, "the stored value is unchanged")
}

// TestRegisterProject_EmbeddedBrokerResponseRedactsNodeImageRegistry: the
// project registration response shows the registered broker's node image
// registry redacted; the stored value is kept.
func TestRegisterProject_EmbeddedBrokerResponseRedactsNodeImageRegistry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	const brokerName = "redact-register-broker"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "redact-register-project",
		GitRemote: "github.com/org/redact-register",
		Broker: &RegisterProjectBrokerInfo{
			Name:         brokerName,
			Version:      "1.0.0",
			Capabilities: &store.BrokerCapabilities{NodeImageRegistry: credentialRegistry},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Broker)
	require.NotNil(t, resp.Broker.Capabilities)
	assert.Equal(t, "***@registry.example/team", resp.Broker.Capabilities.NodeImageRegistry)

	stored, err := s.GetRuntimeBrokerByName(ctx, brokerName)
	require.NoError(t, err)
	require.NotNil(t, stored.Capabilities)
	assert.Equal(t, credentialRegistry, stored.Capabilities.NodeImageRegistry, "the stored value is unchanged")
}

// TestBrokerDisplay_FlatCloudRunRedactsGCPHostPairOnlyForNonUpdaters verifies
// that GCPHostServiceAccountEmail and GCPHostProjectID are redacted ONLY on
// flat Cloud Run rows for non-updater readers (including broker OBO requests
// on behalf of non-updaters), kept for pure brokerSelf and updaters, and left
// unchanged on legacy rows and non-Cloud-Run flat rows.
func TestBrokerDisplay_FlatCloudRunRedactsGCPHostPairOnlyForNonUpdaters(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := addExtraUser(t, s, tid("redact-cr-owner"), "redact-cr-owner@test.com", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	reader := addExtraUser(t, s, tid("redact-cr-reader"), "redact-cr-reader@test.com", store.UserRoleMember)
	ensureHubMembership(ctx, s, reader.ID)

	flatCR := &store.RuntimeBroker{
		ID:                         tid("redact-flat-cr"),
		Name:                       "redact-flat-cr",
		Slug:                       "redact-flat-cr",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		RuntimeTarget:              &api.RuntimeTargetDescriptor{ID: tid("t-redact-cr"), Type: "cloudrun", DisplayName: "CR"},
		GCPHostServiceAccountEmail: "cr-sa@cr-proj.iam.gserviceaccount.com",
		GCPHostProjectID:           "cr-proj",
		Created:                    time.Now(),
		Updated:                    time.Now(),
	}
	flatDocker := &store.RuntimeBroker{
		ID:                         tid("redact-flat-docker"),
		Name:                       "redact-flat-docker",
		Slug:                       "redact-flat-docker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		RuntimeTarget:              &api.RuntimeTargetDescriptor{ID: tid("t-redact-docker"), Type: "docker", DisplayName: "Docker"},
		GCPHostServiceAccountEmail: "docker-sa@docker-proj.iam.gserviceaccount.com",
		GCPHostProjectID:           "docker-proj",
		Created:                    time.Now(),
		Updated:                    time.Now(),
	}
	legacyBroker := &store.RuntimeBroker{
		ID:                         tid("redact-legacy"),
		Name:                       "redact-legacy",
		Slug:                       "redact-legacy",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		GCPHostServiceAccountEmail: "legacy-sa@legacy-proj.iam.gserviceaccount.com",
		GCPHostProjectID:           "legacy-proj",
		Created:                    time.Now(),
		Updated:                    time.Now(),
	}
	for _, b := range []*store.RuntimeBroker{flatCR, flatDocker, legacyBroker} {
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	}

	// 1. Non-updater reader sees flatCR redacted, while flatDocker and legacyBroker keep today's display.
	rec := doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/runtime-brokers/"+flatCR.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "cr-sa@cr-proj.iam.gserviceaccount.com")
	assert.NotContains(t, rec.Body.String(), "cr-proj")

	recDocker := doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/runtime-brokers/"+flatDocker.ID, nil)
	require.Equal(t, http.StatusOK, recDocker.Code, recDocker.Body.String())
	assert.Contains(t, recDocker.Body.String(), "docker-sa@docker-proj.iam.gserviceaccount.com")
	assert.Contains(t, recDocker.Body.String(), "docker-proj")

	recLegacy := doRequestAsUser(t, srv, reader, http.MethodGet, "/api/v1/runtime-brokers/"+legacyBroker.ID, nil)
	require.Equal(t, http.StatusOK, recLegacy.Code, recLegacy.Body.String())
	assert.Contains(t, recLegacy.Body.String(), "legacy-sa@legacy-proj.iam.gserviceaccount.com")
	assert.Contains(t, recLegacy.Body.String(), "legacy-proj")

	// 2. Pure brokerSelf read keeps its own GCP host pair.
	brokerIdent := NewBrokerIdentity(flatCR.ID)
	selfReq := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/"+flatCR.ID, nil)
	selfReq = selfReq.WithContext(contextWithIdentity(contextWithBrokerIdentity(selfReq.Context(), brokerIdent), brokerIdent))
	selfRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(selfRec, selfReq)
	require.Equal(t, http.StatusOK, selfRec.Code, selfRec.Body.String())
	assert.Contains(t, selfRec.Body.String(), "cr-sa@cr-proj.iam.gserviceaccount.com")
	assert.Contains(t, selfRec.Body.String(), "cr-proj")

	// 3. Broker OBO request on behalf of a non-updater reader MUST NOT leak the pair!
	readerIdent := userIdentityFor(reader)
	oboReq := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/"+flatCR.ID, nil)
	oboCtx := contextWithBrokerIdentity(oboReq.Context(), brokerIdent)
	oboCtx = context.WithValue(oboCtx, userContextKey{}, readerIdent)
	oboCtx = contextWithIdentity(oboCtx, readerIdent)
	oboCtx = contextWithBrokerOnBehalfOf(oboCtx, BrokerOnBehalfOf{Broker: brokerIdent, BrokerID: flatCR.ID})
	oboReq = oboReq.WithContext(oboCtx)
	oboRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(oboRec, oboReq)
	require.Equal(t, http.StatusOK, oboRec.Code, oboRec.Body.String())
	assert.NotContains(t, oboRec.Body.String(), "cr-sa@cr-proj.iam.gserviceaccount.com")
	assert.NotContains(t, oboRec.Body.String(), "cr-proj")

	// 4. handleProjectRegister (POST /api/v1/projects/register) & registerProjectBrokerForDisplay:
	//    - HTTP register-project against a flat Cloud Run broker is refused before writing a Broker
	//      payload and never echoes gcpHostServiceAccountEmail or gcpHostProjectId to a non-updater;
	//    - registerProjectBrokerForDisplay omits the flat Cloud Run GCP host pair for a non-updater
	//      and for a broker OBO request on behalf of a non-updater, while keeping it for owner and
	//      pure brokerSelf;
	//    - HTTP register-project over broker OBO on a legacy broker with WorkspaceStorage applies
	//      pureSelf=false (redacting WorkspaceStorage for an OBO user lacking hub.config.read).
	regFlatRec := doRequestAsUser(t, srv, reader, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "redact-reg-flat-cr-proj",
		GitRemote: "github.com/org/redact-reg-flat-cr-proj",
		BrokerID:  flatCR.ID,
	})
	assert.Equal(t, http.StatusConflict, regFlatRec.Code, regFlatRec.Body.String())
	assert.NotContains(t, regFlatRec.Body.String(), "cr-sa@cr-proj.iam.gserviceaccount.com")
	assert.NotContains(t, regFlatRec.Body.String(), "cr-proj")

	readerOnlyCtx := contextWithIdentity(ctx, readerIdent)
	shownReader := srv.registerProjectBrokerForDisplay(readerOnlyCtx, flatCR)
	assert.Empty(t, shownReader.GCPHostServiceAccountEmail, "registerProjectBrokerForDisplay must redact flat Cloud Run SA email for non-updater")
	assert.Empty(t, shownReader.GCPHostProjectID, "registerProjectBrokerForDisplay must redact flat Cloud Run project ID for non-updater")

	shownOBO := srv.registerProjectBrokerForDisplay(oboCtx, flatCR)
	assert.Empty(t, shownOBO.GCPHostServiceAccountEmail, "registerProjectBrokerForDisplay must redact flat Cloud Run SA email on OBO non-updater")
	assert.Empty(t, shownOBO.GCPHostProjectID, "registerProjectBrokerForDisplay must redact flat Cloud Run project ID on OBO non-updater")

	ownerIdent := userIdentityFor(owner)
	shownOwner := srv.registerProjectBrokerForDisplay(contextWithIdentity(ctx, ownerIdent), flatCR)
	assert.Equal(t, "cr-sa@cr-proj.iam.gserviceaccount.com", shownOwner.GCPHostServiceAccountEmail)
	assert.Equal(t, "cr-proj", shownOwner.GCPHostProjectID)

	selfOnlyCtx := contextWithIdentity(contextWithBrokerIdentity(ctx, brokerIdent), brokerIdent)
	shownSelf := srv.registerProjectBrokerForDisplay(selfOnlyCtx, flatCR)
	assert.Equal(t, "cr-sa@cr-proj.iam.gserviceaccount.com", shownSelf.GCPHostServiceAccountEmail)
	assert.Equal(t, "cr-proj", shownSelf.GCPHostProjectID)

	legacyBroker.WorkspaceStorage = &api.BrokerWorkspaceStorage{
		Backend: "nfs",
		Homes:   "nfs",
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.9.8.7", Export: "/srv/nfs", Healthy: true},
	}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, legacyBroker))
	regBody, err := json.Marshal(RegisterProjectRequest{
		Name:      "redact-reg-obo-legacy",
		GitRemote: "github.com/org/redact-reg-obo-legacy",
		BrokerID:  legacyBroker.ID,
	})
	require.NoError(t, err)
	legacyBrokerIdent := NewBrokerIdentity(legacyBroker.ID)
	regOBOReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/register", bytes.NewReader(regBody))
	regOBOReq.Header.Set("Content-Type", "application/json")
	regOBOCtx := contextWithBrokerIdentity(regOBOReq.Context(), legacyBrokerIdent)
	regOBOCtx = context.WithValue(regOBOCtx, userContextKey{}, ownerIdent)
	regOBOCtx = contextWithIdentity(regOBOCtx, ownerIdent)
	regOBOCtx = contextWithBrokerOnBehalfOf(regOBOCtx, BrokerOnBehalfOf{Broker: legacyBrokerIdent, BrokerID: legacyBroker.ID})
	regOBOReq = regOBOReq.WithContext(regOBOCtx)
	regOBORec := httptest.NewRecorder()
	srv.mux.ServeHTTP(regOBORec, regOBOReq)
	require.Equal(t, http.StatusOK, regOBORec.Code, regOBORec.Body.String())
	assert.NotContains(t, regOBORec.Body.String(), "10.9.8.7", "handleProjectRegister must pass pureSelf=false on broker OBO reads")
}
