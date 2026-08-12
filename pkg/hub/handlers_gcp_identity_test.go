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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestProjectForSA(t *testing.T, srv *Server, s store.Store) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]string{
		"name": "test-project-sa",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "create project: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return project.ID
}

func TestCreateGCPServiceAccount_Success(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.Equal(t, "agent@my-project.iam.gserviceaccount.com", sa.Email)
	assert.Equal(t, "my-project", sa.ProjectID)
	assert.NotEmpty(t, sa.ID)
}

func TestCreateGCPServiceAccount_MissingEmail(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	body := map[string]string{
		"projectId": "my-project",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "email")
}

func TestCreateGCPServiceAccount_InferProjectIDFromEmail(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	body := map[string]string{
		"email": "agent@my-project.iam.gserviceaccount.com",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.Equal(t, "my-project", sa.ProjectID)
}

func TestCreateGCPServiceAccount_CannotInferProjectID(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	body := map[string]string{
		"email": "agent@example.com",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)

	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "projectId")
}

func TestCreateGCPServiceAccount_InvalidJSON(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	rec := doRequestRaw(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		[]byte("not-json"), "application/json")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "invalid request body")
}

func TestCreateGCPServiceAccount_ProjectNotFound(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/nonexistent-project-id/gcp-service-accounts", body)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCreateGCPServiceAccount_Duplicate(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}

	// First create should succeed
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "first create: %s", rec.Body.String())

	// Second create with same email should conflict
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusConflict, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeConflict, errResp.Error.Code)
}

// mockGCPServiceAccountAdmin is a test implementation of GCPServiceAccountAdmin.
type mockGCPServiceAccountAdmin struct {
	createErr   error
	policyErr   error
	deleteErr   error
	createdSAs  []string // track created account IDs
	deletedSAs  []string // track deleted SA emails (cleanup calls)
	lastEmail   string
	lastProject string

	// Track IAM mutations for assertions.
	iamPolicies []mockIAMPolicyCall // SA-level SetIAMPolicy calls
}

type mockIAMPolicyCall struct {
	SAEmail string
	Member  string
	Role    string
}

func (m *mockGCPServiceAccountAdmin) CreateServiceAccount(_ context.Context, projectID, accountID, _, _ string) (string, string, error) {
	if m.createErr != nil {
		return "", "", m.createErr
	}
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", accountID, projectID)
	m.createdSAs = append(m.createdSAs, accountID)
	m.lastEmail = email
	m.lastProject = projectID
	return email, "unique-id-123", nil
}

func (m *mockGCPServiceAccountAdmin) DeleteServiceAccount(_ context.Context, saEmail string) error {
	m.deletedSAs = append(m.deletedSAs, saEmail)
	return m.deleteErr
}

func (m *mockGCPServiceAccountAdmin) SetIAMPolicy(_ context.Context, saEmail, member, role string) error {
	m.iamPolicies = append(m.iamPolicies, mockIAMPolicyCall{
		SAEmail: saEmail,
		Member:  member,
		Role:    role,
	})
	return m.policyErr
}

func testServerWithMinting(t *testing.T) (*Server, store.Store, *mockGCPServiceAccountAdmin) {
	t.Helper()
	srv, s := testServer(t)
	mock := &mockGCPServiceAccountAdmin{}
	srv.SetGCPServiceAccountAdmin(mock)
	srv.SetGCPProjectID("test-hub-project")

	// Set a mock token generator so the hub SA email is available
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})

	return srv, s, mock
}

// mockGCPTokenGenerator implements GCPTokenGenerator for testing.
type mockGCPTokenGenerator struct {
	email string
}

func (m *mockGCPTokenGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	return &GCPAccessToken{AccessToken: "test-token", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (m *mockGCPTokenGenerator) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "test-id-token"}, nil
}

func (m *mockGCPTokenGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	return nil
}

func (m *mockGCPTokenGenerator) ServiceAccountEmail() string {
	return m.email
}

// countingMockAdmin is a mock that fails SetIAMPolicy on the first N calls
// to simulate GCP eventual consistency, then succeeds on subsequent calls.
type countingMockAdmin struct {
	failUntilCall   int // fail SetIAMPolicy until this call count (1-indexed)
	failErr         error
	policyCallCount int
	createdSAs      []string
	deletedSAs      []string
}

