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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stable identity error codes (ptone/scion#4019, spec §7). One test group per
// code, plus the unchanged not-available answer.

func assignRequest(name, saID string) CreateAgentRequest {
	return CreateAgentRequest{
		Name: name,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: saID,
		},
	}
}

// --- identity_not_verified ---

func TestIdentityNotVerified_AssignAtCreate(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)

	rec := createAgentAsOwner(t, f, assignRequest("not-verified-agent", sa.ID))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityNotVerified, apiErr.Code)
	assert.Contains(t, apiErr.Message, sa.Email, "a project member may see the account email (Q1)")
	assert.Contains(t, apiErr.Message, "the hub cannot obtain tokens for it")
	assert.Contains(t, apiErr.Message, "A project admin must grant the hub's service account roles/iam.serviceAccountTokenCreator")
	assert.Contains(t, apiErr.Message, "scion service-accounts verify <id>")
}

func TestIdentityNotVerifiedMessage_NamesAdminByScope(t *testing.T) {
	hub := identityNotVerifiedMessage(&store.GCPServiceAccount{Scope: store.ScopeHub, Email: "hub@example.iam.gserviceaccount.com"})
	assert.Contains(t, hub, "A hub admin must grant")
	assert.Contains(t, hub, "scion service-accounts verify --global <id>")

	project := identityNotVerifiedMessage(&store.GCPServiceAccount{Scope: store.ScopeProject, Email: "p@example.iam.gserviceaccount.com"})
	assert.Contains(t, project, "A project admin must grant")
}

// The not-available answer is unchanged: one text for unknown and not
// visible, no account string, no identity code.
func TestIdentityNotAvailable_Unchanged(t *testing.T) {
	f := bypassAgentsSetup(t)
	rec := createAgentAsOwner(t, f, assignRequest("not-available-agent", uuid.New().String()))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeValidationError, apiErr.Code)
	assert.Equal(t, msgSANotAvailableInProject, apiErr.Message)
}

// --- identity_default_invalid ---

func TestIdentityDefaultInvalid_ProjectDefaultUnverified(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)
	setStaleProjectDefaultSA(t, f, sa.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "default-unverified"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityDefaultInvalid, apiErr.Code)
	assert.Contains(t, apiErr.Message, "project default GCP service account")
	assert.Contains(t, apiErr.Message, sa.Email)
	assert.Contains(t, apiErr.Message, "roles/iam.serviceAccountTokenCreator")
	assert.Contains(t, apiErr.Message, "a project admin can point defaultGCPIdentityServiceAccountID")
}

func TestIdentityDefaultInvalid_ProjectDefaultNotAvailable(t *testing.T) {
	f := bypassAgentsSetup(t)
	setStaleProjectDefaultSA(t, f, uuid.New().String())

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "default-missing"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityDefaultInvalid, apiErr.Code)
	assert.Contains(t, apiErr.Message, "project default GCP service account is not available in this project")
	assert.Contains(t, apiErr.Message, "a project admin must change or clear defaultGCPIdentityServiceAccountID")
}

// Each tier names its own setting and who changes it.
func TestIdentityDefaultInvalid_NamesTheDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, store.UserRoleMember, "test"))

	cases := []struct {
		tier defaultTier
		want []string
	}{
		{defaultTierProject, []string{"project default GCP service account", "a project admin", "defaultGCPIdentityServiceAccountID in the project settings"}},
		{profileDefaultTier("remote"), []string{`project default for profile "remote"`, "a project admin", `the "remote" entry of defaultGCPIdentityServiceAccountIDByProfile`}},
		{defaultTierHub, []string{"hub default GCP service account", "a hub admin", "agent_defaults.default_gcp_identity_service_account_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.tier.subject(), func(t *testing.T) {
			_, err := f.srv.resolveDefaultSAAssignmentCore(ctx, nil, f.proj.ID, uuid.New().String(), SurfaceProjectDefault, tc.tier)
			var unusable *defaultSAUnusableError
			require.True(t, errors.As(err, &unusable), "want a default-invalid refusal, got %v", err)
			for _, w := range tc.want {
				assert.Contains(t, unusable.Error(), w)
			}
		})
	}
}

// --- identity_assign_denied ---

