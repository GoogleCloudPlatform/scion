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
	"errors"
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

func TestLifecycleHook_RefusesHubOwnIdentityLiveAtFireAndWrite(t *testing.T) {
	const hubEmail = "hub-runner@hub-proj.iam.gserviceaccount.com"

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: hubEmail}
	srv.SetGCPTokenGenerator(gen)
	ctx := context.Background()

	// Seed an active admin writer who holds hub.lifecycle_hooks.update.
	adminID := tid("user-hook-admin")
	adminEmail := "hook-admin@test.com"
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          adminID,
		Email:       adminEmail,
		DisplayName: "Hook Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
		Created:     time.Now().Add(-time.Hour),
	}))
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	adminUser := &store.User{
		ID:          adminID,
		Email:       adminEmail,
		DisplayName: "Hook Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}

	// Seed a VERIFIED hub-scoped row with a mixed-case Hub email (simulating
	// a pre-existing row when the startup scan did not run or failed).
	hubSA := &store.GCPServiceAccount{
		ID:                 tid("sa-hook-hub"),
		Scope:              store.ScopeHub,
		ScopeID:            srv.HubID(),
		Email:              "Hub-Runner@Hub-Proj.IAM.GServiceAccount.COM",
		ProjectID:          "hub-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          adminID,
		CreatedAt:          time.Now(),
	}
	// Positive control: a normal verified hub-scoped SA.
	normalSA := &store.GCPServiceAccount{
		ID:                 tid("sa-hook-normal"),
		Scope:              store.ScopeHub,
		ScopeID:            srv.HubID(),
		Email:              "worker@hub-proj.iam.gserviceaccount.com",
		ProjectID:          "hub-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          adminID,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, normalSA))

	// 1. Live fire-time refusal through HTTPExecutor.resolveIdentityAndToken.
	exec := NewHTTPExecutor(s, gen, nil, slog.Default(), srv)
	action := &store.LifecycleHookAction{
		Type:   store.LifecycleHookActionHTTP,
		Method: http.MethodPost,
		URL:    "https://example.com/hook",
	}

	reservedHook := &store.LifecycleHook{
		ID:                tid("hook-fire-reserved"),
		Name:              "fire-reserved",
		ScopeType:         store.LifecycleHookScopeHub,
		ExecutionIdentity: hubSA.ID,
		CreatedBy:         adminEmail,
		Created:           time.Now(),
	}
	_, _, err = exec.resolveIdentityAndToken(ctx, reservedHook, action)
	require.Error(t, err)
	var denied *HookFireDeniedError
	require.True(t, errors.As(err, &denied), "expected HookFireDeniedError, got %T: %v", err, err)
	assert.Equal(t, HookFireIdentityNotAdmissible, denied.Cause)
	assert.Zero(t, gen.accessCalls.Load(), "GenerateAccessToken must not be called for Hub's own SA")

	// Positive control: normal SA succeeds and mints a token.
	normalHook := &store.LifecycleHook{
		ID:                tid("hook-fire-normal"),
		Name:              "fire-normal",
		ScopeType:         store.LifecycleHookScopeHub,
		ExecutionIdentity: normalSA.ID,
		CreatedBy:         adminEmail,
		Created:           time.Now(),
	}
	gotEmail, gotTok, err := exec.resolveIdentityAndToken(ctx, normalHook, action)
	require.NoError(t, err)
	assert.Equal(t, normalSA.Email, gotEmail)
	assert.NotEmpty(t, gotTok)
	assert.Equal(t, int32(1), gen.accessCalls.Load())

	// 2. Write-time refusal on POST /api/v1/admin/lifecycle-hooks.
	createBody := map[string]any{
		"name":              "create-reserved-hook",
		"scopeType":         store.LifecycleHookScopeHub,
		"trigger":           "running",
		"action":            action,
		"executionIdentity": hubSA.ID,
		"enabled":           true,
	}
	recCreate := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/admin/lifecycle-hooks", createBody)
	require.Equal(t, http.StatusBadRequest, recCreate.Code, "body: %s", recCreate.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)

	// 3. Write-time refusal on PUT /api/v1/admin/lifecycle-hooks/{id}.
	existingHook := &store.LifecycleHook{
		ID:                tid("hook-update-target"),
		Name:              "existing-hook",
		ScopeType:         store.LifecycleHookScopeHub,
		Trigger:           "running",
		Action:            action,
		ExecutionIdentity: normalSA.ID,
		Enabled:           true,
		CreatedBy:         adminEmail,
		Created:           time.Now(),
		Updated:           time.Now(),
		StateVersion:      1,
	}
	require.NoError(t, s.CreateLifecycleHook(ctx, existingHook))

	updateBody := map[string]any{
		"name":              existingHook.Name,
		"trigger":           existingHook.Trigger,
		"action":            action,
		"executionIdentity": hubSA.ID,
		"enabled":           true,
		"stateVersion":      1,
	}
	recUpdate := doRequestAsUser(t, srv, adminUser, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+existingHook.ID, updateBody)
	require.Equal(t, http.StatusBadRequest, recUpdate.Code, "body: %s", recUpdate.Body.String())
	errResp = ErrorResponse{}
	require.NoError(t, json.Unmarshal(recUpdate.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)
}

func seedBrokerWithHostSAs(t *testing.T, s store.Store, id, configuredEmail, observedEmail string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:                         id,
		Name:                       id,
		Slug:                       id,
		GCPHostServiceAccountEmail: configuredEmail,
		Status:                     store.BrokerStatusOnline,
		Created:                    time.Now(),
		Updated:                    time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	if observedEmail != "" {
		require.NoError(t, s.RecordRuntimeBrokerHostObservation(ctx, b.ID, store.HostIdentityObservation{
			Outcome:    store.HostObservationPresent,
			Email:      observedEmail,
			ObservedAt: time.Now().UTC(),
		}))
	}
	return b
}

func TestGCPServiceAccount_RefuseBrokerHostIdentityOnRegistration(t *testing.T) {
	const (
		configuredEmail = "broker-configured@host-proj.iam.gserviceaccount.com"
		observedEmail   = "broker-observed@host-proj.iam.gserviceaccount.com"
	)

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: "hub-runner@hub-proj.iam.gserviceaccount.com"}
	srv.SetGCPTokenGenerator(gen)

	seedBrokerWithHostSAs(t, s, tid("broker-configured"), configuredEmail, "")
	seedBrokerWithHostSAs(t, s, tid("broker-observed"), "", observedEmail)

	projectID := createTestProjectForSA(t, srv, s)

	ownerID := tid("user-broker-sa-owner")
	createDCUser(t, s, ownerID, "broker-sa-owner@test.com", projectID, store.ProjectRoleOwner)
	ensureHubMembership(context.Background(), s, ownerID)
	ownerUser := &store.User{
		ID:          ownerID,
		Email:       "broker-sa-owner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
	}

	variants := []struct {
		name  string
		email string
	}{
		{"configured_exact", configuredEmail},
		{"configured_mixed_case", "Broker-Configured@Host-Proj.IAM.GServiceAccount.COM"},
		{"configured_trimmed", "  BROKER-CONFIGURED@host-proj.iam.gserviceaccount.com  "},
		{"observed_exact", observedEmail},
		{"observed_mixed_case", "Broker-Observed@Host-Proj.IAM.GServiceAccount.COM"},
		{"observed_trimmed", "  BROKER-OBSERVED@host-proj.iam.gserviceaccount.com  "},
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
					"projectId": "host-proj",
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
				assert.Contains(t, errResp.Error.Message, "Runtime Broker's host GCP service account")
			})
		}
	}

	rows, err := s.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Zero(t, gen.verifyCalls.Load())
}

