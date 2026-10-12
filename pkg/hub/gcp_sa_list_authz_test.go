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

// Authorization of the GCP service-account list endpoints (ptone/scion#4019).
//
// The rule is spec §9 Q1: any project member, agents included, may see an
// account's email; a non-member may not. Both project lists (the nested route
// and the top-level route with scope=project) require project read; the
// hub-scope list requires hub membership.

// saListAuthzFixture is setupGCPAuthzTest's world plus an agent in the
// project, a second project with its own agent, and a user who is not a hub
// member.
type saListAuthzFixture struct {
	srv          *Server
	owner        *store.User
	member       *store.User
	outsider     *store.User // hub member, not a project member
	nonHubMember *store.User
	project      *store.Project
	otherProject *store.Project
	agent        *store.Agent // in project
	otherAgent   *store.Agent // in otherProject
	projectSA    *store.GCPServiceAccount
	hubSA        *store.GCPServiceAccount
}

func newSAListAuthzFixture(t *testing.T) *saListAuthzFixture {
	t.Helper()
	srv, s, owner, member, outsider, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	f := &saListAuthzFixture{srv: srv, owner: owner, member: member, outsider: outsider, project: project}

	f.nonHubMember = &store.User{
		ID: tid("user-gcp-non-hub-member"), Email: "non-hub-member@example.com", DisplayName: "Not A Hub Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.nonHubMember))

	// A project member is a user with a project role, as adding a member
	// through the members API creates.
	grantProjectRole(t, s, member.ID, project.ID, "project-member")

	f.otherProject = &store.Project{
		ID: tid("project-gcp-list-other"), Name: "GCP List Other", Slug: "gcp-list-other",
		OwnerID: outsider.ID, CreatedBy: outsider.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.otherProject))
	srv.seedProjectCreatorMembership(ctx, f.otherProject)

	newAgent := func(id, slug, projectID string) *store.Agent {
		a := &store.Agent{
			ID: tid(id), Slug: slug, Name: slug, ProjectID: projectID,
			Phase: "running", Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	f.agent = newAgent("agent-gcp-list", "gcp-list-agent", project.ID)
	f.otherAgent = newAgent("agent-gcp-list-other", "gcp-list-other-agent", f.otherProject.ID)

	f.projectSA, f.hubSA = seedListMixSAs(t, ctx, s, owner, project)
	return f
}

// seedListMixSAs seeds one project-scoped and one hub-scoped account.
func seedListMixSAs(t *testing.T, ctx context.Context, s store.Store, owner *store.User, project *store.Project) (*store.GCPServiceAccount, *store.GCPServiceAccount) {
	t.Helper()
	projectSA := &store.GCPServiceAccount{
		ID: tid("list-authz-project-sa"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "list-project@example.iam.gserviceaccount.com", ProjectID: "example",
		CreatedBy: owner.ID, CreatedAt: time.Now(),
	}
	hubSA := &store.GCPServiceAccount{
		ID: tid("list-authz-hub-sa"), Scope: store.ScopeHub, ScopeID: "hub",
		Email: "list-hub@example.iam.gserviceaccount.com", ProjectID: "example",
		CreatedBy: owner.ID, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projectSA))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))
	return projectSA, hubSA
}

func (f *saListAuthzFixture) projectListPaths() []string {
	return []string{
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts?includeHubScoped=true", f.project.ID),
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s&includeHubScoped=true", f.project.ID),
	}
}

func (f *saListAuthzFixture) asAgent(t *testing.T, agent *store.Agent, path string) (int, string) {
	t.Helper()
	rec := doAgentReadRequest(t, f.srv, agent.ID, agent.ProjectID, path, ScopesForRole(AgentRoleBaseline))
	return rec.Code, rec.Body.String()
}

func listedEmails(t *testing.T, body string) []string {
	t.Helper()
	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	emails := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		emails = append(emails, item.Email)
	}
	return emails
}

func TestGCPSAList_ProjectList_MembersAllowed(t *testing.T) {
	f := newSAListAuthzFixture(t)
	for _, path := range f.projectListPaths() {
		for _, u := range []*store.User{f.owner, f.member} {
			t.Run(u.DisplayName+" "+path, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, u, http.MethodGet, path, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.ElementsMatch(t, []string{f.projectSA.Email, f.hubSA.Email}, listedEmails(t, rec.Body.String()))
			})
		}
	}
}

func TestGCPSAList_ProjectList_NonMemberDenied(t *testing.T) {
	f := newSAListAuthzFixture(t)
	for _, path := range f.projectListPaths() {
		for _, u := range []*store.User{f.outsider, f.nonHubMember} {
			t.Run(u.DisplayName+" "+path, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, u, http.MethodGet, path, nil)
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), f.projectSA.Email, "a non-member must not see an account email")
				assert.NotContains(t, rec.Body.String(), f.hubSA.Email, "a non-member must not see an account email")
			})
		}
	}
}

func TestGCPSAList_ProjectList_AgentOfSameProjectAllowed(t *testing.T) {
	f := newSAListAuthzFixture(t)
	for _, path := range f.projectListPaths() {
		t.Run(path, func(t *testing.T) {
			code, body := f.asAgent(t, f.agent, path)
			require.Equal(t, http.StatusOK, code, body)
			assert.ElementsMatch(t, []string{f.projectSA.Email, f.hubSA.Email}, listedEmails(t, body))
		})
	}
}

func TestGCPSAList_ProjectList_AgentOfOtherProjectDenied(t *testing.T) {
	f := newSAListAuthzFixture(t)
	for _, path := range f.projectListPaths() {
		t.Run(path, func(t *testing.T) {
			code, body := f.asAgent(t, f.otherAgent, path)
			require.Equal(t, http.StatusForbidden, code, body)
			assert.NotContains(t, body, f.projectSA.Email)
			assert.NotContains(t, body, f.hubSA.Email)
		})
	}
}

// An unknown project stays a 404 on both routes, as before the check.
func TestGCPSAList_ProjectList_UnknownProjectNotFound(t *testing.T) {
	f := newSAListAuthzFixture(t)
	for _, path := range []string{
		"/api/v1/projects/" + tid("no-such-project") + "/gcp-service-accounts",
		"/api/v1/gcp-service-accounts?scope=project&scopeId=" + tid("no-such-project"),
	} {
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

func TestGCPSAList_HubList_HubMemberAllowed(t *testing.T) {
	f := newSAListAuthzFixture(t)
	// The outsider belongs to no project here; hub membership alone admits.
	for _, u := range []*store.User{f.member, f.outsider} {
		rec := doRequestAsUser(t, f.srv, u, http.MethodGet, "/api/v1/gcp-service-accounts?scope=hub", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.ElementsMatch(t, []string{f.hubSA.Email}, listedEmails(t, rec.Body.String()))
	}
}

func TestGCPSAList_HubList_NonHubMemberDenied(t *testing.T) {
	f := newSAListAuthzFixture(t)
	rec := doRequestAsUser(t, f.srv, f.nonHubMember, http.MethodGet, "/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), f.hubSA.Email)
}

// An agent is not a hub member. It reaches the hub-scoped accounts it may use
// through its own project's list with includeHubScoped, shown above.
func TestGCPSAList_HubList_AgentDenied(t *testing.T) {
	f := newSAListAuthzFixture(t)
	code, body := f.asAgent(t, f.agent, "/api/v1/gcp-service-accounts?scope=hub")
	require.Equal(t, http.StatusForbidden, code, body)
	assert.NotContains(t, body, f.hubSA.Email)
}