func TestIdentityAssignDenied_WriteCarriesCode(t *testing.T) {
	rec := httptest.NewRecorder()
	(&saAssignDenial{kind: saAssignDenyForbiddenStructured, msg: "m", resourceType: "gcp_service_account"}).write(rec)
	require.Equal(t, http.StatusForbidden, rec.Code)
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityAssignDenied, apiErr.Code)
	assert.Equal(t, "gcp_service_account", apiErr.Details["resource_type"], "structured details are unchanged")
	assert.Equal(t, string(ActionAssign), apiErr.Details["denied_action"])

	rec = httptest.NewRecorder()
	(&saAssignDenial{kind: saAssignDenyForbidden, msg: "m"}).write(rec)
	require.Equal(t, http.StatusForbidden, rec.Code)
	apiErr = decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityAssignDenied, apiErr.Code)
	// Structured details too, so a client labelling by denied_action does not
	// fall back to showing the code.
	assert.Equal(t, "gcp_service_account", apiErr.Details["resource_type"])
	assert.Equal(t, string(ActionAssign), apiErr.Details["denied_action"])

	rec = httptest.NewRecorder()
	(&saAssignDenial{kind: saAssignDenyUnauthorized}).write(rec)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no caller identity stays a 401")
}

func TestIdentityAssignDenied_PolicyMessageNamesPermissionAndGrantor(t *testing.T) {
	hub := saAssignPolicyDeniedMessage(&store.GCPServiceAccount{Scope: store.ScopeHub, Email: "hub@example.iam.gserviceaccount.com"})
	assert.Contains(t, hub, "gcp_service_account.assign")
	assert.Contains(t, hub, "A hub admin grants it")
	assert.NotContains(t, hub, "hub@example.iam.gserviceaccount.com")

	project := saAssignPolicyDeniedMessage(&store.GCPServiceAccount{Scope: store.ScopeProject, Email: "p@example.iam.gserviceaccount.com"})
	assert.Contains(t, project, "gcp_service_account.assign")
	assert.Contains(t, project, "A project owner or admin grants it")
	assert.NotContains(t, project, "p@example.iam.gserviceaccount.com")
}

func TestIdentityAssignDenied_ActAsOverHTTP(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "identity-actas-deny@example.iam.gserviceaccount.com")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().
		DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs"))

	req := assignRequest("identity-actas-deny", sa.ID)
	req.ProjectID = project.ID
	req.Task = "do something"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityAssignDenied, apiErr.Code)
	assert.Contains(t, apiErr.Message, store.PermissionActAs+" is required on "+sa.Email)
	assert.Contains(t, apiErr.Message, "roles/iam.serviceAccountUser")
}

func TestIdentityAssignDenied_HubScopedWithCheckModeOff(t *testing.T) {
	f := bypassAgentsSetup(t)
	setMode(f.srv, SAAssignCheckOff)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSACreatedBy(t, f, f.owner.ID, true)

	rec := createAgentAsOwner(t, f, assignRequest("hub-mode-off", sa.ID))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityAssignDenied, apiErr.Code)
	assert.Contains(t, apiErr.Message, "requires gcpIamCheckMode=enforce")
	assert.Contains(t, apiErr.Message, saAssignHubModeRemedy)
}

// --- start refusal ---

func TestGCPIdentityStartRefusalText(t *testing.T) {
	status, code, msg := gcpIdentityStartRefusalText("start", errGCPSANotVerified)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, ErrCodeIdentityNotVerified, code)
	assert.Contains(t, msg, "Cannot start agent")
	assert.Contains(t, msg, "not verified")
	assert.Contains(t, msg, "roles/iam.serviceAccountTokenCreator")

	// identity_assign_denied is a 403 on every path, start included.
	status, code, msg = gcpIdentityStartRefusalText("restart", errGCPSAHubModeOff)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, ErrCodeIdentityAssignDenied, code)
	assert.Contains(t, msg, "gcpIamCheckMode=enforce")
	assert.Contains(t, msg, saAssignHubModeRemedy)

	// A vanished account reads as before: 400, no identity code, no remedy
	// that would reveal more than the not-available text.
	status, code, msg = gcpIdentityStartRefusalText("start", errGCPSANotAvailable)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, ErrCodeValidationError, code)
	assert.Contains(t, msg, errGCPSANotAvailable.Error())
}

// --- identity_mode_unsupported ---

func TestIdentityModeUnsupported_BlockDefaultOnKubernetesProject(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	createTestBroker(t, s, "identity-mode-k8s-broker-"+t.Name(), "k8s-broker", "",
		[]store.BrokerProfile{{Name: "default", Type: "kubernetes", Available: true}}, nil)
	require.NoError(t, s.AddProjectProvider(t.Context(), &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   tid("identity-mode-k8s-broker-" + t.Name()),
		BrokerName: "k8s-broker",
		Status:     store.BrokerStatusOnline,
	}))

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityMode: "block"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeIdentityModeUnsupported, apiErr.Code)
	assert.Equal(t, "default GCP identity mode 'block' is not available for a Kubernetes-bound project; "+
		"choose 'passthrough' or 'assign' instead", apiErr.Message, "the text is unchanged")
}
