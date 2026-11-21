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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reservedTestTokenGenerator struct {
	email       string
	accessCalls atomic.Int32
	idCalls     atomic.Int32
	verifyCalls atomic.Int32
}

func (g *reservedTestTokenGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	g.accessCalls.Add(1)
	return &GCPAccessToken{AccessToken: "ya29.test", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (g *reservedTestTokenGenerator) GenerateIDToken(_ context.Context, _, _ string) (*GCPIDToken, error) {
	g.idCalls.Add(1)
	return &GCPIDToken{Token: "eyJ.test"}, nil
}

func (g *reservedTestTokenGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	g.verifyCalls.Add(1)
	return nil
}

func (g *reservedTestTokenGenerator) ServiceAccountEmail() string {
	return g.email
}

func TestGCPServiceAccount_RefuseHubOwnIdentityOnRegistration(t *testing.T) {
	const hubEmail = "hub-runner@hub-proj.iam.gserviceaccount.com"

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: hubEmail}
	srv.SetGCPTokenGenerator(gen)

	projectID := createTestProjectForSA(t, srv, s)

	// Create a project owner (non-admin) user who is also a hub member.
	ownerID := tid("user-proj-owner")
	createDCUser(t, s, ownerID, "owner@test.com", projectID, store.ProjectRoleOwner)
	ensureHubMembership(context.Background(), s, ownerID)
	ownerUser := &store.User{
		ID:          ownerID,
		Email:       "owner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
	}

	variants := []struct {
		name  string
		email string
	}{
		{"exact", hubEmail},
		{"mixed_case", "Hub-Runner@Hub-Proj.IAM.GServiceAccount.COM"},
		{"trimmed_mixed_case", "  HUB-RUNNER@hub-proj.iam.gserviceaccount.com  "},
	}

	routes := []struct {
		name     string
		path     string
		useOwner bool
	}{
		{"project_nested_admin", fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), false},
		{"project_nested_owner", fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), true},
		{"project_flat_admin", fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID), false},
		{"project_flat_owner", fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID), true},
		{"hub_scope_admin", "/api/v1/gcp-service-accounts?scope=hub", false},
		{"hub_scope_member", "/api/v1/gcp-service-accounts?scope=hub", true},
	}

	for _, rt := range routes {
		for _, v := range variants {
			t.Run(rt.name+"/"+v.name, func(t *testing.T) {
				body := map[string]string{
					"email":     v.email,
					"projectId": "hub-proj",
				}
				var recCode int
				var recBody []byte
				if rt.useOwner {
					rec := doRequestAsUser(t, srv, ownerUser, http.MethodPost, rt.path, body)
					recCode = rec.Code
					recBody = rec.Body.Bytes()
				} else {
					rec := doRequest(t, srv, http.MethodPost, rt.path, body)
					recCode = rec.Code
					recBody = rec.Body.Bytes()
				}

				require.Equal(t, http.StatusBadRequest, recCode, "body: %s", string(recBody))
				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(recBody, &errResp))
				assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)
				assert.Contains(t, errResp.Error.Message, "Hub's own GCP service account")
			})
		}
	}

	// No rows should have been created and VerifyImpersonation must never have been called.
	rows, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Zero(t, gen.verifyCalls.Load())
}