func (m *countingMockAdmin) CreateServiceAccount(_ context.Context, projectID, accountID, _, _ string) (string, string, error) {
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", accountID, projectID)
	m.createdSAs = append(m.createdSAs, accountID)
	return email, "unique-id-123", nil
}

func (m *countingMockAdmin) DeleteServiceAccount(_ context.Context, saEmail string) error {
	m.deletedSAs = append(m.deletedSAs, saEmail)
	return nil
}

func (m *countingMockAdmin) SetIAMPolicy(_ context.Context, _, _, _ string) error {
	m.policyCallCount++
	if m.policyCallCount <= m.failUntilCall {
		return m.failErr
	}
	return nil
}

func TestMintGCPServiceAccount_Success(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.True(t, sa.Managed)
	assert.True(t, sa.Verified)
	assert.Contains(t, sa.Email, "@test-hub-project.iam.gserviceaccount.com")
	assert.Contains(t, sa.Email, "scion-")
	assert.Equal(t, "test-hub-project", sa.ProjectID)
	assert.Len(t, mock.createdSAs, 1)
}

func TestMintGCPServiceAccount_SelfActAs_Default(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	// No allow_self_act_as field → default true → both grants
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Len(t, mock.iamPolicies, 2, "expected tokenCreator + serviceAccountUser grants")
	assert.Equal(t, "roles/iam.serviceAccountTokenCreator", mock.iamPolicies[0].Role)
	assert.Equal(t, "serviceAccount:hub-sa@test-hub-project.iam.gserviceaccount.com", mock.iamPolicies[0].Member)
	assert.Equal(t, "roles/iam.serviceAccountUser", mock.iamPolicies[1].Role)
	// Self-grant: member is serviceAccount:<minted-sa-email>
	assert.Equal(t, "serviceAccount:"+mock.lastEmail, mock.iamPolicies[1].Member)
	assert.Equal(t, mock.lastEmail, mock.iamPolicies[1].SAEmail)
}

func TestMintGCPServiceAccount_SelfActAs_ExplicitFalse(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	// Explicit false → only tokenCreator grant
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]interface{}{
			"allow_self_act_as": false,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Len(t, mock.iamPolicies, 1, "expected only tokenCreator grant")
	assert.Equal(t, "roles/iam.serviceAccountTokenCreator", mock.iamPolicies[0].Role)
}

func TestMintGCPServiceAccount_SelfActAs_ExplicitTrue(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	// Explicit true → both grants
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]interface{}{
			"allow_self_act_as": true,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Len(t, mock.iamPolicies, 2, "expected tokenCreator + serviceAccountUser grants")
	assert.Equal(t, "roles/iam.serviceAccountTokenCreator", mock.iamPolicies[0].Role)
	assert.Equal(t, "roles/iam.serviceAccountUser", mock.iamPolicies[1].Role)
	assert.Equal(t, "serviceAccount:"+mock.lastEmail, mock.iamPolicies[1].Member)
}

func TestMintGCPServiceAccount_CustomAccountID(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{
			"account_id":   "my-pipeline",
			"display_name": "My Pipeline SA",
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.True(t, sa.Managed)
	assert.Equal(t, "scion-my-pipeline@test-hub-project.iam.gserviceaccount.com", sa.Email)
	assert.Equal(t, "My Pipeline SA", sa.DisplayName)
	assert.Equal(t, "scion-my-pipeline", mock.createdSAs[0])
}

func TestMintGCPServiceAccount_AccountIDTooLong(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{
			"account_id": "this-is-a-very-long-account-id-that-exceeds",
		})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
}

func TestMintGCPServiceAccount_NotConfigured(t *testing.T) {
	srv, _ := testServer(t) // No minting configured
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestMintGCPServiceAccount_ProjectNotFound(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/nonexistent-project-id/gcp-service-accounts/mint",
		map[string]string{})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMintGCPServiceAccount_NoAuth(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequestNoAuth(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	// Should be forbidden without auth
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"expected 401 or 403, got %d", rec.Code)
}

func TestMintGCPServiceAccount_PerProjectCap(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	srv.config.GCPMintCapPerProject = 2
	projectID := createTestProjectForSA(t, srv, nil)

	// Mint first two — should succeed
	for i := 0; i < 2; i++ {
		rec := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
			map[string]string{})
		require.Equal(t, http.StatusCreated, rec.Code, "mint %d: %s", i+1, rec.Body.String())
	}

	// Third mint should be rejected
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusConflict, rec.Code, "expected cap enforcement: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "per-project mint limit")
}