func TestGCPServiceAccount_RefuseVerifyAndAssignForBrokerHostIdentity(t *testing.T) {
	const (
		configuredEmail = "broker-configured@host-proj.iam.gserviceaccount.com"
		observedEmail   = "broker-observed@host-proj.iam.gserviceaccount.com"
	)

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: "hub-runner@hub-proj.iam.gserviceaccount.com"}
	srv.SetGCPTokenGenerator(gen)

	seedBrokerWithHostSAs(t, s, tid("broker-verify-cfg"), configuredEmail, "")
	seedBrokerWithHostSAs(t, s, tid("broker-verify-obs"), "", observedEmail)

	projectID := createTestProjectForSA(t, srv, s)
	ctx := context.Background()

	projSA := &store.GCPServiceAccount{
		ID:                 tid("sa-broker-proj"),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              "Broker-Configured@Host-Proj.iam.gserviceaccount.com",
		ProjectID:          "host-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "dev-user",
		CreatedAt:          time.Now(),
	}
	hubSA := &store.GCPServiceAccount{
		ID:                 tid("sa-broker-hub"),
		Scope:              store.ScopeHub,
		ScopeID:            srv.HubID(),
		Email:              observedEmail,
		ProjectID:          "host-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "dev-user",
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projSA))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))

	// Nested verify refuses configured broker host SA with 400 identity_reserved.
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, projSA.ID), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)

	// Flat verify refuses observed broker host SA with 400 identity_reserved.
	rec = doRequest(t, srv, http.MethodPost, flatSAPath+hubSA.ID+"/verify", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	errResp = ErrorResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)
	assert.Zero(t, gen.verifyCalls.Load())

	// Assignment gate refuses both configured and observed broker host SAs.
	for _, sa := range []*store.GCPServiceAccount{projSA, hubSA} {
		denial := srv.evaluateSAAssignment(ctx, nil, sa, projectID, SurfaceAgentCreate)
		require.NotNil(t, denial)
		assert.Equal(t, saAssignDenyValidation, denial.kind)
		assert.Equal(t, ErrCodeIdentityReserved, denial.code)
	}
}

