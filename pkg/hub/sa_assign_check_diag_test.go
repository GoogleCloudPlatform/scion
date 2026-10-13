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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// saCheckEndUserMsg is the end-user text for a check that did not complete.
// It is spelled out here, not taken from the gate, so a change to the gate's
// text fails this test.
const saCheckEndUserMsg = "Could not verify your permission to use this GCP service " +
	"account because the check did not complete; try again"

// countingPTClient counts the Policy Troubleshooter calls that reach pt.
type countingPTClient struct {
	*fakePTClient
	calls int
}

func (c *countingPTClient) TroubleshootIamPolicy(
	ctx context.Context,
	req *policytroubleshooterpb.TroubleshootIamPolicyRequest,
	opts ...gax.CallOption,
) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error) {
	c.calls++
	return c.fakePTClient.TroubleshootIamPolicy(ctx, req, opts...)
}

// saCheckDiagServer returns a create-ready server enforcing the assignment
// check through a cached Policy Troubleshooter checker backed by pt, wired
// to the diagnostic the same way the server command wires it.
func saCheckDiagServer(t *testing.T, pt *fakePTClient) (*Server, *store.GCPServiceAccount, string) {
	t.Helper()
	srv, sa, projectID, _ := saCheckDiagServerCounting(t, pt)
	return srv, sa, projectID
}

func saCheckDiagServerCounting(t *testing.T, pt *fakePTClient) (*Server, *store.GCPServiceAccount, string, *countingPTClient) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	counting := &countingPTClient{fakePTClient: pt}
	checker := NewPolicyTroubleshooterChecker(counting, "hub@test.iam.gserviceaccount.com", false)
	checker.SetCallObserver(srv.NoteSAAssignCheckCall)
	enforceSAAssign(srv, NewCachedCallerPermissionChecker(checker, time.Minute, time.Minute))
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "diag-target@p.iam.gserviceaccount.com")
	// Write this instance's registry row, so the summary's fleet hub
	// status is healthy and only the diagnostic changes the status.
	srv.newHubInstanceRegistry().tick(context.Background())
	return srv, sa, project.ID, counting
}

func createWithSA(t *testing.T, srv *Server, projectID, name string, sa *store.GCPServiceAccount) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      name,
		ProjectID: projectID,
		Task:      "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	})
}

// adminHealthSummary runs one registry tick, as the registry loop does
// every 15 s, so the instance's row carries its current diagnostic, then
// fetches the admin health summary as an admin.
func adminHealthSummary(t *testing.T, srv *Server) HealthSummaryResponse {
	t.Helper()
	srv.newHubInstanceRegistry().tick(context.Background())
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleHealthSummary(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

func saCheckErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())
	return body.Error.Message
}

func TestSACheckDiag_HubIdentityRefused_ShownToAdminAndAssignmentDenied(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	require.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck,
		"no diagnostic before the check has run")

	rec := createWithSA(t, srv, projectID, "diag-denied", sa)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

	resp := adminHealthSummary(t, srv)
	require.NotNil(t, resp.ServiceAccountCheck)
	d := resp.ServiceAccountCheck
	assert.Equal(t, saAssignCheckDiagCause, d.Cause)
	assert.Contains(t, d.Remedy, "Grant the hub's identity")
	assert.Equal(t, saAssignCheckDiagDocsURL, d.DocsURL)
	require.NotNil(t, resp.HubInstances)
	require.Len(t, resp.HubInstances.Items, 1)
	label := resp.HubInstances.Items[0].Label
	assert.Equal(t, []string{label}, d.Instances, "the instance whose row reports it")
	assert.Equal(t, HealthStatusDegraded, resp.Status)
	// The diagnostic reaches the summary through the instance's row, as
	// check sa_assign_check, and counts through the fleet rule.
	assert.Equal(t, "degraded", resp.HubInstances.Items[0].Checks[saAssignCheckName])
	assert.Equal(t, HealthStatusDegraded, resp.Hub.Status)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
		Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: srv.InstanceID(), Name: label},
		Message: "Service account assignment check cannot run on instance " + label,
	})
}

func TestSACheckDiag_EndUserTextUnchanged(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	rec := createWithSA(t, srv, projectID, "diag-text", sa)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, saCheckEndUserMsg, saCheckErrorMessage(t, rec))
	assert.NotContains(t, rec.Body.String(), saAssignCheckDiagCause)
	assert.NotContains(t, rec.Body.String(), "hub's identity")
}

func TestSACheckDiag_ClearsOnceCheckRuns(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-before", sa).Code)
	require.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck)

	pt.err = nil
	pt.resp = &policytroubleshooterpb.TroubleshootIamPolicyResponse{
		OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
	}
	rec := createWithSA(t, srv, projectID, "diag-after", sa)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck)
}

func TestSACheckDiag_OtherCheckErrorsDoNotRecord(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.Unavailable, "temporarily unavailable")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	rec := createWithSA(t, srv, projectID, "diag-unavailable", sa)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, saCheckEndUserMsg, saCheckErrorMessage(t, rec))
	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck)
}