func TestMintGCPServiceAccount_GlobalCap(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	srv.config.GCPMintCapGlobal = 3

	// Create two projects and mint in each
	projectID1 := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]string{
		"name": "test-project-sa-2",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	var project2 struct{ ID string }
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project2))
	projectID2 := project2.ID

	// Mint 2 in project 1, 1 in project 2 (total 3)
	for i := 0; i < 2; i++ {
		rec := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID1),
			map[string]string{})
		require.Equal(t, http.StatusCreated, rec.Code)
	}
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID2),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Fourth mint (in either project) should be rejected
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID2),
		map[string]string{})
	require.Equal(t, http.StatusConflict, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "global mint limit")
}

func TestListGCPServiceAccounts_IncludesMintQuota(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	srv.config.GCPMintCapPerProject = 5
	srv.config.GCPMintCapGlobal = 10
	projectID := createTestProjectForSA(t, srv, nil)

	// Mint one SA
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code)

	// List should include quota info
	rec = doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Items     []json.RawMessage `json:"items"`
		MintQuota *struct {
			ProjectMinted int `json:"project_minted"`
			ProjectCap    int `json:"project_cap"`
			GlobalMinted  int `json:"global_minted"`
			GlobalCap     int `json:"global_cap"`
		} `json:"mint_quota"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.MintQuota, "mint_quota should be present")
	assert.Equal(t, 1, resp.MintQuota.ProjectMinted)
	assert.Equal(t, 5, resp.MintQuota.ProjectCap)
	assert.Equal(t, 1, resp.MintQuota.GlobalMinted)
	assert.Equal(t, 10, resp.MintQuota.GlobalCap)
}

func TestMintGCPServiceAccount_ManagedFlagSet(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code)

	var sa struct {
		Managed   bool   `json:"managed"`
		ManagedBy string `json:"managedBy"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.True(t, sa.Managed)
}

func TestCreateGCPServiceAccount_AutoVerifySuccess(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	// Set a mock token generator that always succeeds
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})

	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		store.GCPServiceAccount
		VerificationFailed  bool `json:"verificationFailed"`
		VerificationDetails *struct {
			HubServiceAccountEmail string `json:"hubServiceAccountEmail"`
			TargetEmail            string `json:"targetEmail"`
		} `json:"verificationDetails"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Verified, "should be verified")
	assert.Equal(t, "verified", resp.VerificationStatus)
	assert.False(t, resp.VerificationFailed, "verificationFailed should be false")
	assert.Nil(t, resp.VerificationDetails, "no verification details on success")
}

func TestCreateGCPServiceAccount_AutoVerifyFailure(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	// Set a mock token generator that always fails verification
	srv.SetGCPTokenGenerator(&mockGCPTokenGeneratorVerifyFail{
		email:     "hub@test.iam.gserviceaccount.com",
		verifyErr: fmt.Errorf("hub service account cannot impersonate agent@my-project.iam.gserviceaccount.com"),
	})

	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	// SA is still created successfully
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		store.GCPServiceAccount
		VerificationFailed  bool `json:"verificationFailed"`
		VerificationDetails *struct {
			HubServiceAccountEmail string `json:"hubServiceAccountEmail"`
			TargetEmail            string `json:"targetEmail"`
		} `json:"verificationDetails"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Verified, "should not be verified")
	assert.Equal(t, "failed", resp.VerificationStatus)
	assert.True(t, resp.VerificationFailed, "verificationFailed should be true")
	require.NotNil(t, resp.VerificationDetails, "should include verification details")
	assert.Equal(t, "hub@test.iam.gserviceaccount.com", resp.VerificationDetails.HubServiceAccountEmail)
	assert.Equal(t, "agent@my-project.iam.gserviceaccount.com", resp.VerificationDetails.TargetEmail)
}