func TestGCPTokenMintPaths_RefuseBrokerHostIdentityAndPreserveTranslationRecord(t *testing.T) {
	const (
		configuredEmail = "broker-host-cfg@host-proj.iam.gserviceaccount.com"
		observedEmail   = "broker-host-obs@host-proj.iam.gserviceaccount.com"
	)

	cases := []struct {
		name         string
		saEmail      string
		expectStatus int
		expectCalls  int32
	}{
		{"configured_user_row_refused", configuredEmail, http.StatusForbidden, 0},
		{"configured_mixed_case_refused", "Broker-Host-Cfg@Host-Proj.IAM.GServiceAccount.COM", http.StatusForbidden, 0},
		{"observed_user_row_refused", observedEmail, http.StatusForbidden, 0},
		{"normal_sa_allowed", "worker@host-proj.iam.gserviceaccount.com", http.StatusOK, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			gen := &reservedTestTokenGenerator{email: "hub-sa@hub-proj.iam.gserviceaccount.com"}
			srv.SetGCPTokenGenerator(gen)

			ctx := context.Background()
			broker := seedBrokerWithHostSAs(t, s, tid("broker-mint-"+tc.name), configuredEmail, observedEmail)

			projectID := tid("proj-broker-mint-" + tc.name)
			require.NoError(t, s.CreateProject(ctx, &store.Project{
				ID: projectID, Name: "broker-mint-" + tc.name, Slug: "broker-mint-" + tc.name,
				Created: time.Now(), Updated: time.Now(),
			}))

			userID := tid("user-broker-mint-" + tc.name)
			createDCUser(t, s, userID, "owner@test.com", projectID, store.ProjectRoleOwner)

			saID := tid("sa-broker-mint-" + tc.name)
			sa := &store.GCPServiceAccount{
				ID:                 saID,
				Scope:              store.ScopeProject,
				ScopeID:            projectID,
				Email:              tc.saEmail,
				ProjectID:          "host-proj",
				Verified:           true,
				VerifiedAt:         time.Now(),
				VerificationStatus: store.GCPVerificationVerified,
				CreatedBy:          userID,
				CreatedAt:          time.Now(),
			}
			require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

			agentID := tid("agent-broker-mint-" + tc.name)
			agent := &store.Agent{
				ID:              agentID,
				Slug:            "agent-broker-mint-" + tc.name,
				Name:            "agent-broker-mint-" + tc.name,
				ProjectID:       projectID,
				RuntimeBrokerID: broker.ID,
				Phase:           string(state.PhaseRunning),
				OwnerID:         userID,
				CreatedBy:       userID,
				Ancestry:        []string{userID},
				AppliedConfig: &store.AgentAppliedConfig{
					GCPIdentity: &store.GCPIdentityConfig{
						MetadataMode:        store.GCPMetadataModeAssign,
						ServiceAccountID:    saID,
						ServiceAccountEmail: tc.saEmail,
						ProjectID:           "host-proj",
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

			recAccess := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-token",
				map[string]any{"scopes": []string{"https://www.googleapis.com/auth/cloud-platform"}}, token)
			assert.Equal(t, tc.expectStatus, recAccess.Code, "body: %s", recAccess.Body.String())

			recID := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/gcp-identity-token",
				map[string]string{"audience": "https://example.com"}, token)
			assert.Equal(t, tc.expectStatus, recID.Code, "body: %s", recID.Body.String())

			assert.Equal(t, tc.expectCalls, gen.accessCalls.Load())
			assert.Equal(t, tc.expectCalls, gen.idCalls.Load())
		})
	}

	// Verify admissibleGCPServiceAccount preserves Hub-created passthrough
	// translation records on their bound broker while still refusing
	// user-registered rows and wrong-broker bindings.
	t.Run("translation_record_admissible_only_on_bound_broker", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetGCPTokenGenerator(&reservedTestTokenGenerator{email: "hub-sa@hub-proj.iam.gserviceaccount.com"})
		srv.saAssignCheckMode = SAAssignCheckEnforce
		ctx := context.Background()

		broker := seedBrokerWithHostSAs(t, s, tid("broker-trans-adm"), configuredEmail, "")
		projectID := createTestProjectForSA(t, srv, s)

		transSA := &store.GCPServiceAccount{
			ID:                 tid("sa-trans-adm"),
			Scope:              store.ScopeHub,
			ScopeID:            broker.ID,
			Email:              configuredEmail,
			ProjectID:          "host-proj",
			Verified:           true,
			VerifiedAt:         time.Now(),
			VerificationStatus: store.GCPVerificationVerified,
			CreatedBy:          store.HostSATranslationCreator,
			CreatedAt:          time.Now(),
		}
		require.NoError(t, s.CreateGCPServiceAccount(ctx, transSA))

		gcpID := &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountID:    transSA.ID,
			ServiceAccountEmail: transSA.Email,
			ProjectID:           "host-proj",
		}
		got, err := srv.admissibleGCPServiceAccount(ctx, gcpID, projectID, broker.ID)
		require.NoError(t, err)
		assert.Equal(t, transSA.ID, got.ID)

		_, err = srv.admissibleGCPServiceAccount(ctx, gcpID, projectID, "other-broker")
		assert.ErrorIs(t, err, errGCPSABrokerMismatch)

		// Row-aware refusal skips the broker-host match for translation records
		// so direct-reference surfaces emit their own dedicated refusal, while
		// still refusing the Hub's own SA even on a translation record.
		_, _, refused, err := srv.gcpServiceAccountRefusal(ctx, transSA)
		require.NoError(t, err)
		assert.False(t, refused)

		hubTransSA := *transSA
		hubTransSA.Email = "hub-sa@hub-proj.iam.gserviceaccount.com"
		code, _, refused, err := srv.gcpServiceAccountRefusal(ctx, &hubTransSA)
		require.NoError(t, err)
		assert.True(t, refused)
		assert.Equal(t, ErrCodeIdentityReserved, code)
	})
}