func TestSACheckDiag_NotVisibleToNonAdmin(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)
	ctx := context.Background()
	seedRoleDefinitions(ctx, srv.store)
	member := &store.User{
		ID: tid("diag-member"), Email: "diag-member@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	require.NoError(t, srv.store.CreateUser(ctx, member))

	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-member-view", sa).Code)
	require.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck)

	handler := srv.routeGuard(routeMetadataTable["/api/v1/admin/health/summary"], srv.handleHealthSummary)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	req = req.WithContext(contextWithIdentity(ctx,
		NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, "member", "api")))
	rr := httptest.NewRecorder()
	handler(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	body := rr.Body.String()
	assert.False(t, strings.Contains(body, saAssignCheckDiagCause) || strings.Contains(body, "hub's identity"),
		"non-admin response must not carry the diagnostic: %s", body)
}

func TestSACheckDiag_HiddenWhenCheckNotEnforced(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-mode", sa).Code)
	require.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck)

	srv.mu.Lock()
	srv.saAssignCheckMode = SAAssignCheckOff
	srv.mu.Unlock()
	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck)
}

func TestSACheckDiag_CachedResultNeitherClearsNorRecords(t *testing.T) {
	pt := &fakePTClient{resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
		OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CAN_ACCESS,
	}}
	srv, sa, projectID, counting := saCheckDiagServerCounting(t, pt)
	other := wiringSA(t, srv.store, store.ScopeProject, projectID, "diag-other@p.iam.gserviceaccount.com")

	require.Equal(t, http.StatusCreated, createWithSA(t, srv, projectID, "diag-cache-1", sa).Code)
	require.Equal(t, 1, counting.calls)

	// The API now refuses the hub, but the allow for sa is cached: no call,
	// so nothing is recorded.
	pt.resp = nil
	pt.err = status.Error(codes.PermissionDenied, "caller lacks access")
	require.Equal(t, http.StatusCreated, createWithSA(t, srv, projectID, "diag-cache-2", sa).Code)
	require.Equal(t, 1, counting.calls, "expected a cache hit")
	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck, "a cache hit must not record")

	// A real call for another account records the diagnostic.
	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-cache-3", other).Code)
	require.Equal(t, 2, counting.calls)
	require.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck)

	// Another cache hit for sa must leave it in place.
	require.Equal(t, http.StatusCreated, createWithSA(t, srv, projectID, "diag-cache-4", sa).Code)
	require.Equal(t, 2, counting.calls, "expected a cache hit")
	assert.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck, "a cache hit must not clear")
}

func TestSACheckDiag_APIDisabledDoesNotRecord(t *testing.T) {
	st, err := status.New(codes.PermissionDenied, "API has not been used or is disabled").
		WithDetails(&errdetails.ErrorInfo{Reason: "SERVICE_DISABLED", Domain: "googleapis.com"})
	require.NoError(t, err)
	pt := &fakePTClient{err: st.Err()}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	rec := createWithSA(t, srv, projectID, "diag-disabled", sa)
	require.Equal(t, http.StatusForbidden, rec.Code, "assignment is still denied")
	assert.Equal(t, saCheckEndUserMsg, saCheckErrorMessage(t, rec))
	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck)
}

func TestSACheckDiag_ClearedWhenModeLeavesEnforce(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)

	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-reenable", sa).Code)
	require.NotNil(t, adminHealthSummary(t, srv).ServiceAccountCheck)

	srv.mu.Lock()
	srv.applyGCPIAMSettingsLocked(gcpIAMSettings{CheckMode: SAAssignCheckOff})
	srv.mu.Unlock()
	srv.mu.Lock()
	srv.applyGCPIAMSettingsLocked(gcpIAMSettings{CheckMode: SAAssignCheckEnforce})
	srv.mu.Unlock()

	assert.Nil(t, adminHealthSummary(t, srv).ServiceAccountCheck,
		"an old record must not reappear when enforcement is turned back on")
}

func TestSACheckDiag_NotRecordedWhenNotEnforced(t *testing.T) {
	pt := &fakePTClient{}
	srv, _, _ := saCheckDiagServer(t, pt)
	srv.mu.Lock()
	srv.saAssignCheckMode = SAAssignCheckOff
	srv.mu.Unlock()

	srv.NoteSAAssignCheckCall(status.Error(codes.PermissionDenied, "caller lacks access"))
	assert.Nil(t, srv.saAssignCheckDiag.Load())
}

// /healthz does not carry the diagnostic: it is a check of this process
// for the registry row only.
func TestSACheckDiag_NotInHealthz(t *testing.T) {
	pt := &fakePTClient{err: status.Error(codes.PermissionDenied, "caller lacks access")}
	srv, sa, projectID := saCheckDiagServer(t, pt)
	require.Equal(t, http.StatusForbidden, createWithSA(t, srv, projectID, "diag-healthz", sa).Code)
	require.True(t, srv.saAssignCheckCannotRun())
	_, ok := srv.GetHealthInfo(context.Background()).Checks[saAssignCheckName]
	assert.False(t, ok)
}