// mockGCPTokenGeneratorVerifyFail is a mock that fails VerifyImpersonation but succeeds on other ops.
type mockGCPTokenGeneratorVerifyFail struct {
	email     string
	verifyErr error
}

func (m *mockGCPTokenGeneratorVerifyFail) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	return &GCPAccessToken{AccessToken: "test-token", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (m *mockGCPTokenGeneratorVerifyFail) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "test-id-token"}, nil
}

func (m *mockGCPTokenGeneratorVerifyFail) VerifyImpersonation(_ context.Context, _ string) error {
	return m.verifyErr
}

func (m *mockGCPTokenGeneratorVerifyFail) ServiceAccountEmail() string {
	return m.email
}

func TestMintGCPServiceAccount_PerProjectCap_DifferentProjects(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	srv.config.GCPMintCapPerProject = 1

	projectID1 := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]string{
		"name": "test-project-sa-3",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	var project2 struct{ ID string }
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project2))

	// Mint in project 1 — should succeed
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID1),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Mint in project 2 — should also succeed (different project)
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project2.ID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Second mint in project 1 — should be rejected
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID1),
		map[string]string{})
	require.Equal(t, http.StatusConflict, rec.Code)
}

// ============================================================================
// Mint Failure Semantics Tests (P7C)
// ============================================================================

// TestMintGCPServiceAccount_IAMGrantFailure_NoVerifiedTrue verifies that when
// the tokenCreator IAM grant fails, the SA is NOT stored as Verified=true and
// the caller receives a non-2xx response.
func TestMintGCPServiceAccount_IAMGrantFailure_NoVerifiedTrue(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	mock.policyErr = fmt.Errorf("IAM policy mutation failed: permission denied")
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})

	// Must NOT be 2xx
	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "IAM grants failed")

	// No SA should have been stored — the mint failed before store.
	managed := true
	count, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no SA should be stored when IAM grant fails")
}

// TestMintGCPServiceAccount_IAMGrantFailure_CleanupAttempted verifies that when
// the IAM grant fails, the handler best-effort deletes the orphaned SA.
func TestMintGCPServiceAccount_IAMGrantFailure_CleanupAttempted(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	mock.policyErr = fmt.Errorf("IAM policy mutation failed")
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})

	require.Equal(t, http.StatusBadGateway, rec.Code)

	// The SA should have been created in GCP then deleted as cleanup.
	require.Len(t, mock.createdSAs, 1, "SA should have been created in GCP")
	require.Len(t, mock.deletedSAs, 1, "cleanup delete should have been attempted")
	assert.Contains(t, mock.deletedSAs[0], "@test-hub-project.iam.gserviceaccount.com")
}

// TestMintGCPServiceAccount_IAMGrantFailure_CleanupFailsToo verifies that when
// the IAM grant fails AND the cleanup delete also fails, the handler still
// returns a non-2xx response and does not store a Verified=true record.
func TestMintGCPServiceAccount_IAMGrantFailure_CleanupFailsToo(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	mock.policyErr = fmt.Errorf("IAM policy mutation failed")
	mock.deleteErr = fmt.Errorf("delete also failed")
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})

	require.Equal(t, http.StatusBadGateway, rec.Code)

	// Cleanup was attempted even though it failed.
	require.Len(t, mock.deletedSAs, 1)

	// No SA stored in the hub store.
	managed := true
	count, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// TestMintGCPServiceAccount_Success_StillWorks is a regression test ensuring
// that the normal successful mint path continues to work correctly after the
// failure-semantics fix.
func TestMintGCPServiceAccount_Success_StillWorks(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.True(t, sa.Verified, "successful mint should be Verified=true")
	assert.Equal(t, store.GCPVerificationVerified, sa.VerificationStatus)
	assert.True(t, sa.Managed)
	assert.Contains(t, sa.Email, "@test-hub-project.iam.gserviceaccount.com")

	// Verify stored record is also verified.
	stored, err := s.GetGCPServiceAccount(context.Background(), sa.ID)
	require.NoError(t, err)
	assert.True(t, stored.Verified)
	assert.Equal(t, store.GCPVerificationVerified, stored.VerificationStatus)

	// No cleanup deletions should have happened.
	assert.Len(t, mock.deletedSAs, 0, "no cleanup deletes on success")
	assert.Len(t, mock.createdSAs, 1, "exactly one SA created")
}