func TestGCPServiceAccount_RejectsNonEmailRegistration(t *testing.T) {
	srv, s := testServer(t)
	srv.SetGCPTokenGenerator(&reservedTestTokenGenerator{email: "hub-runner@hub-proj.iam.gserviceaccount.com"})
	projectID := createTestProjectForSA(t, srv, s)

	invalidValues := []struct {
		name  string
		value string
	}{
		{"numeric_id", "109876543210987654321"},
		{"resource_path", "projects/-/serviceAccounts/hub-runner@hub-proj.iam.gserviceaccount.com"},
		{"multiple_at", "a@b@c.com"},
		{"missing_domain", "worker@"},
		{"internal_space", "worker @hub-proj.iam.gserviceaccount.com"},
	}

	routes := []struct {
		name string
		path string
	}{
		{"project_nested", fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID)},
		{"project_flat", fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", projectID)},
		{"hub_scope", "/api/v1/gcp-service-accounts?scope=hub"},
	}

	for _, rt := range routes {
		for _, iv := range invalidValues {
			t.Run(rt.name+"/"+iv.name, func(t *testing.T) {
				rec := doRequest(t, srv, http.MethodPost, rt.path, map[string]string{
					"email":     iv.value,
					"projectId": "hub-proj",
				})
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
				assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
			})
		}
	}

	rows, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCloneProjectGCPServiceAccounts_SkipsReservedAndRemapsDefaults(t *testing.T) {
	const hubEmail = "hub-runner@hub-proj.iam.gserviceaccount.com"

	srv, s := testServer(t)
	srv.SetGCPTokenGenerator(&reservedTestTokenGenerator{email: hubEmail})
	ctx := context.Background()

	srcID := tid("proj-clone-src")
	cloneID := tid("proj-clone-dst")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: srcID, Name: "clone-src", Slug: "clone-src", Created: time.Now(), Updated: time.Now(),
	}))

	// Seed three SAs where the reserved row is NEWEST so ListGCPServiceAccounts
	// (ordered by Created DESC) returns it at index 0, shifting all subsequent
	// indices relative to clonedIDs.
	baseTime := time.Now().Add(-10 * time.Minute)
	saC := &store.GCPServiceAccount{
		ID: tid("sa-clone-c"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "ccc@hub-proj.iam.gserviceaccount.com", ProjectID: "hub-proj",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: baseTime,
	}
	saB := &store.GCPServiceAccount{
		ID: tid("sa-clone-b"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "bbb@hub-proj.iam.gserviceaccount.com", ProjectID: "hub-proj",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: baseTime.Add(time.Minute),
	}
	saReserved := &store.GCPServiceAccount{
		ID: tid("sa-clone-reserved"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: hubEmail, ProjectID: "hub-proj",
		Verified: false, VerificationStatus: store.GCPVerificationFailed,
		CreatedBy: "dev-user", CreatedAt: baseTime.Add(2 * time.Minute),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saC))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saB))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saReserved))

	cloneAnnotations := map[string]string{
		projectSettingDefaultGCPIdentitySAID: saC.ID,
	}
	setBrokerDefaultSAIDsAnnotation(cloneAnnotations, map[string]string{
		"br-1": saC.ID,
		"br-2": saReserved.ID,
	})
	setProfileDefaultSAIDsAnnotation(cloneAnnotations, map[string]string{
		"prof-1": saB.ID,
		"prof-2": saReserved.ID,
	})
	cloneProj := &store.Project{
		ID: cloneID, Name: "clone-dst", Slug: "clone-dst",
		Annotations: cloneAnnotations,
		Created:     time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, cloneProj))

	var rollback []func()
	require.NoError(t, srv.cloneProjectGCPServiceAccounts(ctx, srcID, cloneProj, "dev-user", &rollback))

	// Only the two non-reserved accounts should be cloned.
	clonedSAs, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: cloneID,
	})
	require.NoError(t, err)
	require.Len(t, clonedSAs, 2)
	idToEmail := make(map[string]string, len(clonedSAs))
	for _, sa := range clonedSAs {
		idToEmail[sa.ID] = sa.Email
	}

	// Reload clone project and verify all three default surfaces.
	reloaded, err := s.GetProject(ctx, cloneID)
	require.NoError(t, err)

	// Project default pointed at saC (index 2 in source list); must remap to cloned saC.
	assert.Equal(t, saC.Email, idToEmail[reloaded.Annotations[projectSettingDefaultGCPIdentitySAID]])

	// Broker default br-1 -> saC must remap to cloned saC; br-2 -> reserved must be dropped.
	gotBrokers := brokerDefaultSAIDsFromAnnotations(reloaded.Annotations)
	assert.Equal(t, saC.Email, idToEmail[gotBrokers["br-1"]])
	_, hasBr2 := gotBrokers["br-2"]
	assert.False(t, hasBr2, "broker default pointing at skipped reserved SA should be dropped")

	// Profile default prof-1 -> saB must remap to cloned saB; prof-2 -> reserved must be dropped.
	gotProfiles := profileDefaultSAIDsFromAnnotations(reloaded.Annotations)
	assert.Equal(t, saB.Email, idToEmail[gotProfiles["prof-1"]])
	_, hasProf2 := gotProfiles["prof-2"]
	assert.False(t, hasProf2, "profile default pointing at skipped reserved SA should be dropped")

	// Also verify that a project default pointing directly at the skipped reserved SA is cleared.
	cloneID2 := tid("proj-clone-dst2")
	cloneProj2 := &store.Project{
		ID: cloneID2, Name: "clone-dst2", Slug: "clone-dst2",
		Annotations: map[string]string{projectSettingDefaultGCPIdentitySAID: saReserved.ID},
		Created:     time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, cloneProj2))
	var rollback2 []func()
	require.NoError(t, srv.cloneProjectGCPServiceAccounts(ctx, srcID, cloneProj2, "dev-user", &rollback2))
	reloaded2, err := s.GetProject(ctx, cloneID2)
	require.NoError(t, err)
	assert.Empty(t, reloaded2.Annotations[projectSettingDefaultGCPIdentitySAID])
}