func TestCloneProjectGCPServiceAccounts_SkipsBrokerHostIdentities(t *testing.T) {
	const (
		configuredEmail = "broker-cfg@host-proj.iam.gserviceaccount.com"
		observedEmail   = "broker-obs@host-proj.iam.gserviceaccount.com"
	)

	srv, s := testServer(t)
	ctx := context.Background()
	seedBrokerWithHostSAs(t, s, tid("broker-clone-cfg"), configuredEmail, "")
	seedBrokerWithHostSAs(t, s, tid("broker-clone-obs"), "", observedEmail)

	srcID := tid("proj-clone-broker-src")
	cloneID := tid("proj-clone-broker-dst")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: srcID, Name: "clone-broker-src", Slug: "clone-broker-src", Created: time.Now(), Updated: time.Now(),
	}))

	saNormal := &store.GCPServiceAccount{
		ID: tid("sa-clone-br-normal"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "worker@host-proj.iam.gserviceaccount.com", ProjectID: "host-proj",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now().Add(-3 * time.Minute),
	}
	saCfg := &store.GCPServiceAccount{
		ID: tid("sa-clone-br-cfg"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "Broker-Cfg@Host-Proj.iam.gserviceaccount.com", ProjectID: "host-proj",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	saObs := &store.GCPServiceAccount{
		ID: tid("sa-clone-br-obs"), Scope: store.ScopeProject, ScopeID: srcID,
		Email: observedEmail, ProjectID: "host-proj",
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now().Add(-1 * time.Minute),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saNormal))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saCfg))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saObs))

	cloneProj := &store.Project{
		ID: cloneID, Name: "clone-broker-dst", Slug: "clone-broker-dst",
		Annotations: map[string]string{projectSettingDefaultGCPIdentitySAID: saCfg.ID},
		Created:     time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, cloneProj))

	var rollback []func()
	require.NoError(t, srv.cloneProjectGCPServiceAccounts(ctx, srcID, cloneProj, "dev-user", &rollback))

	clonedSAs, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: cloneID,
	})
	require.NoError(t, err)
	require.Len(t, clonedSAs, 1)
	assert.Equal(t, saNormal.Email, clonedSAs[0].Email)

	reloaded, err := s.GetProject(ctx, cloneID)
	require.NoError(t, err)
	assert.Empty(t, reloaded.Annotations[projectSettingDefaultGCPIdentitySAID])
}

