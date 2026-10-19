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
	"errors"
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
	"google.golang.org/protobuf/protoadapt"

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

// saCheckIAMReadDeniedMsg is the end-user text when the hub's identity was
// refused reading IAM policy in gcpProject. Spelled out here, not taken
// from the gate, so a change to the gate's text fails these tests.
func saCheckIAMReadDeniedMsg(gcpProject string) string {
	const hub = "hub@test.iam.gserviceaccount.com"
	return "Could not verify your permission to use this GCP service account: " +
		"the Hub could not read IAM policy in GCP project " + gcpProject + ". " +
		"The Hub's service account " + hub + " needs the IAM Security Reviewer " +
		"role (roles/iam.securityReviewer) on that project. A GCP project admin can " +
		"grant it with: gcloud projects add-iam-policy-binding " + gcpProject +
		` --member="serviceAccount:` + hub + `" --role="roles/iam.securityReviewer"`
}

func saCheckStatusErr(t *testing.T, code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	t.Helper()
	st, err := status.New(code, msg).WithDetails(details...)
	require.NoError(t, err)
	return st.Err()
}

func TestSACheckDiag_EndUserTextByErrorClass(t *testing.T) {
	const upstream = "upstream-detail-7f3a"
	cases := []struct {
		name     string
		err      error
		detailed bool
	}{
		{"permission denied", status.Error(codes.PermissionDenied, upstream), true},
		{"permission denied with IAM reason", saCheckStatusErr(t, codes.PermissionDenied, upstream,
			&errdetails.ErrorInfo{Reason: "IAM_PERMISSION_DENIED", Domain: "iam.googleapis.com"}), true},
		{"api disabled", saCheckStatusErr(t, codes.PermissionDenied, upstream,
			&errdetails.ErrorInfo{Reason: "SERVICE_DISABLED", Domain: "googleapis.com"}), false},
		{"vpc service controls precondition", saCheckStatusErr(t, codes.PermissionDenied, upstream,
			&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
				{Type: "VPC_SERVICE_CONTROLS"}}}), false},
		{"vpc service controls reason", saCheckStatusErr(t, codes.PermissionDenied, upstream,
			&errdetails.ErrorInfo{Reason: "SECURITY_POLICY_VIOLATED", Domain: "googleapis.com"}), false},
		{"vpc service controls message", status.Error(codes.PermissionDenied,
			"Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: x"), false},
		{"other error info reason", saCheckStatusErr(t, codes.PermissionDenied, upstream,
			&errdetails.ErrorInfo{Reason: "SOMETHING_ELSE", Domain: "googleapis.com"}), false},
		{"unavailable", status.Error(codes.Unavailable, upstream), false},
		{"not found", status.Error(codes.NotFound, upstream), false},
		{"unauthenticated", status.Error(codes.Unauthenticated, upstream), false},
		{"internal", status.Error(codes.Internal, upstream), false},
		{"non-grpc error", errors.New(upstream), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, sa, projectID := saCheckDiagServer(t, &fakePTClient{err: tc.err})
			rec := createWithSA(t, srv, projectID, "diag-text", sa)
			require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			want := saCheckEndUserMsg
			if tc.detailed {
				want = saCheckIAMReadDeniedMsg(sa.ProjectID)
			}
			assert.Equal(t, want, saCheckErrorMessage(t, rec))
			body := rec.Body.String()
			assert.NotContains(t, body, upstream, "upstream error text must not reach the caller")
			assert.NotContains(t, body, saAssignCheckDiagCause)
			assert.NotContains(t, body, sa.Email)
		})
	}
}

// existencePTClient stands in for Policy Troubleshooter when the hub's
// identity has no IAM read access: every call is refused with
// PermissionDenied, with upstream wording that differs by whether the
// target account exists.
type existencePTClient struct {
	existing map[string]bool
}

