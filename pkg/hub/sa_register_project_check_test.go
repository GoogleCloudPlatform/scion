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
	"fmt"
	"net/http"
	"strings"
	"testing"

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// regHubProject is the hub's own GCP project in these tests.
const regHubProject = "hub-proj"

// regUpstream is upstream error text that must never reach a response.
const regUpstream = "upstream-detail-5397"

// regProbe is a scriptable SAProjectAccessProbe that records its calls.
type regProbe struct {
	err      error
	projects []string
	emails   []string
}

func (p *regProbe) ProbeProjectAccess(_ context.Context, gcpProjectID, hubSAEmail string) error {
	p.projects = append(p.projects, gcpProjectID)
	p.emails = append(p.emails, hubSAEmail)
	return p.err
}

// regServer returns a server enforcing the assignment check, with the
// mock token generator (hub@test.iam.gserviceaccount.com) and probe
// installed for hub project regHubProject, plus a project to register in.
func regServer(t *testing.T, probe SAProjectAccessProbe) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker())
	srv.SetSAProjectAccessProbe(probe, regHubProject)
	return srv, s, projectID
}

// regRegister registers email in projectID and decodes the 201 response.
func regRegister(t *testing.T, srv *Server, projectID string, body map[string]string) (createGCPServiceAccountResponse, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID), body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp createGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp, rec.Body.String()
}

// regVerify verifies saID in projectID and decodes the 200 response.
func regVerify(t *testing.T, srv *Server, projectID, saID string) (gcpServiceAccountWithWarnings, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s/verify", projectID, saID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp gcpServiceAccountWithWarnings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp, rec.Body.String()
}

// regIAMWarnings returns the warnings that are the IAM-read text, nil when
// there are none, so unrelated warnings do not affect these tests.
func regIAMWarnings(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, "could not read IAM policy") {
			out = append(out, w)
		}
	}
	return out
}

func TestSARegisterProjectCheck_OtherProjectRefusedWarnsOnRegister(t *testing.T) {
	probe := &regProbe{err: status.Error(codes.PermissionDenied, regUpstream)}
	srv, _, projectID := regServer(t, probe)

	resp, body := regRegister(t, srv, projectID,
		map[string]string{"email": "reg@other-proj.iam.gserviceaccount.com"})
	require.Equal(t, []string{saCheckIAMReadDeniedMsg("other-proj")}, regIAMWarnings(resp.Warnings))

	// A warning only: the account is registered and verified as before.
	assert.True(t, resp.Verified)
	assert.False(t, resp.VerificationFailed)
	assert.Equal(t, []string{"other-proj"}, probe.projects)
	assert.Equal(t, []string{"hub@test.iam.gserviceaccount.com"}, probe.emails)
	assert.NotContains(t, body, regUpstream)
	assert.NotContains(t, body, regHubProject)
}