// TestMintGCPServiceAccount_NilTokenGenerator verifies that minting fails
// gracefully when gcpTokenGenerator is nil. The Hub cannot impersonate any SA
// without a token generator, so a minted SA would be unusable.
func TestMintGCPServiceAccount_NilTokenGenerator(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	// Clear the token generator — simulates misconfiguration.
	srv.SetGCPTokenGenerator(nil)
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "token generator")

	// SA was created in GCP then cleanup-deleted.
	require.Len(t, mock.createdSAs, 1)
	require.Len(t, mock.deletedSAs, 1)

	// No SA stored in the hub.
	managed := true
	count, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// TestMintGCPServiceAccount_EmptyHubEmail verifies that minting fails when the
// token generator is configured but returns an empty service account email.
func TestMintGCPServiceAccount_EmptyHubEmail(t *testing.T) {
	srv, s, mock := testServerWithMinting(t)
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: ""})
	projectID := createTestProjectForSA(t, srv, nil)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "Hub service account email")

	// SA was created in GCP then cleanup-deleted.
	require.Len(t, mock.createdSAs, 1)
	require.Len(t, mock.deletedSAs, 1)

	// No SA stored in the hub.
	managed := true
	count, err := s.CountGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// TestMintGCPServiceAccount_IAMGrantFailure_DoesNotCountAgainstQuota verifies
// that a failed mint does not permanently consume a quota slot.
func TestMintGCPServiceAccount_IAMGrantFailure_DoesNotCountAgainstQuota(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	srv.config.GCPMintCapPerProject = 1
	projectID := createTestProjectForSA(t, srv, nil)

	// First attempt fails on IAM grant.
	mock.policyErr = fmt.Errorf("IAM policy failed")
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusBadGateway, rec.Code)

	// Fix the mock and retry — should succeed since the failed attempt didn't
	// store anything that would count against the quota.
	mock.policyErr = nil
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "retry should succeed: %s", rec.Body.String())
}

func TestMintGCPServiceAccount_RetryOnEventualConsistency(t *testing.T) {
	srv, _, _ := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	// Use a counter-based mock that fails on the first SetIAMPolicy call with
	// a "does not exist" error (simulating GCP eventual consistency after SA
	// creation) and succeeds on the retry.
	retryMock := &countingMockAdmin{
		failUntilCall: 1,
		failErr:       fmt.Errorf("googleapi: Error 400: Service account does not exist., badRequest"),
	}
	srv.SetGCPServiceAccountAdmin(retryMock)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "mint should succeed after retry: %s", rec.Body.String())

	// The first SetIAMPolicy call failed, the retry (call #2) succeeded.
	assert.GreaterOrEqual(t, retryMock.policyCallCount, 2,
		"expected at least 2 SetIAMPolicy calls (1 failed + 1 retry)")
}

func TestMintGCPServiceAccount_NoRetryOnNonConsistencyError(t *testing.T) {
	srv, _, mock := testServerWithMinting(t)
	projectID := createTestProjectForSA(t, srv, nil)

	// SetIAMPolicy fails with a non-retryable error (no "does not exist").
	mock.policyErr = fmt.Errorf("googleapi: Error 403: permission denied")

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", projectID),
		map[string]string{})
	require.Equal(t, http.StatusBadGateway, rec.Code, "mint should fail immediately for non-retryable error")

	// Only one SetIAMPolicy call should have been made — no retry.
	assert.Equal(t, 1, len(mock.iamPolicies),
		"expected exactly 1 SetIAMPolicy call (no retry for non-consistency error)")

	// SA should have been cleaned up.
	assert.Len(t, mock.deletedSAs, 1, "orphaned SA should be cleaned up")
}

// ============================================================================
// GCP Service Account Authorization Tests
// ============================================================================