func TestGCPServiceAccount_UnknownHubIdentityAllowsRegistration(t *testing.T) {
	// Documented behaviour when Hub identity is unknown (not running on GCP):
	// both nil token generator and a generator with an empty ServiceAccountEmail
	// must allow normal registration without erroring.
	cases := []struct {
		name string
		gen  GCPTokenGenerator
	}{
		{"nil_generator", nil},
		{"empty_hub_email", &reservedTestTokenGenerator{email: ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetGCPTokenGenerator(tc.gen)
			projectID := createTestProjectForSA(t, srv, s)

			rec := doRequest(t, srv, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
				map[string]string{
					"email":     "worker@my-proj.iam.gserviceaccount.com",
					"projectId": "my-proj",
				})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

func TestGCPServiceAccount_RefuseVerifyAndAssignForHubOwnIdentity(t *testing.T) {
	const hubEmail = "hub-runner@hub-proj.iam.gserviceaccount.com"

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: hubEmail}
	srv.SetGCPTokenGenerator(gen)

	projectID := createTestProjectForSA(t, srv, s)
	ctx := context.Background()

	// Seed pre-existing project-scoped and hub-scoped rows matching the Hub SA
	// (including mixed case) directly in the store.
	projSA := &store.GCPServiceAccount{
		ID:                 tid("sa-hub-proj"),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              "Hub-Runner@Hub-Proj.iam.gserviceaccount.com",
		ProjectID:          "hub-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "dev-user",
		CreatedAt:          time.Now(),
	}
	hubSA := &store.GCPServiceAccount{
		ID:                 tid("sa-hub-global"),
		Scope:              store.ScopeHub,
		ScopeID:            srv.HubID(),
		Email:              hubEmail,
		ProjectID:          "hub-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "dev-user",
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projSA))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))

	// Nested verify must refuse with identity_reserved and not call VerifyImpersonation.
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, projSA.ID), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)

	// Flat verify must refuse with identity_reserved and not call VerifyImpersonation.
	rec = doRequest(t, srv, http.MethodPost, flatSAPath+hubSA.ID+"/verify", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	errResp = ErrorResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)
	assert.Zero(t, gen.verifyCalls.Load())

	// Assignment gate must refuse even when the row is still marked verified.
	denial := srv.evaluateSAAssignment(ctx, nil, projSA, projectID, SurfaceAgentCreate)
	require.NotNil(t, denial)
	assert.Equal(t, saAssignDenyValidation, denial.kind)
	assert.Equal(t, ErrCodeIdentityReserved, denial.code)
}