func TestScanReservedGCPServiceAccounts_BrokerHostIdentities(t *testing.T) {
	const (
		configuredEmail = "broker-scan-cfg@host-proj.iam.gserviceaccount.com"
		observedEmail   = "broker-scan-obs@host-proj.iam.gserviceaccount.com"
	)

	srv, s := testServer(t)
	// Intentionally leave Hub token generator nil to verify broker host SAs are
	// still scanned even when Hub's own identity is unknown.
	srv.SetGCPTokenGenerator(nil)
	ctx := context.Background()

	broker := seedBrokerWithHostSAs(t, s, tid("broker-scan-1"), configuredEmail, observedEmail)
	projectID := createTestProjectForSA(t, srv, s)

	matchCfg := &store.GCPServiceAccount{
		ID: tid("sa-scan-br-cfg"), Scope: store.ScopeProject, ScopeID: projectID,
		Email: " Broker-Scan-Cfg@Host-Proj.IAM.GServiceAccount.COM ", ProjectID: "host-proj",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now(),
	}
	matchObs := &store.GCPServiceAccount{
		ID: tid("sa-scan-br-obs"), Scope: store.ScopeHub, ScopeID: srv.HubID(),
		Email: observedEmail, ProjectID: "host-proj",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now(),
	}
	// Legitimate passthrough translation record must NOT be flagged.
	translationRec := &store.GCPServiceAccount{
		ID: tid("sa-scan-br-trans"), Scope: store.ScopeHub, ScopeID: broker.ID,
		Email: configuredEmail, ProjectID: "host-proj",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: store.HostSATranslationCreator, CreatedAt: time.Now(),
	}
	normalSA := &store.GCPServiceAccount{
		ID: tid("sa-scan-br-normal"), Scope: store.ScopeProject, ScopeID: projectID,
		Email: "worker@host-proj.iam.gserviceaccount.com", ProjectID: "host-proj",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: "dev-user", CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, matchCfg))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, matchObs))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, translationRec))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, normalSA))

	flagged, err := srv.ScanReservedGCPServiceAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, flagged)

	for _, id := range []string{matchCfg.ID, matchObs.ID} {
		got, err := s.GetGCPServiceAccount(ctx, id)
		require.NoError(t, err)
		assert.False(t, got.Verified)
		assert.Equal(t, store.GCPVerificationFailed, got.VerificationStatus)
		assert.Equal(t, gcpSABrokerHostIdentityFlagError, got.VerificationError)
	}

	// Translation record and normal SA remain untouched.
	for _, id := range []string{translationRec.ID, normalSA.ID} {
		got, err := s.GetGCPServiceAccount(ctx, id)
		require.NoError(t, err)
		assert.True(t, got.Verified)
		assert.Equal(t, store.GCPVerificationVerified, got.VerificationStatus)
		assert.Empty(t, got.VerificationError)
	}

	// Status endpoint surfaces the flagged state for broker host SA.
	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/status", projectID, matchCfg.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var st GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.False(t, st.Verification.Verified)
	assert.Equal(t, store.GCPVerificationFailed, st.Verification.Status)
	assert.Equal(t, gcpSABrokerHostIdentityFlagError, st.Verification.Error)
	assert.Contains(t, st.NextStep.Message, "Remove this service account registration")

	// Re-running is idempotent.
	flagged2, err := srv.ScanReservedGCPServiceAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, flagged2)
}