// setupGCPAuthzTest creates a test server with three users and a project:
//   - owner: project owner (non-admin member), in project members group
//   - member: project member (non-admin), in project members group
//   - outsider: hub member but NOT in project members group
//
// Returns the server, store, users, and project.
func setupGCPAuthzTest(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.User, *store.Project) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-gcp-owner"),
		Email:       "gcp-owner@test.com",
		DisplayName: "GCP Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	member := &store.User{
		ID:          tid("user-gcp-member"),
		Email:       "gcp-member@test.com",
		DisplayName: "GCP Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	outsider := &store.User{
		ID:          tid("user-gcp-outsider"),
		Email:       "gcp-outsider@test.com",
		DisplayName: "GCP Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	for _, u := range []*store.User{owner, member, outsider} {
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
	}

	project := &store.Project{
		ID:        tid("project-gcp-authz"),
		Name:      "GCP Authz Project",
		Slug:      "gcp-authz-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create project members group and policies (simulates project creation handler)
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Add member to project members group
	membersGroup, err := s.GetGroupBySlug(ctx, "project:gcp-authz-project:members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   member.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	return srv, s, owner, member, outsider, project
}

func TestGCPSA_Create_ProjectOwnerAllowed(t *testing.T) {
	srv, _, owner, _, _, project := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
		map[string]string{"email": "sa@proj.iam.gserviceaccount.com", "projectId": tid("proj")})
	require.Equal(t, http.StatusCreated, rec.Code,
		"project owner should be able to create SA; got: %s", rec.Body.String())
}

func TestGCPSA_Create_MemberDenied(t *testing.T) {
	srv, _, _, member, _, project := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
		map[string]string{"email": "sa@proj.iam.gserviceaccount.com", "projectId": tid("proj")})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"project member should not be able to create SA; got: %s", rec.Body.String())
}

func TestGCPSA_Create_OutsiderDenied(t *testing.T) {
	srv, _, _, _, outsider, project := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, outsider, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
		map[string]string{"email": "sa@proj.iam.gserviceaccount.com", "projectId": tid("proj")})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"outsider should not be able to create SA; got: %s", rec.Body.String())
}

func TestGCPSA_Delete_ProjectOwnerAllowed(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-del-owner"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "del-owner@proj.iam.gserviceaccount.com",
		ProjectID: tid("proj"),
		CreatedBy: owner.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"project owner should be able to delete SA; got: %s", rec.Body.String())
}

func TestGCPSA_Delete_MemberDenied(t *testing.T) {
	srv, s, owner, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-del-member"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "del-member@proj.iam.gserviceaccount.com",
		ProjectID: tid("proj"),
		CreatedBy: owner.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, member, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"project member should not be able to delete SA; got: %s", rec.Body.String())
}

func TestGCPSA_Mint_ProjectOwnerAllowed(t *testing.T) {
	srv, _, owner, _, _, project := setupGCPAuthzTest(t)

	mock := &mockGCPServiceAccountAdmin{}
	srv.SetGCPServiceAccountAdmin(mock)
	srv.SetGCPProjectID("test-hub-project")
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project.ID),
		map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code,
		"project owner should be able to mint SA; got: %s", rec.Body.String())
}

func TestGCPSA_Mint_MemberDenied(t *testing.T) {
	srv, _, _, member, _, project := setupGCPAuthzTest(t)

	mock := &mockGCPServiceAccountAdmin{}
	srv.SetGCPServiceAccountAdmin(mock)
	srv.SetGCPProjectID("test-hub-project")
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/mint", project.ID),
		map[string]string{})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"project member should not be able to mint SA; got: %s", rec.Body.String())
}

func TestGCPSA_Verify_ProjectOwnerAllowed(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-verify-owner"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "verify@proj.iam.gserviceaccount.com",
		ProjectID: tid("proj"),
		CreatedBy: owner.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", project.ID, sa.ID), nil)
	// Should not be 403 — project owner has manage permission
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner should not get 403 for verify; got: %s", rec.Body.String())
}

func TestGCPSA_Verify_MemberDenied(t *testing.T) {
	srv, s, owner, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-verify-member"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "verify-m@proj.iam.gserviceaccount.com",
		ProjectID: tid("proj"),
		CreatedBy: owner.ID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", project.ID, sa.ID), nil)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"project member should not be able to verify SA; got: %s", rec.Body.String())
}

// TestGCPSA_ProjectOwnerCanAddMembers verifies that project owners can add members
// to the project's members group (regression test for missing OwnerID on group).
func TestGCPSA_ProjectOwnerCanAddMembers(t *testing.T) {
	srv, s, owner, _, outsider, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)

	// Project owner should be able to add outsider as a member
	body := AddGroupMemberRequest{
		MemberType: "user",
		MemberID:   outsider.ID,
		Role:       "member",
	}
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/groups/%s/members", membersGroup.ID), body)
	require.Equal(t, http.StatusCreated, rec.Code,
		"project owner should be able to add members to project group; got: %s", rec.Body.String())
}