func TestSARegisterProjectCheck_HubScopedRegisterWarns(t *testing.T) {
	probe := &regProbe{err: status.Error(codes.PermissionDenied, regUpstream)}
	srv, s, _ := regServer(t, probe)
	ensureHubMembership(context.Background(), s, DevUserID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/gcp-service-accounts?scope=hub",
		map[string]string{"email": "hubreg@other-proj.iam.gserviceaccount.com"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp createGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{saCheckIAMReadDeniedMsg("other-proj")}, regIAMWarnings(resp.Warnings))

	assert.True(t, resp.Verified)
	assert.NotContains(t, rec.Body.String(), regUpstream)
}

func TestSARegisterProjectCheck_VerifyWarns(t *testing.T) {
	probe := &regProbe{}
	srv, _, projectID := regServer(t, probe)
	created, _ := regRegister(t, srv, projectID,
		map[string]string{"email": "ver@other-proj.iam.gserviceaccount.com"})
	assert.Empty(t, regIAMWarnings(created.Warnings), "no warning while the probe succeeds")

	probe.err = status.Error(codes.PermissionDenied, regUpstream)
	resp, body := regVerify(t, srv, projectID, created.ID)
	require.Equal(t, []string{saCheckIAMReadDeniedMsg("other-proj")}, regIAMWarnings(resp.Warnings))

	assert.True(t, resp.Verified)
	assert.NotContains(t, body, regUpstream)
}

func TestSARegisterProjectCheck_WarningByErrorClass(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		detailed bool
	}{
		{"permission denied", status.Error(codes.PermissionDenied, regUpstream), true},
		{"permission denied with IAM reason", saCheckStatusErr(t, codes.PermissionDenied, regUpstream,
			&errdetails.ErrorInfo{Reason: "IAM_PERMISSION_DENIED", Domain: "iam.googleapis.com"}), true},
		{"api disabled", saCheckStatusErr(t, codes.PermissionDenied, regUpstream,
			&errdetails.ErrorInfo{Reason: "SERVICE_DISABLED", Domain: "googleapis.com"}), false},
		{"vpc service controls precondition", saCheckStatusErr(t, codes.PermissionDenied, regUpstream,
			&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
				{Type: "VPC_SERVICE_CONTROLS"}}}), false},
		{"vpc service controls reason", saCheckStatusErr(t, codes.PermissionDenied, regUpstream,
			&errdetails.ErrorInfo{Reason: "SECURITY_POLICY_VIOLATED", Domain: "googleapis.com"}), false},
		{"vpc service controls message", status.Error(codes.PermissionDenied,
			"Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: x"), false},
		{"other error info reason", saCheckStatusErr(t, codes.PermissionDenied, regUpstream,
			&errdetails.ErrorInfo{Reason: "SOMETHING_ELSE", Domain: "googleapis.com"}), false},
		{"unavailable", status.Error(codes.Unavailable, regUpstream), false},
		{"not found", status.Error(codes.NotFound, regUpstream), true},
		{"not found api disabled", saCheckStatusErr(t, codes.NotFound, regUpstream,
			&errdetails.ErrorInfo{Reason: "SERVICE_DISABLED", Domain: "googleapis.com"}), false},
		{"not found vpc service controls precondition", saCheckStatusErr(t, codes.NotFound, regUpstream,
			&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
				{Type: "VPC_SERVICE_CONTROLS"}}}), false},
		{"not found vpc service controls message", status.Error(codes.NotFound,
			"Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: x"), false},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, regUpstream), false},
		{"non-grpc error", errors.New(regUpstream), false},
		{"success", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &regProbe{err: tc.err}
			srv, _, projectID := regServer(t, probe)
			resp, body := regRegister(t, srv, projectID,
				map[string]string{"email": "cls@other-proj.iam.gserviceaccount.com"})
			if tc.detailed {
				require.Equal(t, []string{saCheckIAMReadDeniedMsg("other-proj")}, regIAMWarnings(resp.Warnings))
			} else {
				assert.Empty(t, regIAMWarnings(resp.Warnings))
			}
			assert.Len(t, probe.projects, 1, "the probe runs once")
			assert.NotContains(t, body, regUpstream, "upstream error text must not reach the caller")
			assert.NotContains(t, body, "vpcServiceControlsUniqueIdentifier")
			assert.True(t, resp.Verified, "the warning never changes the registration")
		})
	}
}

// regProjectPTClient stands in for Policy Troubleshooter when the hub's
// identity has no IAM read access, keyed by what the probe asks about: the
// GCP project resource. A project in existing is refused with one upstream
// text; any other project is answered with missing (a PermissionDenied or
// a NotFound with different text). It records every resource asked about.
type regProjectPTClient struct {
	existing map[string]bool
	missing  error
	asked    []string
}

func (c *regProjectPTClient) TroubleshootIamPolicy(
	_ context.Context,
	req *policytroubleshooterpb.TroubleshootIamPolicyRequest,
	_ ...gax.CallOption,
) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error) {
	name := req.GetAccessTuple().GetFullResourceName()
	c.asked = append(c.asked, name)
	if c.existing[name] {
		return nil, status.Error(codes.PermissionDenied, "permission denied on existing project")
	}
	return nil, c.missing
}

func TestSARegisterProjectCheck_SameWhetherTargetExists(t *testing.T) {
	const existingProject = "//cloudresourcemanager.googleapis.com/projects/other-proj"
	for _, tc := range []struct {
		name    string
		missing error
	}{
		{"missing project refused", status.Error(codes.PermissionDenied, "permission denied (or it may not exist)")},
		{"missing project not found", status.Error(codes.NotFound, "project no-such-proj not found")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt := &regProjectPTClient{existing: map[string]bool{existingProject: true}, missing: tc.missing}
			srv, _, projectID := regServer(t, NewPTProjectAccessProbe(pt))

			existing, existingBody := regRegister(t, srv, projectID,
				map[string]string{"email": "exists@other-proj.iam.gserviceaccount.com"})
			missing, _ := regRegister(t, srv, projectID,
				map[string]string{"email": "missing@other-proj.iam.gserviceaccount.com"})
			noProject, noProjectBody := regRegister(t, srv, projectID,
				map[string]string{"email": "any@no-such-proj.iam.gserviceaccount.com"})
			require.Equal(t, []string{saCheckIAMReadDeniedMsg("other-proj")}, existing.Warnings)

			// The fake answered the two projects differently.
			assert.Equal(t, []string{existingProject, existingProject,
				"//cloudresourcemanager.googleapis.com/projects/no-such-proj"}, pt.asked)

			// Any account in the same project gets the same response.
			assert.Equal(t, existing.Warnings, missing.Warnings)
			assert.Equal(t, existing.Verified, missing.Verified)
			assert.Equal(t, existing.VerificationFailed, missing.VerificationFailed)
			assert.Equal(t, existing.VerificationStatus, missing.VerificationStatus)

			// A GCP project that does not exist reads the same as one the
			// hub may not read, apart from the project ID the caller sent.
			require.Len(t, noProject.Warnings, 1)
			assert.Equal(t, existing.Warnings[0], strings.ReplaceAll(noProject.Warnings[0], "no-such-proj", "other-proj"))
			assert.Equal(t, existing.Verified, noProject.Verified)
			assert.Equal(t, existing.VerificationFailed, noProject.VerificationFailed)
			assert.Equal(t, existing.VerificationStatus, noProject.VerificationStatus)
			for _, body := range []string{existingBody, noProjectBody} {
				assert.NotContains(t, body, "existing project")
				assert.NotContains(t, body, "not found")
				assert.NotContains(t, body, "may not exist")
			}
		})
	}
}