func TestLifecycleHook_RefusesBrokerHostIdentityLiveAtFireAndWrite(t *testing.T) {
	const brokerEmail = "broker-hook@host-proj.iam.gserviceaccount.com"

	srv, s := testServer(t)
	gen := &reservedTestTokenGenerator{email: "hub-runner@hub-proj.iam.gserviceaccount.com"}
	srv.SetGCPTokenGenerator(gen)
	ctx := context.Background()

	seedBrokerWithHostSAs(t, s, tid("broker-hook-host"), "", brokerEmail)

	adminID := tid("user-broker-hook-admin")
	adminEmail := "broker-hook-admin@test.com"
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: adminEmail, DisplayName: "Hook Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now().Add(-time.Hour),
	}))
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	adminUser := &store.User{
		ID: adminID, Email: adminEmail, DisplayName: "Hook Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive,
	}

	brokerSA := &store.GCPServiceAccount{
		ID: tid("sa-hook-broker"), Scope: store.ScopeHub, ScopeID: srv.HubID(),
		Email: "Broker-Hook@Host-Proj.IAM.GServiceAccount.COM", ProjectID: "host-proj",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: adminID, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, brokerSA))

	exec := NewHTTPExecutor(s, gen, nil, slog.Default(), srv)
	action := &store.LifecycleHookAction{
		Type:   store.LifecycleHookActionHTTP,
		Method: http.MethodPost,
		URL:    "https://example.com/hook",
	}

	// 1. Live fire-time refusal.
	reservedHook := &store.LifecycleHook{
		ID: tid("hook-fire-broker"), Name: "fire-broker", ScopeType: store.LifecycleHookScopeHub,
		ExecutionIdentity: brokerSA.ID, CreatedBy: adminEmail, Created: time.Now(),
	}
	_, _, err = exec.resolveIdentityAndToken(ctx, reservedHook, action)
	require.Error(t, err)
	var denied *HookFireDeniedError
	require.True(t, errors.As(err, &denied), "expected HookFireDeniedError, got %T: %v", err, err)
	assert.Equal(t, HookFireIdentityNotAdmissible, denied.Cause)
	assert.Zero(t, gen.accessCalls.Load())

	// 2. Write-time refusal on POST /api/v1/admin/lifecycle-hooks.
	recCreate := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/admin/lifecycle-hooks", map[string]any{
		"name":              "create-broker-reserved-hook",
		"scopeType":         store.LifecycleHookScopeHub,
		"trigger":           "running",
		"action":            action,
		"executionIdentity": brokerSA.ID,
		"enabled":           true,
	})
	require.Equal(t, http.StatusBadRequest, recCreate.Code, "body: %s", recCreate.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeIdentityReserved, errResp.Error.Code)
}

type brokerListCountingFaultStore struct {
	store.Store
	fault     *storeFaultSwitch
	listCalls atomic.Int32
}

func (w *brokerListCountingFaultStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	w.listCalls.Add(1)
	if w.fault.Active() {
		return nil, errors.New("injected broker list failure")
	}
	return w.Store.ListRuntimeBrokers(ctx, filter, opts)
}

func TestGCPServiceAccount_BrokerHostCheck_NoNPlusOneAndFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	wrapper, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *brokerListCountingFaultStore {
		return &brokerListCountingFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	seedBrokerWithHostSAs(t, s, tid("broker-nplus1"), "broker-n1@host-proj.iam.gserviceaccount.com", "")
	projectID := createTestProjectForSA(t, srv, s)

	// Seed multiple SAs and verify ScanReservedGCPServiceAccounts lists brokers once (no N+1).
	for i := 0; i < 5; i++ {
		require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
			ID: tid(fmt.Sprintf("sa-nplus1-%d", i)), Scope: store.ScopeProject, ScopeID: projectID,
			Email: fmt.Sprintf("worker-%d@host-proj.iam.gserviceaccount.com", i), ProjectID: "host-proj",
			Verified: true, VerificationStatus: store.GCPVerificationVerified,
			CreatedBy: "dev-user", CreatedAt: time.Now(),
		}))
	}

	wrapper.listCalls.Store(0)
	flagged, err := srv.ScanReservedGCPServiceAccounts(ctx)
	require.NoError(t, err)
	assert.Zero(t, flagged)
	assert.Equal(t, int32(1), wrapper.listCalls.Load(), "ScanReservedGCPServiceAccounts must load brokers once, not per SA")

	// Arm fault: refusal must fail closed (never silently allow on store error).
	fault.Arm()
	_, _, refused, err := srv.gcpServiceAccountEmailRefusal(ctx, "worker@host-proj.iam.gserviceaccount.com")
	require.Error(t, err)
	assert.False(t, refused)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		map[string]string{
			"email":     "worker@host-proj.iam.gserviceaccount.com",
			"projectId": "host-proj",
		})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "registration must fail closed on broker list error")
}