func TestVerifyGCPServiceAccount_NoTokenGenerator_Returns503(t *testing.T) {
	srv, s := testServer(t) // no token generator configured
	projectID := createTestProjectForSA(t, srv, s)

	// Create a SA to verify
	body := map[string]string{
		"email":     "agent@my-project.iam.gserviceaccount.com",
		"projectId": "my-project",
	}
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "create SA: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))

	// Attempt to verify — should fail with 503 because no token generator
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, sa.ID), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"verify without token generator should return 503; got: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, "gcp_not_configured", errResp.Error.Code)

	// Verify the SA was NOT marked as Verified=true
	storedSA, err := s.GetGCPServiceAccount(context.Background(), sa.ID)
	require.NoError(t, err)
	assert.False(t, storedSA.Verified, "SA should NOT be marked as verified when token generator is nil")
}

func TestProjectIDFromServiceAccountEmail(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"agent@my-project.iam.gserviceaccount.com", "my-project"},
		{"fold-run-infra@foldrun-ptone-argolis.iam.gserviceaccount.com", "foldrun-ptone-argolis"},
		{"sa@example.com", ""},
		{"no-at-sign", ""},
		{"", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, projectIDFromServiceAccountEmail(tt.email), "email=%q", tt.email)
	}
}

// TestGCPServiceAccount_FailedVerificationIsDurable is the end-to-end version of
// the defect P0.1 fixes. The "failed" status and its error message used to exist
// only in the response body of the request that produced them: the next read
// recomputed the status from the verified bool and reported "unverified",
// dropping the diagnostic entirely.
func TestGCPServiceAccount_FailedVerificationIsDurable(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	srv.SetGCPTokenGenerator(&mockGCPTokenGeneratorVerifyFail{
		email:     "hub@test.iam.gserviceaccount.com",
		verifyErr: fmt.Errorf("hub service account cannot impersonate agent@my-project.iam.gserviceaccount.com"),
	})

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		map[string]string{
			"email":     "agent@my-project.iam.gserviceaccount.com",
			"projectId": "my-project",
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var created store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.Equal(t, store.GCPVerificationFailed, created.VerificationStatus)
	require.NotEmpty(t, created.VerificationError)

	// Re-read through the store: status and error must both still be there.
	stored, err := s.GetGCPServiceAccount(context.Background(), created.ID)
	require.NoError(t, err)
	assert.False(t, stored.Verified)
	assert.Equal(t, store.GCPVerificationFailed, stored.VerificationStatus)
	assert.Equal(t, created.VerificationError, stored.VerificationError,
		"the diagnostic must survive the round-trip, not just the response")

	// ...and through the list endpoint the UI actually calls.
	rec = doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var list ListGCPServiceAccountsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, store.GCPVerificationFailed, list.Items[0].VerificationStatus)
	assert.Equal(t, created.VerificationError, list.Items[0].VerificationError)
}

// A retry that succeeds must clear the stale error, not leave it alongside a
// "verified" status.
func TestGCPServiceAccount_VerifyClearsPreviousFailure(t *testing.T) {
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)

	srv.SetGCPTokenGenerator(&mockGCPTokenGeneratorVerifyFail{
		email:     "hub@test.iam.gserviceaccount.com",
		verifyErr: fmt.Errorf("IAM policy not yet propagated"),
	})

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID),
		map[string]string{
			"email":     "agent@my-project.iam.gserviceaccount.com",
			"projectId": "my-project",
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.Equal(t, store.GCPVerificationFailed, created.VerificationStatus)

	// The IAM policy propagates; verify now succeeds.
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	rec = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, created.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	stored, err := s.GetGCPServiceAccount(context.Background(), created.ID)
	require.NoError(t, err)
	assert.True(t, stored.Verified)
	assert.Equal(t, store.GCPVerificationVerified, stored.VerificationStatus)
	assert.Empty(t, stored.VerificationError, "the stale failure message must not outlive the failure")
}
