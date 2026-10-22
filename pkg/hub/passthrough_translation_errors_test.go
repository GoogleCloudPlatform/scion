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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestWritePassthroughTranslationError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"refusal", &passthroughTranslationDenied{status: http.StatusForbidden, code: ErrCodeForbidden, msg: "no"}, http.StatusForbidden, ErrCodeForbidden},
		{"unverified record", &passthroughTranslationDenied{status: http.StatusBadRequest, code: ErrCodeIdentityNotVerified, msg: "unverified"}, http.StatusBadRequest, ErrCodeIdentityNotVerified},
		{"record conflict", fmt.Errorf("ensure host SA record: %w", errHostSARecordConflict), http.StatusConflict, ErrCodeConflict},
		{"probe failure", fmt.Errorf("ensure host SA record: %w", errHostSARecordUnverifiable), http.StatusBadGateway, "gcp_verification_failed"},
		{"other", errors.New("store down"), http.StatusInternalServerError, ErrCodeRuntimeError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePassthroughTranslationError(rec, tc.err)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), `"`+tc.wantCode+`"`)
		})
	}
}

// pteFixture is a cloudrun-sandbox broker server in enforce mode whose
// project owner owns the broker and holds actAs on its host account.
type pteFixture struct {
	srv     *Server
	store   store.Store
	owner   *store.User
	project *store.Project
	broker  *store.RuntimeBroker
}

func setupPTE(t *testing.T, name string, verifyErr error) *pteFixture {
	t.Helper()
	owner := ptUser(tid(name+"-owner"), name+"-owner@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hsrEmail, "hsr-project")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hsrEmail))
	srv.SetGCPTokenGenerator(&probeCountingGenerator{
		mockGCPTokenGenerator: mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"},
		verifyErr:             verifyErr,
	})
	return &pteFixture{srv: srv, store: s, owner: owner, project: project, broker: broker}
}

func (f *pteFixture) createExplicit(t *testing.T, name string) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: name, ProjectID: f.project.ID, Task: "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	})
	return rec.Code, rec.Body.String()
}

func (f *pteFixture) patchToPassthrough(t *testing.T, name string) (int, string, string) {
	t.Helper()
	agent := &store.Agent{
		ID: tid(name), Slug: name, Name: name,
		ProjectID: f.project.ID, RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseCreated),
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Created: time.Now(), Updated: time.Now(),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]string{"metadata_mode": "passthrough"}})
	return rec.Code, rec.Body.String(), agent.ID
}

func (f *pteFixture) dispatchDefault(t *testing.T, name string) error {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	return f.srv.dispatchAgentEventHandler()(ctx, withSessionRevision(store.ScheduledEvent{
		ID:        "evt-" + name,
		ProjectID: f.project.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"` + name + `","task":"scheduled work"}`,
		CreatedBy: f.owner.ID,
	}, f.owner.ID))
}

func TestPassthroughTranslation_ProbeFailure_Create502(t *testing.T) {
	f := setupPTE(t, "pte-create-502", errors.New("permission denied"))

	code, body := f.createExplicit(t, "pte-create-502-agent")

	assert.Equal(t, http.StatusBadGateway, code, "body: %s", body)
	assert.Contains(t, body, "gcp_verification_failed")
	assert.Empty(t, hsrHubRecords(t, f.store))
}

func TestPassthroughTranslation_Patch_RecordConflict409(t *testing.T) {
	f := setupPTE(t, "pte-patch-409", nil)
	hsrSeed(t, f.store, "pte-patch-409-proj", store.ScopeProject, tid("pte-other-project"), tid("someone"), false)

	code, body, id := f.patchToPassthrough(t, "pte-patch-409-agent")

	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Equal(t, store.GCPMetadataModeBlock, mustGetAgent(t, f.store, id).AppliedConfig.GCPIdentity.MetadataMode)
	assert.Empty(t, hsrHubRecords(t, f.store))
}

func TestPassthroughTranslation_Patch_ProbeFailure502(t *testing.T) {
	f := setupPTE(t, "pte-patch-502", errors.New("permission denied"))

	code, body, id := f.patchToPassthrough(t, "pte-patch-502-agent")

	assert.Equal(t, http.StatusBadGateway, code, "body: %s", body)
	assert.Contains(t, body, "gcp_verification_failed")
	assert.Equal(t, store.GCPMetadataModeBlock, mustGetAgent(t, f.store, id).AppliedConfig.GCPIdentity.MetadataMode)
}

func TestPassthroughTranslation_Scheduled_RecordConflictFailsDispatch(t *testing.T) {
	f := setupPTE(t, "pte-sched-409", nil)
	hsrSeed(t, f.store, "pte-sched-409-proj", store.ScopeProject, tid("pte-other-project"), tid("someone"), false)

	err := f.dispatchDefault(t, "pte-sched-409-agent")

	require.Error(t, err)
	assert.ErrorIs(t, err, errHostSARecordConflict)
	_, getErr := f.store.GetAgentBySlug(context.Background(), f.project.ID, "pte-sched-409-agent")
	assert.ErrorIs(t, getErr, store.ErrNotFound)
}

func TestPassthroughTranslation_Scheduled_ProbeFailureFailsDispatch(t *testing.T) {
	f := setupPTE(t, "pte-sched-502", errors.New("permission denied"))

	err := f.dispatchDefault(t, "pte-sched-502-agent")

	require.Error(t, err)
	assert.ErrorIs(t, err, errHostSARecordUnverifiable)
	assert.Empty(t, hsrHubRecords(t, f.store))
}

// TestPassthroughTranslation_UnverifiedOwnRecord_Create400: a reused
// translation record that is not verified fails the shared service-account
// rules with identity_not_verified.
func TestPassthroughTranslation_UnverifiedOwnRecord_Create400(t *testing.T) {
	f := setupPTE(t, "pte-unverified", nil)
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), &store.GCPServiceAccount{
		ID: tid("pte-unverified-sa"), Scope: store.ScopeHub, ScopeID: f.broker.ID, Email: hsrEmail, ProjectID: "hsr-project",
		Verified: false, VerificationStatus: store.GCPVerificationFailed,
		CreatedBy: hostSATranslationCreator, CreatedAt: time.Now(),
	}))

	code, body := f.createExplicit(t, "pte-unverified-agent")

	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, ErrCodeIdentityNotVerified)
	_, err := f.store.GetAgentBySlug(context.Background(), f.project.ID, "pte-unverified-agent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
