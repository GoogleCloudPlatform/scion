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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3409: an agent may read the status of an agent it
// directly launched, in the same project.

// launcherReadAgent stores an agent in the shared agent fixture.
func launcherReadAgent(t *testing.T, f *bypassAgentsFixture, name, projectID string, ancestry []string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:        tid(name),
		Slug:      tid(name),
		Name:      name,
		ProjectID: projectID,
		Phase:     string(state.PhaseProvisioning),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
		Ancestry:  ancestry,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// setLauncherReadPhase sets the stored phase of an agent.
func setLauncherReadPhase(t *testing.T, f *bypassAgentsFixture, id string, phase state.Phase) {
	t.Helper()
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, id)
	require.NoError(t, err)
	a.Phase = string(phase)
	require.NoError(t, f.store.UpdateAgent(ctx, a))
}

// launcherReadPaths returns both single-agent GET routes for a.
func launcherReadPaths(projectID, agentRef string) map[string]string {
	return map[string]string{
		"project route": "/api/v1/projects/" + projectID + "/agents/" + agentRef,
		"global route":  "/api/v1/agents/" + agentRef,
	}
}

func decodeLauncherReadAgent(t *testing.T, rec *httptest.ResponseRecorder) AgentWithCapabilities {
	t.Helper()
	var got AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	return got
}

func TestLauncherRead_LauncherReadsChildThroughToRunning(t *testing.T) {
	f := bypassAgentsSetup(t)
	bindFixtureOwner(t, f)
	child := launcherReadAgent(t, f, "launcher-child", f.proj.ID, []string{f.owner.ID, f.caller.ID})

	for name, path := range launcherReadPaths(f.proj.ID, child.Slug) {
		t.Run(name, func(t *testing.T) {
			setLauncherReadPhase(t, f, child.ID, state.PhaseProvisioning)
			rec := f.asAgent(t, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, string(state.PhaseProvisioning), decodeLauncherReadAgent(t, rec).Phase)

			setLauncherReadPhase(t, f, child.ID, state.PhaseRunning)
			rec = f.asAgent(t, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, string(state.PhaseRunning), decodeLauncherReadAgent(t, rec).Phase)
		})
	}
}

// TestLauncherRead_DeniedTargetsMatchMissing checks that every target the
// caller did not directly launch in its project gets exactly the answer a
// missing agent gets.
func TestLauncherRead_DeniedTargetsMatchMissing(t *testing.T) {
	f := bypassAgentsSetup(t)
	bindFixtureOwner(t, f)
	child := launcherReadAgent(t, f, "launcher-child-2", f.proj.ID, []string{f.owner.ID, f.caller.ID})
	grandchild := launcherReadAgent(t, f, "launcher-grandchild", f.proj.ID, []string{f.owner.ID, f.caller.ID, child.ID})
	otherProjectChild := launcherReadAgent(t, f, "launcher-other-child", f.other.ID, []string{f.owner.ID, f.caller.ID})

	for name, path := range launcherReadPaths(f.proj.ID, tid("launcher-missing")) {
		t.Run(name, func(t *testing.T) {
			missing := f.asAgent(t, http.MethodGet, path, nil)
			require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())

			targets := map[string]*store.Agent{
				"unrelated agent in the same project": f.sibling,
				"grandchild":                          grandchild,
				"child in another project":            otherProjectChild,
			}
			for targetName, target := range targets {
				projectID := target.ProjectID
				p := launcherReadPaths(projectID, target.ID)[name]
				rec := f.asAgent(t, http.MethodGet, p, nil)
				assert.Equal(t, missing.Code, rec.Code, "%s: %s", targetName, rec.Body.String())
				assert.Equal(t, missing.Body.String(), rec.Body.String(), targetName)
			}
		})
	}
}