func TestGCPTokenMintPaths_RefuseHubOwnIdentityEvenIfRowExists(t *testing.T) {
	const hubEmail = "hub-sa@test-hub-project.iam.gserviceaccount.com"

	cases := []struct {
		name         string
		hubIdentity  string
		saEmail      string
		expectStatus int
		expectCalls  int32
	}{
		{"exact", hubEmail, hubEmail, http.StatusForbidden, 0},
		{"mixed_case", hubEmail, "Hub-SA@Test-Hub-Project.IAM.GServiceAccount.COM", http.StatusForbidden, 0},
		{"normal_sa_allowed", hubEmail, "worker@test-hub-project.iam.gserviceaccount.com", http.StatusOK, 1},
		{"unknown_hub_identity_allowed", "", "worker@test-hub-project.iam.gserviceaccount.com", http.StatusOK, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			gen := &reservedTestTokenGenerator{email: tc.hubIdentity}
			srv.SetGCPTokenGenerator(gen)

			ctx := context.Background()
			projectID := tid("proj-mint-refuse-" + tc.name)
			require.NoError(t, s.CreateProject(ctx, &store.Project{
				ID: projectID, Name: "mint-refuse-" + tc.name, Slug: "mint-refuse-" + tc.name,
				Created: time.Now(), Updated: time.Now(),
			}))

			userID := tid("user-mint-refuse-" + tc.name)
			createDCUser(t, s, userID, "owner@test.com", projectID, store.ProjectRoleOwner)

			// Seed a pre-existing VERIFIED row matching the Hub's own SA.
			saID := tid("sa-mint-refuse-" + tc.name)
			sa := &store.GCPServiceAccount{
				ID:                 saID,
				Scope:              store.ScopeProject,
				ScopeID:            projectID,
				Email:              tc.saEmail,
				ProjectID:          "test-hub-project",
				Verified:           true,
				VerifiedAt:         time.Now(),
				VerificationStatus: store.GCPVerificationVerified,
				CreatedBy:          userID,
				CreatedAt:          time.Now(),
			}
			require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

			agentID := tid("agent-mint-refuse-" + tc.name)
			agent := &store.Agent{
				ID:        agentID,
				Slug:      "agent-mint-refuse-" + tc.name,
				Name:      "agent-mint-refuse-" + tc.name,
				ProjectID: projectID,
				Phase:     string(state.PhaseRunning),
				OwnerID:   userID,
				CreatedBy: userID,
				Ancestry:  []string{userID},
				AppliedConfig: &store.AgentAppliedConfig{
					GCPIdentity: &store.GCPIdentityConfig{
						MetadataMode:        store.GCPMetadataModeAssign,
						ServiceAccountID:    saID,
						ServiceAccountEmail: tc.saEmail,
						ProjectID:           "test-hub-project",
					},
				},
				Created: time.Now(),
				Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))
			agentRow, err := s.GetAgent(ctx, agentID)
			require.NoError(t, err)
			seedAssignment(t, s, agentRow, userID, saID)

			token := srv.generateRecordedAgentToken(t, agentID, projectID,
				[]AgentTokenScope{GCPTokenScopeForSA(saID)}, []string{userID})

			// Both /api/v1/agent/gcp-token and /api/v1/agent/gcp-identity-token must enforce the same decision.
			recAccess := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-token",
				map[string]any{"scopes": []string{"https://www.googleapis.com/auth/cloud-platform"}}, token)
			assert.Equal(t, tc.expectStatus, recAccess.Code, "body: %s", recAccess.Body.String())

			recID := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-identity-token",
				map[string]string{"audience": "https://example.com"}, token)
			assert.Equal(t, tc.expectStatus, recID.Code, "body: %s", recID.Body.String())

			assert.Equal(t, tc.expectCalls, gen.accessCalls.Load(), "unexpected GenerateAccessToken call count")
			assert.Equal(t, tc.expectCalls, gen.idCalls.Load(), "unexpected GenerateIDToken call count")
		})
	}
}