func TestSARegisterProjectCheck_HubProjectUnchanged(t *testing.T) {
	probe := &regProbe{err: status.Error(codes.PermissionDenied, regUpstream)}
	srv, _, projectID := regServer(t, probe)

	withProbe, _ := regRegister(t, srv, projectID,
		map[string]string{"email": "a@hub-proj.iam.gserviceaccount.com"})
	verifiedWithProbe, _ := regVerify(t, srv, projectID, withProbe.ID)
	assert.Empty(t, probe.projects, "no probe call for an account in the hub's own project")

	srv.SetSAProjectAccessProbe(nil, "")
	without, _ := regRegister(t, srv, projectID,
		map[string]string{"email": "b@hub-proj.iam.gserviceaccount.com"})
	verifiedWithout, _ := regVerify(t, srv, projectID, without.ID)

	assert.Equal(t, without.Warnings, withProbe.Warnings)
	assert.Equal(t, without.Verified, withProbe.Verified)
	assert.Equal(t, without.VerificationFailed, withProbe.VerificationFailed)
	assert.Equal(t, without.VerificationDetails, withProbe.VerificationDetails)
	assert.Equal(t, without.VerificationStatus, withProbe.VerificationStatus)
	assert.Equal(t, verifiedWithout.Warnings, verifiedWithProbe.Warnings)
	assert.Equal(t, verifiedWithout.Verified, verifiedWithProbe.Verified)
	assert.Empty(t, probe.projects)
}

func TestSARegisterProjectCheck_NotProbedWhenCheckNotEnforced(t *testing.T) {
	probe := &regProbe{err: status.Error(codes.PermissionDenied, regUpstream)}
	srv, _, projectID := regServer(t, probe)
	srv.mu.Lock()
	srv.saAssignCheckMode = SAAssignCheckOff
	srv.mu.Unlock()

	resp, _ := regRegister(t, srv, projectID,
		map[string]string{"email": "off@other-proj.iam.gserviceaccount.com"})
	assert.Empty(t, regIAMWarnings(resp.Warnings))
	assert.Empty(t, probe.projects)
}

func TestSARegisterProjectCheck_UnshowableInputsNotProbed(t *testing.T) {
	probe := &regProbe{err: status.Error(codes.PermissionDenied, regUpstream)}
	srv, _, projectID := regServer(t, probe)

	// A project ID that is not shaped like a GCP project ID.
	resp, body := regRegister(t, srv, projectID, map[string]string{
		"email": "bad@other-proj.iam.gserviceaccount.com", "projectId": "Other_Proj",
	})
	assert.Empty(t, regIAMWarnings(resp.Warnings))
	assert.NotContains(t, body, "gcloud")

	// No usable hub service account email.
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: ""})
	resp, _ = regRegister(t, srv, projectID,
		map[string]string{"email": "noemail@other-proj.iam.gserviceaccount.com"})
	assert.Empty(t, regIAMWarnings(resp.Warnings))
	assert.Empty(t, probe.projects)
}

func TestPTProjectAccessProbe_AsksAboutTheProject(t *testing.T) {
	refused := status.Error(codes.PermissionDenied, regUpstream)
	pt := &fakePTClient{err: refused}
	err := NewPTProjectAccessProbe(pt).ProbeProjectAccess(context.Background(),
		"other-proj", "hub@test.iam.gserviceaccount.com")
	assert.Equal(t, refused, err)
	require.NotNil(t, pt.captured)
	tuple := pt.captured.GetAccessTuple()
	assert.Equal(t, "//cloudresourcemanager.googleapis.com/projects/other-proj", tuple.GetFullResourceName())
	assert.Equal(t, "hub@test.iam.gserviceaccount.com", tuple.GetPrincipal())
	assert.Equal(t, store.PermissionActAs, tuple.GetPermission())

	// The access answer itself is not used: any response is no error.
	pt = &fakePTClient{resp: &policytroubleshooterpb.TroubleshootIamPolicyResponse{
		OverallAccessState: policytroubleshooterpb.TroubleshootIamPolicyResponse_CANNOT_ACCESS,
	}}
	assert.NoError(t, NewPTProjectAccessProbe(pt).ProbeProjectAccess(context.Background(),
		"other-proj", "hub@test.iam.gserviceaccount.com"))
}
