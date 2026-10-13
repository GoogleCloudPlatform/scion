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
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newArtifactSiteFixture(t *testing.T) *artifactSiteFixture {
	t.Helper()
	srv, s, project, sender, target, _, dispatcher, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	enableOffload(t, srv, 0, true) // envelope switch on, offload off
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	return &artifactSiteFixture{
		srv: srv, s: s, st: st, project: project, owner: owner, sender: sender, target: target, dispatcher: dispatcher,
		userOwned:  seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindUser, owner.ID, "Owner notes"),
		agentOwned: seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Agent report"),
		unreadable: seedMessageArtifact(t, st, tid("msgart-site-other"), artifacts.PrincipalKindUser, tid("msgart-site-stranger"), "Secret title"),
	}
}

func newChatV2WebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	if srv.webChatStore != nil {
		return srv.webChatStore
	}
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// seedMessageArtifact writes a ready one-file artifact homed in scopeRef
// with the home-scope read grant, and returns its id.
func seedMessageArtifact(t *testing.T, st artifacts.Store, scopeRef, ownerKind, ownerRef, title string) string {
	t.Helper()
	now := time.Now()
	a := &artifacts.Artifact{ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: scopeRef,
		OwnerKind: ownerKind, OwnerRef: ownerRef, Title: title, CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "doc.md", TotalBytes: 3, FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	files := []artifacts.File{{VersionID: v.ID, Path: "doc.md", Size: 3, SHA256: strings.Repeat("ab", 32), MediaType: "text/markdown"}}
	grants := []artifacts.Grant{{ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: artifacts.SubjectScope,
		SubjectRef: scopeRef, Permission: artifacts.GrantRead, CreatedAt: now}}
	require.NoError(t, st.CreatePublished(context.Background(), a, v, files, grants))
	return a.ID
}

// tokenBackedSender returns the request identity of agent a as the hub
// builds it from a validated agent token with the baseline role's scopes,
// and records the delegation edge the artifact host's chain check needs.
func tokenBackedSender(t *testing.T, s store.Store, a *store.Agent) *agentIdentityWrapper {
	t.Helper()
	delegator := tid("msgart-delegator-" + a.ID)
	createTestUserWithProjectRole(t, s, delegator, "delegator-"+a.Slug+"@test.com", a.ProjectID, store.ProjectRoleOwner)
	addRecordedArtifactEdge(t, s, delegator, a.ID, a.ProjectID)
	ensureEdgeBackfillComplete(t, s)
	id := artifactTestAgent(a.ID, a.ProjectID, ScopesForRole(AgentRoleBaseline)...)
	id.AgentTokenClaims.Ancestry = a.Ancestry
	return id
}

// requestAuthCtx is ctx as the authentication middleware leaves it for a
// request authenticated as identity: the identity plus its credential
// context.
func requestAuthCtx(ctx context.Context, identity Identity) context.Context {
	return contextWithCredentialContext(contextWithIdentity(ctx, identity), credentialContextForIdentity(identity))
}

func refsValue(refs ...artifacts.MessageRef) string { return artifacts.EncodeMessageRefs(refs) }

// failListRefsArtifactStore is an artifact store whose recorded message
// reference reads fail.
type failListRefsArtifactStore struct{ artifacts.Store }

// wakeLifecycleUser creates a hub member bound in projectID to a custom role
// holding exactly permissions.
func wakeLifecycleUser(t *testing.T, s store.Store, id, projectID string, permissions ...string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := hubMemberUser(t, s, id)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-role-" + id,
		Description: "Test role for message wake lifecycle tests",
		ScopeType:   store.RoleScopeProject,
		Permissions: permissions,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

// wakeDMSenderIdentity returns an agent identity for sender carrying exactly
// scopes.
func wakeDMSenderIdentity(sender *store.Agent, scopes ...AgentTokenScope) *wakeLifecycleAgentIdentity {
	return &wakeLifecycleAgentIdentity{
		wakeDMTestIdentity: wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		scopes:             scopes,
	}
}

func (failListRefsArtifactStore) ListMessageRefs(context.Context, []string) (map[string][]artifacts.MessageRef, error) {
	return nil, errors.New("message refs unavailable")
}

// artifactSiteFixture is a parity hub with artifacts on, the hub-rendered
// envelope on, and three artifacts: one owned by the project owner (a
// user), one owned by the sending agent, and one in another project that
// neither can read.
type artifactSiteFixture struct {
	srv        *Server
	s          store.Store
	st         artifacts.Store
	project    *store.Project
	owner      *store.User
	sender     *store.Agent
	target     *store.Agent
	dispatcher *recordingDispatcher
	userOwned  string
	agentOwned string
	unreadable string
}

// wakeLifecycleAgentIdentity is an agent caller whose token carries exactly
// scopes.
type wakeLifecycleAgentIdentity struct {
	wakeDMTestIdentity
	scopes []AgentTokenScope
}

func (f *artifactSiteFixture) ownerIdentity() *AuthenticatedUser {
	return NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, "member", "web")
}

func (f *artifactSiteFixture) recorded(t *testing.T, msgID string) []artifacts.MessageRef {
	t.Helper()
	got, err := f.st.ListMessageRefs(context.Background(), []string{msgID})
	require.NoError(t, err)
	return got[msgID]
}

// authzClassification opts this fake into agent JWT classification, so
// authorization decisions treat it as an agent caller with its scopes.
func (i *wakeLifecycleAgentIdentity) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindAgent, CredentialKindAgentJWT
}

func (i *wakeLifecycleAgentIdentity) Scopes() []AgentTokenScope { return i.scopes }
func (i *wakeLifecycleAgentIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range i.scopes {
		if s == scope {
			return true
		}
	}
	return false
}