// TestLauncherRead_UserCallerUnchanged checks that a user caller's denial
// on the single-agent GET routes is still a 403.
func TestLauncherRead_UserCallerUnchanged(t *testing.T) {
	f := bypassAgentsSetup(t)
	stranger := hubMemberUser(t, f.store, tid("launcher-user-stranger"))
	rec := requestAsIdentity(t, f.srv, authUser(stranger), http.MethodGet, "/api/v1/agents/"+f.child.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestLauncherRead_OnlySingleAgentReadWidened compares every agent
// permission for a launcher with and without the single-agent GET
// resource: only agent.read changes, and only on that resource.
func TestLauncherRead_OnlySingleAgentReadWidened(t *testing.T) {
	authz, _ := authzTestSetup(t)
	projectID := tid("launcher-only-project")
	launcherID := tid("launcher-only-agent")
	launcher := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: launcherID},
		ProjectID: projectID,
		Scopes:    allRegisteredAgentScopes(),
	}}
	child := &store.Agent{
		ID:        tid("launcher-only-child"),
		ProjectID: projectID,
		Ancestry:  []string{tid("launcher-only-root"), launcherID},
	}

	for _, p := range sameTypeRegistryPermissions("agent") {
		t.Run(p.ID, func(t *testing.T) {
			plain := decideExplicit(t, authz, launcher, agentResource(child), p)
			single := decideExplicit(t, authz, launcher, agentStatusReadResource(child), p)
			if p.ID == "agent.read" {
				assert.False(t, plain.Allowed, "agent.read without the single-agent GET resource: %q", plain.Reason)
				assert.True(t, single.Allowed, "agent.read on the single-agent GET resource: %q", single.Reason)
				assert.Equal(t, "relationship grant: launcher status read", single.Reason)
				return
			}
			assert.Equal(t, plain.Allowed, single.Allowed, "%s: plain %q, single %q", p.ID, plain.Reason, single.Reason)
		})
	}

	for _, p := range permissions.Registry {
		assert.Equal(t, p.ID == "agent.read", permissions.RelationshipPolicyAllows("launcher", "agent", "agent", p.ID), p.ID)
	}
}

// TestLauncherRead_DecidesFromStoredTarget checks that the rule reads the
// launcher from the stored target record and the project from the caller,
// and that the caller's own ancestry claim plays no part.
func TestLauncherRead_DecidesFromStoredTarget(t *testing.T) {
	projectID := tid("launcher-stored-project")
	callerID := tid("launcher-stored-caller")
	rootID := tid("launcher-stored-root")
	read := permissions.Permission{ID: "agent.read", Action: "read"}
	authz, _ := authzTestSetup(t)

	caller := func(ancestry []string, project string) Identity {
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: callerID},
			ProjectID: project,
			Scopes:    []AgentTokenScope{ScopeProjectRead},
			Ancestry:  ancestry,
		}}
	}
	target := func(ancestry []string) *store.Agent {
		return &store.Agent{ID: tid("launcher-stored-target"), ProjectID: projectID, Ancestry: ancestry}
	}

	cases := []struct {
		name   string
		caller Identity
		target *store.Agent
		want   bool
	}{
		{"stored launcher matches", caller(nil, projectID), target([]string{rootID, callerID}), true},
		{"caller claims to be the launcher", caller([]string{rootID, callerID}, projectID), target([]string{rootID}), false},
		{"caller is an earlier ancestor", caller(nil, projectID), target([]string{rootID, callerID, tid("launcher-stored-mid")}), false},
		{"caller in another project", caller(nil, tid("launcher-stored-other")), target([]string{rootID, callerID}), false},
		{"target has no ancestry", caller(nil, projectID), target(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideExplicit(t, authz, tc.caller, agentStatusReadResource(tc.target), read)
			assert.Equal(t, tc.want, d.Allowed, d.Reason)
		})
	}
}

func TestLaunchRelationship_NamesLauncherKind(t *testing.T) {
	assert.Equal(t, "direct_launcher_agent", launchRelationship(&store.Agent{CreatedBy: "a1", Ancestry: []string{"u1", "a1"}}))
	assert.Equal(t, "direct_launcher_user", launchRelationship(&store.Agent{CreatedBy: "u1", Ancestry: []string{"u1"}}))
}