func TestScanReservedGCPServiceAccounts(t *testing.T) {
	const hubEmail = "hub-runner@hub-proj.iam.gserviceaccount.com"

	t.Run("flags_matching_rows_and_surfaces_status_and_warns", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetGCPTokenGenerator(&reservedTestTokenGenerator{email: hubEmail})
		projectID := createTestProjectForSA(t, srv, s)
		ctx := context.Background()

		// 1. Matching project-scoped row (mixed case + whitespace).
		matchProj := &store.GCPServiceAccount{
			ID:                 tid("sa-scan-match-proj"),
			Scope:              store.ScopeProject,
			ScopeID:            projectID,
			Email:              " Hub-Runner@Hub-Proj.IAM.GServiceAccount.COM ",
			ProjectID:          "hub-proj",
			Verified:           true,
			VerifiedAt:         time.Now(),
			VerificationStatus: store.GCPVerificationVerified,
			CreatedBy:          "dev-user",
			CreatedAt:          time.Now(),
		}
		// 2. Matching hub-scoped row (exact).
		matchHub := &store.GCPServiceAccount{
			ID:                 tid("sa-scan-match-hub"),
			Scope:              store.ScopeHub,
			ScopeID:            srv.HubID(),
			Email:              hubEmail,
			ProjectID:          "hub-proj",
			Verified:           true,
			VerifiedAt:         time.Now(),
			VerificationStatus: store.GCPVerificationVerified,
			CreatedBy:          "dev-user",
			CreatedAt:          time.Now(),
		}
		// 3. Non-matching verified row — must remain untouched.
		normalSA := &store.GCPServiceAccount{
			ID:                 tid("sa-scan-normal"),
			Scope:              store.ScopeProject,
			ScopeID:            projectID,
			Email:              "worker@hub-proj.iam.gserviceaccount.com",
			ProjectID:          "hub-proj",
			Verified:           true,
			VerifiedAt:         time.Now(),
			VerificationStatus: store.GCPVerificationVerified,
			CreatedBy:          "dev-user",
			CreatedAt:          time.Now(),
		}
		require.NoError(t, s.CreateGCPServiceAccount(ctx, matchProj))
		require.NoError(t, s.CreateGCPServiceAccount(ctx, matchHub))
		require.NoError(t, s.CreateGCPServiceAccount(ctx, normalSA))

		// Capture warn logs during the scan.
		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		flagged, err := srv.ScanReservedGCPServiceAccounts(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, flagged)

		// Verify warn logs were emitted for both matching rows.
		logs := logBuf.String()
		assert.Contains(t, logs, "level=WARN")
		assert.Contains(t, logs, matchProj.ID)
		assert.Contains(t, logs, matchHub.ID)
		assert.NotContains(t, logs, normalSA.ID)

		// Verify persisted state of both flagged rows.
		gotProj, err := s.GetGCPServiceAccount(ctx, matchProj.ID)
		require.NoError(t, err)
		assert.False(t, gotProj.Verified)
		assert.Equal(t, store.GCPVerificationFailed, gotProj.VerificationStatus)
		assert.Equal(t, gcpSAHubIdentityFlagError, gotProj.VerificationError)

		gotHub, err := s.GetGCPServiceAccount(ctx, matchHub.ID)
		require.NoError(t, err)
		assert.False(t, gotHub.Verified)
		assert.Equal(t, store.GCPVerificationFailed, gotHub.VerificationStatus)
		assert.Equal(t, gcpSAHubIdentityFlagError, gotHub.VerificationError)

		// Verify normal row was untouched.
		gotNormal, err := s.GetGCPServiceAccount(ctx, normalSA.ID)
		require.NoError(t, err)
		assert.True(t, gotNormal.Verified)
		assert.Equal(t, store.GCPVerificationVerified, gotNormal.VerificationStatus)
		assert.Empty(t, gotNormal.VerificationError)

		// Verify existing admin-visible status endpoint reflects the flagged state
		// and does not advise granting tokenCreator.
		rec := doRequest(t, srv, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/status", projectID, matchProj.ID), nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var st GCPServiceAccountStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		assert.False(t, st.Verification.Verified)
		assert.Equal(t, store.GCPVerificationFailed, st.Verification.Status)
		assert.Equal(t, gcpSAHubIdentityFlagError, st.Verification.Error)
		assert.Contains(t, st.NextStep.Message, "Remove this service account registration")
		assert.NotContains(t, st.NextStep.Message, "token-creator")

		// Re-running the scan is idempotent.
		flagged2, err := srv.ScanReservedGCPServiceAccounts(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, flagged2)
	})

	t.Run("unknown_hub_identity_is_noop", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			gen  GCPTokenGenerator
		}{
			{"nil_generator", nil},
			{"empty_hub_email", &reservedTestTokenGenerator{email: ""}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, s := testServer(t)
				srv.SetGCPTokenGenerator(tc.gen)
				projectID := createTestProjectForSA(t, srv, s)
				ctx := context.Background()

				sa := &store.GCPServiceAccount{
					ID:                 tid("sa-unknown-hub-" + tc.name),
					Scope:              store.ScopeProject,
					ScopeID:            projectID,
					Email:              "worker@hub-proj.iam.gserviceaccount.com",
					ProjectID:          "hub-proj",
					Verified:           true,
					VerifiedAt:         time.Now(),
					VerificationStatus: store.GCPVerificationVerified,
					CreatedBy:          "dev-user",
					CreatedAt:          time.Now(),
				}
				require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

				flagged, err := srv.ScanReservedGCPServiceAccounts(ctx)
				require.NoError(t, err)
				assert.Zero(t, flagged)

				got, err := s.GetGCPServiceAccount(ctx, sa.ID)
				require.NoError(t, err)
				assert.True(t, got.Verified)
				assert.Equal(t, store.GCPVerificationVerified, got.VerificationStatus)
			})
		}
	})
}