func (c *existencePTClient) TroubleshootIamPolicy(
	_ context.Context,
	req *policytroubleshooterpb.TroubleshootIamPolicyRequest,
	_ ...gax.CallOption,
) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error) {
	if c.existing[req.GetAccessTuple().GetFullResourceName()] {
		return nil, status.Error(codes.PermissionDenied, "permission denied on existing resource")
	}
	return nil, status.Error(codes.PermissionDenied, "permission denied (or the resource may not exist)")
}

func TestSACheckDiag_EndUserTextSameWhetherTargetExists(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	pt := &existencePTClient{existing: map[string]bool{}}
	enforceSAAssign(srv, NewPolicyTroubleshooterChecker(pt, "hub@test.iam.gserviceaccount.com", false))

	newSA := func(email, gcpProject string, exists bool) *store.GCPServiceAccount {
		sa := wiringSA(t, s, store.ScopeProject, project.ID, email)
		sa.ProjectID = gcpProject
		require.NoError(t, s.UpdateGCPServiceAccount(context.Background(), sa))
		if exists {
			pt.existing["//iam.googleapis.com/projects/"+gcpProject+"/serviceAccounts/"+email] = true
		}
		return sa
	}
	message := func(name string, sa *store.GCPServiceAccount) string {
		rec := createWithSA(t, srv, project.ID, name, sa)
		require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		return saCheckErrorMessage(t, rec)
	}

	existing := newSA("exists@other-proj.iam.gserviceaccount.com", "other-proj", true)
	missing := newSA("missing@other-proj.iam.gserviceaccount.com", "other-proj", false)
	gotExisting := message("diag-exists", existing)
	gotMissing := message("diag-missing", missing)
	assert.Equal(t, saCheckIAMReadDeniedMsg("other-proj"), gotExisting)
	assert.Equal(t, gotExisting, gotMissing, "same text for an existing and a missing account")

	// A target in a GCP project that does not exist differs only by the
	// project ID the caller already registered.
	noProject := newSA("any@no-such-proj.iam.gserviceaccount.com", "no-such-proj", false)
	gotNoProject := message("diag-no-project", noProject)
	assert.Equal(t, gotExisting, strings.ReplaceAll(gotNoProject, "no-such-proj", "other-proj"))
}

func TestSAAssignIAMReadDeniedMessage_RefusesUnusableInputs(t *testing.T) {
	assert.Equal(t, saCheckIAMReadDeniedMsg("p-1"),
		saAssignIAMReadDeniedMessage("p-1", "hub@test.iam.gserviceaccount.com"))
	assert.Equal(t, saCheckIAMReadDeniedMsg("example.com:p-1"),
		saAssignIAMReadDeniedMessage("example.com:p-1", "hub@test.iam.gserviceaccount.com"))
	for _, in := range [][2]string{
		{"", "hub@test.iam.gserviceaccount.com"},
		{"p-1", ""},
		{"p 1", "hub@test.iam.gserviceaccount.com"},
		{"p-1;x", "hub@test.iam.gserviceaccount.com"},
		{"P-1", "hub@test.iam.gserviceaccount.com"},
		{"p@1", "hub@test.iam.gserviceaccount.com"},
		{"p-1", "hub@test.iam.gserviceaccount.com --role=x"},
		{"p-1", "hub$(x)@test.iam.gserviceaccount.com"},
		{"--quiet", "hub@test.iam.gserviceaccount.com"},
		{"-x", "hub@test.iam.gserviceaccount.com"},
		{"a:-b", "hub@test.iam.gserviceaccount.com"},
		{"a:", "hub@test.iam.gserviceaccount.com"},
		{":a", "hub@test.iam.gserviceaccount.com"},
		{"1abc", "hub@test.iam.gserviceaccount.com"},
		{"p-1", "a@b@c"},
		{"p-1", "-hub@test.iam.gserviceaccount.com"},
		{"p-1", "hub@-test.iam.gserviceaccount.com"},
		{"p-1", "@test.iam.gserviceaccount.com"},
		{"p-1", "hub@"},
		{"p-1", "hub.test.iam.gserviceaccount.com"},
	} {
		assert.Empty(t, saAssignIAMReadDeniedMessage(in[0], in[1]), "inputs %q", in)
	}
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
