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

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Placement for agent-launched creates (agent_create_placement.go).
//
// Fixture shape: the project has two brokers. The Kubernetes broker
// (f.broker) offers "gke" (kubernetes) and "local" (docker); the docker
// broker offers only "local" and is the project's default broker. The
// creating agent (f.caller) runs on the Kubernetes broker. Without any
// placement tier, a create therefore lands on the docker broker.

type placementFixture struct {
	*bypassAgentsFixture
	k8sBroker    *store.RuntimeBroker
	dockerBroker *store.RuntimeBroker
}

func newPlacementFixture(t *testing.T) *placementFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
	ctx := context.Background()

	k8s, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	// The name and slug differ so a test can tell a name match from a slug
	// match.
	k8s.Name, k8s.Slug = "k8s-broker", "k8s-broker-slug"
	k8s.Profiles = []store.BrokerProfile{
		{Name: "gke", Type: "kubernetes", Available: true},
		{Name: "local", Type: "docker", Available: true},
	}
	k8s.DefaultProfile = "local"
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, k8s))
	// bypassAgentsSetup wrote the provider row under the broker's old name;
	// rewrite it so the provider row carries the new name, as it would for a
	// broker registered under that name.
	pv, err := f.store.GetProjectProvider(ctx, f.proj.ID, k8s.ID)
	require.NoError(t, err)
	pv.BrokerName = k8s.Name
	require.NoError(t, f.store.AddProjectProvider(ctx, pv))

	docker := &store.RuntimeBroker{
		ID:             tid("placement-docker-broker"),
		Name:           "docker-broker",
		Slug:           "docker-broker",
		Status:         store.BrokerStatusOnline,
		AutoProvide:    true,
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}, {Name: "docker-only", Type: "docker", Available: true}},
		DefaultProfile: "local",
		Created:        time.Now(),
		Updated:        time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, docker))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.proj.ID, BrokerID: docker.ID, BrokerName: docker.Name, Status: store.BrokerStatusOnline,
	}))
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	proj.DefaultRuntimeBrokerID = docker.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	f.proj = proj

	pf := &placementFixture{bypassAgentsFixture: f, k8sBroker: k8s, dockerBroker: docker}
	pf.setCreator(t, k8s.ID, "gke")
	return pf
}

// setCreator places the creating agent on brokerID under profile.
func (pf *placementFixture) setCreator(t *testing.T, brokerID, profile string) {
	t.Helper()
	ctx := context.Background()
	caller, err := pf.store.GetAgent(ctx, pf.caller.ID)
	require.NoError(t, err)
	caller.RuntimeBrokerID = brokerID
	caller.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull), Profile: profile}
	require.NoError(t, pf.store.UpdateAgent(ctx, caller))
}

func (pf *placementFixture) setAnnotations(t *testing.T, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	proj, err := pf.store.GetProject(ctx, pf.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	for k, v := range kv {
		proj.Annotations[k] = v
	}
	require.NoError(t, pf.store.UpdateProject(ctx, proj))
}

// enableInheritPlacement turns the inheritance experiment on for srv.
func enableInheritPlacement(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.AgentCreateInheritPlacement {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.True(t, srv.experimentEnabled(experiments.AgentCreateInheritPlacement))
}

// createAsCreator creates an agent as the creating agent and returns the
// persisted record. It fails the test unless the create succeeds.
func (pf *placementFixture) createAsCreator(t *testing.T, req CreateAgentRequest) *store.Agent {
	t.Helper()
	rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig, "the create response must carry the applied config")
	require.NotNil(t, resp.Agent.AppliedConfig.Placement, "the create response must carry the placement sources")
	got, err := pf.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.Placement, "the agent record must carry the placement sources")
	assert.Equal(t, resp.Agent.AppliedConfig.Placement, got.AppliedConfig.Placement)
	return got
}

func assertPlacement(t *testing.T, agent *store.Agent, brokerID, brokerSource, profile, profileSource string) {
	t.Helper()
	assert.Equal(t, brokerID, agent.RuntimeBrokerID, "broker")
	assert.Equal(t, brokerSource, agent.AppliedConfig.Placement.BrokerSource, "broker source")
	if profile != "" {
		assert.Equal(t, profile, agent.AppliedConfig.Profile, "profile")
	}
	assert.Equal(t, profileSource, agent.AppliedConfig.Placement.ProfileSource, "profile source")
}

// assertNoIDs fails if the response body mentions any broker or agent ID.
func (pf *placementFixture) assertNoIDs(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, id := range []string{pf.k8sBroker.ID, pf.dockerBroker.ID, pf.caller.ID} {
		assert.NotContains(t, body, id, "error responses must not carry IDs")
	}
}

// AC1: a Kubernetes creator with no flags places the child on its own broker
// and profile.
func TestAgentCreatePlacement_InheritsKubernetesCreator(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "inherit-child"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "gke", store.PlacementSourceInherited)
}

// A creator with no saved profile runs under its broker's default profile;
// that profile decides whether inheritance applies.
func TestAgentCreatePlacement_InheritsBrokerDefaultProfileWhenKubernetes(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.DefaultProfile = "gke"
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))
	pf.setCreator(t, pf.k8sBroker.ID, "")

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "inherit-default-profile"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "gke", store.PlacementSourceInherited)
}

// No opt-in, no change: with the experiment off, a Kubernetes creator's
// child takes today's default chain.
func TestAgentCreatePlacement_ExperimentOffKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "experiment-off"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "", store.PlacementSourceDefault)
	assert.NotEqual(t, "gke", agent.AppliedConfig.Profile)
}

// A creator on a docker profile is not inherited from.
func TestAgentCreatePlacement_DockerCreatorKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setCreator(t, pf.k8sBroker.ID, "local")

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "docker-creator"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "", store.PlacementSourceDefault)
}

// A creator whose saved profile no longer resolves on its broker cannot be
// confirmed as Kubernetes, so inheritance is not attempted.
func TestAgentCreatePlacement_UnresolvableCreatorProfileKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setCreator(t, pf.k8sBroker.ID, "gone")

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "gone-profile"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "", store.PlacementSourceDefault)
}

// Users never inherit, and the agent-create settings do not apply to them.
func TestAgentCreatePlacement_UserCallerKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setAnnotations(t, map[string]string{
		projectSettingAgentCreateBroker:  pf.k8sBroker.ID,
		projectSettingAgentCreateProfile: "gke",
	})

	// newPlacementFixture already grants the owner project membership, so
	// only the members group is seeded here; createAgentAsOwner would also
	// create an owner role binding, which conflicts with that grant.
	ctx := context.Background()
	pf.srv.seedProjectCreatorMembership(ctx, pf.proj)
	rec := doRequestAsUser(t, pf.srv, pf.owner, http.MethodPost,
		"/api/v1/projects/"+pf.proj.ID+"/agents", CreateAgentRequest{Name: "user-create"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	agent, err := pf.store.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	require.NotNil(t, agent.AppliedConfig.Placement)
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "", store.PlacementSourceDefault)
	assert.NotEqual(t, "gke", agent.AppliedConfig.Profile)
	assert.Empty(t, createAuditSummaryFor(t, pf.store, agent.ID), "a user create records no placement in the audit")
}

// The creator's broker must serve the target project; otherwise inheritance
// is not attempted.
func TestAgentCreatePlacement_CreatorBrokerNotServingProjectKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	require.NoError(t, pf.store.RemoveProjectProvider(context.Background(), pf.proj.ID, pf.k8sBroker.ID))

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "not-serving"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "", store.PlacementSourceDefault)
}

// Fail closed: inheritance applies but the creator's broker is offline, so
// the create is refused rather than placed on the default broker.
func TestAgentCreatePlacement_InheritedBrokerOfflineRefuses(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.Status = store.BrokerStatusOffline
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))

	rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "offline-inherit"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "creating agent's broker")
	pf.assertNoIDs(t, rec)
	_, err = pf.store.GetAgentBySlug(ctx, pf.proj.ID, "offline-inherit")
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused create must store nothing")
}

// The dispatch check for an inherited broker runs against the caller's own
// authority: a caller that may not dispatch to the broker is refused, and
// the create does not fall back to another broker.
//
// This calls applyAgentCreatePlacement directly, so it covers the check in
// placementBroker, not the handler's checkBrokerDispatchAccess. Both call
// canUseBrokerForProject with the same request context and broker, so they
// cannot disagree: the handler check sees the inherited broker exactly as an
// explicit --broker, by construction. The deny case cannot be reached over
// HTTP for an agent caller, because the create route itself requires
// agent:create and tier 3 requires the broker to serve the caller's project,
// which together satisfy the agent branch of brokerDispatchAllowed.
// TestAgentCreatePlacement_InheritedBrokerDispatchThroughHandler covers the
// handler path for an inherited broker that is not auto-provide.
func TestAgentCreatePlacement_InheritedBrokerDispatchUsesCallerAuthority(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.AutoProvide = false
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))
	proj, err := pf.store.GetProject(ctx, pf.proj.ID)
	require.NoError(t, err)

	// The caller's credential lacks agent:create, so it may not dispatch to a
	// broker that is not auto-provide, even though the creator runs there.
	callerCtx := contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: pf.caller.ID},
		ProjectID: pf.proj.ID,
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}})
	req := CreateAgentRequest{Name: "dispatch-denied"}
	rec := httptest.NewRecorder()
	placement, ok := pf.srv.applyAgentCreatePlacement(callerCtx, rec, proj, &req)
	require.False(t, ok)
	assert.Nil(t, placement)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "creating agent's broker")
	pf.assertNoIDs(t, rec)

	// The same caller with agent:create is allowed.
	allowedCtx := contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: pf.caller.ID},
		ProjectID: pf.proj.ID,
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate},
	}})
	req = CreateAgentRequest{Name: "dispatch-allowed"}
	rec = httptest.NewRecorder()
	placement, ok = pf.srv.applyAgentCreatePlacement(allowedCtx, rec, proj, &req)
	require.True(t, ok, rec.Body.String())
	assert.Equal(t, pf.k8sBroker.ID, req.RuntimeBrokerID)
	assert.Equal(t, store.PlacementSourceInherited, placement.brokerSource)
	// The profile is inherited once the broker is resolved.
	require.True(t, pf.srv.inheritCreatorProfile(rec, placement, req.RuntimeBrokerID, &req), rec.Body.String())
	assert.Equal(t, "gke", req.Profile)
	assert.Equal(t, store.PlacementSourceInherited, placement.profileSource)
}

// Handler level: an inherited broker that is not auto-provide passes both
// the placement check and the handler's checkBrokerDispatchAccess on the
// caller's own agent:create scope, and the child lands on it.
func TestAgentCreatePlacement_InheritedBrokerDispatchThroughHandler(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.AutoProvide = false
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "dispatch-handler"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "gke", store.PlacementSourceInherited)
}

// Explicit flags win. An explicit broker elsewhere does not carry the
// creator's profile with it.
func TestAgentCreatePlacement_ExplicitBrokerWins(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "explicit-broker", RuntimeBrokerID: pf.dockerBroker.ID})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceFlag, "", store.PlacementSourceDefault)
	assert.NotEqual(t, "gke", agent.AppliedConfig.Profile)
}

// An explicit broker that is the creator's broker still inherits the profile.
func TestAgentCreatePlacement_ExplicitCreatorBrokerInheritsProfile(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "explicit-same-broker", RuntimeBrokerID: pf.k8sBroker.ID})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceFlag, "gke", store.PlacementSourceInherited)
}

// An explicit broker given by name (as the CLI sends a --broker value it
// does not resolve) that names the creator's broker inherits the profile.
func TestAgentCreatePlacement_ExplicitCreatorBrokerByNameInheritsProfile(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "explicit-broker-name", RuntimeBrokerID: "K8S-Broker"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceFlag, "gke", store.PlacementSourceInherited)
}

// An explicit profile the creator's broker offers inherits the broker.
func TestAgentCreatePlacement_ExplicitProfileInheritsBroker(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "explicit-profile", Profile: "local"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "local", store.PlacementSourceFlag)
}

// -p alone still inherits the creator's broker: a profile that broker does
// not offer refuses the create, naming the profile and the broker, and does
// not move the child to another broker. A profile the broker registers but
// reports unavailable is refused the same way.
func TestAgentCreatePlacement_ExplicitProfileNotOnCreatorBrokerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		setup   func(t *testing.T, pf *placementFixture)
	}{
		{name: "not registered", profile: "docker-only"},
		{name: "registered but unavailable", profile: "local", setup: func(t *testing.T, pf *placementFixture) {
			ctx := context.Background()
			k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
			require.NoError(t, err)
			k8s.Profiles = []store.BrokerProfile{
				{Name: "gke", Type: "kubernetes", Available: true},
				{Name: "local", Type: "docker", Available: false},
			}
			require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pf := newPlacementFixture(t)
			enableInheritPlacement(t, pf.srv)
			if tc.setup != nil {
				tc.setup(t, pf)
			}

			rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "explicit-other-profile", Profile: tc.profile})
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.profile, "the error names the requested profile")
			assert.Contains(t, rec.Body.String(), "k8s-broker", "the error names the creator's broker")
			pf.assertNoIDs(t, rec)
			_, err := pf.store.GetAgentBySlug(context.Background(), pf.proj.ID, "explicit-other-profile")
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused create must store nothing")
		})
	}
}

// With the experiment off, -p alone keeps today's default chain.
func TestAgentCreatePlacement_ExplicitProfileExperimentOffKeepsDefaultChain(t *testing.T) {
	pf := newPlacementFixture(t)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "explicit-profile-off", Profile: "docker-only"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "docker-only", store.PlacementSourceFlag)
}

// brokerOffersProfile requires the profile to be registered and available.
func TestAgentCreatePlacement_BrokerOffersProfileChecksAvailable(t *testing.T) {
	b := &store.RuntimeBroker{Profiles: []store.BrokerProfile{
		{Name: "up", Type: "kubernetes", Available: true},
		{Name: "down", Type: "kubernetes", Available: false},
	}}
	assert.True(t, brokerOffersProfile(b, "up"))
	assert.False(t, brokerOffersProfile(b, "down"))
	assert.False(t, brokerOffersProfile(b, "missing"))
	assert.False(t, brokerOffersProfile(b, ""))
	assert.False(t, brokerOffersProfile(nil, "up"))
}

// AC2: the project's agent-create settings apply with no flags, and rank
// above inheritance.
func TestAgentCreatePlacement_SettingsBeatInheritance(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setAnnotations(t, map[string]string{
		projectSettingAgentCreateBroker:  pf.dockerBroker.ID,
		projectSettingAgentCreateProfile: "docker-only",
	})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "setting-wins"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceSetting, "docker-only", store.PlacementSourceSetting)
}

// AC2 with the experiment off: the settings need no experiment.
func TestAgentCreatePlacement_SettingsApplyWithoutExperiment(t *testing.T) {
	pf := newPlacementFixture(t)
	pf.setAnnotations(t, map[string]string{
		projectSettingAgentCreateBroker:  pf.k8sBroker.ID,
		projectSettingAgentCreateProfile: "gke",
	})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "setting-no-experiment"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceSetting, "gke", store.PlacementSourceSetting)
}

// AC3: explicit flags beat the settings.
func TestAgentCreatePlacement_FlagsBeatSettings(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setAnnotations(t, map[string]string{
		projectSettingAgentCreateBroker:  pf.k8sBroker.ID,
		projectSettingAgentCreateProfile: "gke",
	})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "flags-win", RuntimeBrokerID: pf.dockerBroker.ID, Profile: "docker-only"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceFlag, "docker-only", store.PlacementSourceFlag)
}

// A profile-only setting combines with an inherited broker that offers it.
func TestAgentCreatePlacement_ProfileSettingWithInheritedBroker(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setAnnotations(t, map[string]string{projectSettingAgentCreateProfile: "local"})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "profile-setting-inherit"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "local", store.PlacementSourceSetting)
}

// Fail closed: a setting broker that no longer serves the project refuses
// the create, naming the setting and no IDs.
func TestAgentCreatePlacement_SettingBrokerNotServingRefuses(t *testing.T) {
	pf := newPlacementFixture(t)
	pf.setAnnotations(t, map[string]string{projectSettingAgentCreateBroker: "no-such-broker"})

	rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "setting-broker-gone"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "agent-create broker setting")
	pf.assertNoIDs(t, rec)
}

// Fail closed: a setting profile the selected broker does not offer refuses
// the create, both when the setting chose the broker and when the default
// chain did.
func TestAgentCreatePlacement_SettingProfileNotOfferedRefuses(t *testing.T) {
	t.Run("setting broker", func(t *testing.T) {
		pf := newPlacementFixture(t)
		pf.setAnnotations(t, map[string]string{
			projectSettingAgentCreateBroker:  pf.dockerBroker.ID,
			projectSettingAgentCreateProfile: "gke",
		})
		rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "setting-profile-bad"})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "agent-create profile setting")
		pf.assertNoIDs(t, rec)
	})
	t.Run("default chain broker", func(t *testing.T) {
		pf := newPlacementFixture(t)
		pf.setAnnotations(t, map[string]string{projectSettingAgentCreateProfile: "gke"})
		rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "setting-profile-default"})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "agent-create profile setting")
		pf.assertNoIDs(t, rec)
	})
}

// The success audit records the broker and profile names and their sources
// for an inherited placement, and nothing for a default one.
func TestAgentCreatePlacement_AuditRecordsNamesAndSources(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)

	inherited := pf.createAsCreator(t, CreateAgentRequest{Name: "audit-inherit"})
	summary := createAuditSummaryFor(t, pf.store, inherited.ID)
	var got map[string]map[string]string
	require.NoError(t, json.Unmarshal([]byte(summary), &got), summary)
	assert.Equal(t, map[string]string{
		"broker":        "k8s-broker",
		"brokerSource":  store.PlacementSourceInherited,
		"profile":       "gke",
		"profileSource": store.PlacementSourceInherited,
	}, got["placement"])
	assert.NotContains(t, summary, pf.k8sBroker.ID, "the audit names the broker, never its ID")

	explicit := pf.createAsCreator(t, CreateAgentRequest{Name: "audit-explicit", RuntimeBrokerID: pf.dockerBroker.ID, Profile: "local"})
	assert.Empty(t, createAuditSummaryFor(t, pf.store, explicit.ID))
}

func createAuditSummaryFor(t *testing.T, s store.Store, agentID string) string {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentDelegation,
		TargetType:   "agent",
		TargetID:     agentID,
	})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	return recs[0].AfterSummary
}

// capabilityView is the part of a created agent that carries capabilities:
// broker, profile, GCP identity, auth and role, env (secret and auth
// bindings resolved by the hub) and the inline config (image, network and
// resource settings).
type capabilityView struct {
	Broker      string
	Profile     string
	GCPIdentity *store.GCPIdentityConfig
	HarnessAuth string
	NoAuth      bool
	AgentRole   string
	Env         map[string]string
	Inline      string
}

func capabilitiesOf(t *testing.T, a *store.Agent) capabilityView {
	t.Helper()
	ac := a.AppliedConfig
	inline, err := json.Marshal(ac.InlineConfig)
	require.NoError(t, err)
	return capabilityView{
		Broker: a.RuntimeBrokerID, Profile: ac.Profile, GCPIdentity: ac.GCPIdentity,
		HarnessAuth: ac.HarnessAuth, NoAuth: ac.NoAuth, AgentRole: ac.AgentRole,
		Env: ac.Env, Inline: string(inline),
	}
}

// An inherited placement must give the child exactly the capabilities an
// explicit -p/--broker naming the same placement gives under today's rules:
// identity (including the per-profile default service account), auth, role,
// env and inline config. Checked with the service account both allowed and
// denied for the caller.
func TestAgentCreatePlacement_InheritedEqualsExplicitCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowSA   bool
		wantAssig bool
	}{
		{name: "service account allowed", allowSA: true, wantAssig: true},
		{name: "service account denied", allowSA: false, wantAssig: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pf := newPlacementFixture(t)
			enableInheritPlacement(t, pf.srv)
			sa := bypassAgentsCreateSA(t, pf.bypassAgentsFixture, pf.proj.ID, true)
			checker := store.NewFakeCallerPermissionChecker()
			if tc.allowSA {
				checker = checker.AllowTarget(sa.Email)
			}
			enforceSAAssign(pf.srv, checker)
			b, err := json.Marshal(map[string]string{"gke": sa.ID})
			require.NoError(t, err)
			pf.setAnnotations(t, map[string]string{
				projectSettingDefaultGCPIdentitySAIDByProfile: string(b),
				projectSettingAutoExposePortsEnabled:          "true",
			})

			explicitRec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID,
				CreateAgentRequest{Name: "cap-explicit", RuntimeBrokerID: pf.k8sBroker.ID, Profile: "gke"})
			inheritedRec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID,
				CreateAgentRequest{Name: "cap-inherited"})
			require.Equal(t, explicitRec.Code, inheritedRec.Code,
				"explicit: %s\ninherited: %s", explicitRec.Body.String(), inheritedRec.Body.String())
			if explicitRec.Code != http.StatusCreated {
				return
			}
			ctx := context.Background()
			explicit, err := pf.store.GetAgentBySlug(ctx, pf.proj.ID, "cap-explicit")
			require.NoError(t, err)
			inherited, err := pf.store.GetAgentBySlug(ctx, pf.proj.ID, "cap-inherited")
			require.NoError(t, err)
			assert.Equal(t, capabilitiesOf(t, explicit), capabilitiesOf(t, inherited))
			if tc.wantAssig {
				require.NotNil(t, inherited.AppliedConfig.GCPIdentity)
				assert.Equal(t, sa.ID, inherited.AppliedConfig.GCPIdentity.ServiceAccountID)
			} else if inherited.AppliedConfig.GCPIdentity != nil {
				assert.NotEqual(t, sa.ID, inherited.AppliedConfig.GCPIdentity.ServiceAccountID,
					"inheritance must not grant a service account the caller cannot get explicitly")
			}
		})
	}
}

// placementSourceDescription never mentions an ID.
func TestAgentCreatePlacement_SourceDescriptions(t *testing.T) {
	assert.Equal(t, "the project's agent-create broker setting", placementSourceDescription(store.PlacementSourceSetting, "broker"))
	assert.Equal(t, "the creating agent's broker", placementSourceDescription(store.PlacementSourceInherited, "broker"))
	assert.False(t, strings.Contains(placementSourceDescription(store.PlacementSourceSetting, "profile"), "/"))
}

func putPlacementSettings(t *testing.T, pf *placementFixture, body hubclient.ProjectSettings) (*httptest.ResponseRecorder, hubclient.ProjectSettings) {
	t.Helper()
	rec := doRequest(t, pf.srv, http.MethodPut, "/api/v1/projects/"+pf.proj.ID+"/settings", body)
	var resp hubclient.ProjectSettings
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

// The settings round-trip: a broker given by name is stored as its ID, an
// absent field keeps the stored value, and an empty string clears it.
func TestAgentCreatePlacementSettings_RoundTrip(t *testing.T) {
	pf := newPlacementFixture(t)

	rec, resp := putPlacementSettings(t, pf, hubclient.ProjectSettings{
		AgentCreateBroker:  strPtr("K8S-Broker"),
		AgentCreateProfile: strPtr("gke"),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, resp.AgentCreateBroker)
	assert.Equal(t, pf.k8sBroker.ID, *resp.AgentCreateBroker, "the broker is stored as its ID")
	require.NotNil(t, resp.AgentCreateProfile)
	assert.Equal(t, "gke", *resp.AgentCreateProfile)

	// An unrelated save (fields absent) keeps both.
	rec, resp = putPlacementSettings(t, pf, hubclient.ProjectSettings{DefaultModel: "m"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, resp.AgentCreateBroker)
	require.NotNil(t, resp.AgentCreateProfile)

	// Empty strings clear both.
	rec, resp = putPlacementSettings(t, pf, hubclient.ProjectSettings{
		AgentCreateBroker:  strPtr(""),
		AgentCreateProfile: strPtr(""),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, resp.AgentCreateBroker)
	assert.Nil(t, resp.AgentCreateProfile)
}

func TestAgentCreatePlacementSettings_Validation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body hubclient.ProjectSettings
		want string
	}{
		{name: "unknown broker", body: hubclient.ProjectSettings{AgentCreateBroker: strPtr("no-such-broker")}, want: "agentCreateBroker"},
		{name: "profile not on broker", body: hubclient.ProjectSettings{AgentCreateBroker: strPtr("docker-broker"), AgentCreateProfile: strPtr("gke")}, want: "agentCreateProfile"},
		{name: "profile on no broker", body: hubclient.ProjectSettings{AgentCreateProfile: strPtr("nowhere")}, want: "agentCreateProfile"},
		{name: "invalid profile name", body: hubclient.ProjectSettings{AgentCreateProfile: strPtr("a.b")}, want: "agentCreateProfile"},
		{name: "profile unavailable on broker", body: hubclient.ProjectSettings{AgentCreateBroker: strPtr("k8s-broker"), AgentCreateProfile: strPtr("down")}, want: "agentCreateProfile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pf := newPlacementFixture(t)
			ctx := context.Background()
			k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
			require.NoError(t, err)
			k8s.Profiles = append(k8s.Profiles, store.BrokerProfile{Name: "down", Type: "kubernetes", Available: false})
			require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))
			rec, _ := putPlacementSettings(t, pf, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want)
			proj, err := pf.store.GetProject(context.Background(), pf.proj.ID)
			require.NoError(t, err)
			assert.Empty(t, proj.Annotations[projectSettingAgentCreateBroker], "a refused PUT stores nothing")
			assert.Empty(t, proj.Annotations[projectSettingAgentCreateProfile], "a refused PUT stores nothing")
		})
	}

	t.Run("profile only, offered by some broker", func(t *testing.T) {
		pf := newPlacementFixture(t)
		rec, resp := putPlacementSettings(t, pf, hubclient.ProjectSettings{AgentCreateProfile: strPtr("gke")})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, resp.AgentCreateProfile)
		assert.Nil(t, resp.AgentCreateBroker)
	})
}

// setProviderName rewrites the provider row's broker name, as left behind
// when a broker is renamed after it was linked to the project.
func (pf *placementFixture) setProviderName(t *testing.T, brokerID, name string) {
	t.Helper()
	ctx := context.Background()
	pv, err := pf.store.GetProjectProvider(ctx, pf.proj.ID, brokerID)
	require.NoError(t, err)
	pv.BrokerName = name
	require.NoError(t, pf.store.AddProjectProvider(ctx, pv))
}

// staleProviderNames sets up two renames: the creator's broker (record
// name "k8s-broker") was linked as "old-k8s-name", and the docker broker
// was linked under the name "k8s-broker", so a --broker of "k8s-broker"
// matches the docker broker's provider row.
func (pf *placementFixture) staleProviderNames(t *testing.T) {
	t.Helper()
	pf.setProviderName(t, pf.k8sBroker.ID, "old-k8s-name")
	pf.setProviderName(t, pf.dockerBroker.ID, "k8s-broker")
}

// The creator's profile is inherited only onto the broker the create
// actually resolves to. A --broker name that matches another broker's stale
// provider row resolves to that broker, so the profile is not inherited,
// even though the name equals the creator broker's current name.
func TestAgentCreatePlacement_RenamedBrokerResolvesByIDBeforeInheritingProfile(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.staleProviderNames(t)

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "renamed-broker", RuntimeBrokerID: "k8s-broker"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceFlag, "", store.PlacementSourceDefault)
	assert.NotEqual(t, "gke", agent.AppliedConfig.Profile)
}

// The agent-create broker setting resolves a name to the same broker a
// --broker flag with that name resolves to, stale provider rows included.
func TestAgentCreatePlacementSettings_RenamedBrokerMatchesFlagResolution(t *testing.T) {
	pf := newPlacementFixture(t)
	pf.staleProviderNames(t)

	rec, resp := putPlacementSettings(t, pf, hubclient.ProjectSettings{AgentCreateBroker: strPtr("k8s-broker")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, resp.AgentCreateBroker)
	assert.Equal(t, pf.dockerBroker.ID, *resp.AgentCreateBroker)

	broker, err := pf.srv.projectProviderBroker(context.Background(), pf.proj.ID, "old-k8s-name")
	require.NoError(t, err)
	assert.Equal(t, pf.k8sBroker.ID, broker.ID, "a provider-row name still resolves")
}

// Fail closed: inheritance applies but the creator's profile is not
// available on its broker, so a create that would inherit it is refused,
// naming the profile and the broker. A create that passes -p is not
// affected, since it does not inherit the profile.
func TestAgentCreatePlacement_InheritedProfileUnavailableRefuses(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.Profiles = []store.BrokerProfile{
		{Name: "gke", Type: "kubernetes", Available: false},
		{Name: "local", Type: "docker", Available: true},
	}
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))

	rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "inherit-unavailable"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `\"gke\"`, "the error names the profile")
	assert.Contains(t, rec.Body.String(), "k8s-broker", "the error names the broker")
	pf.assertNoIDs(t, rec)
	_, err = pf.store.GetAgentBySlug(ctx, pf.proj.ID, "inherit-unavailable")
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused create must store nothing")

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "inherit-unavailable-p", Profile: "local"})
	assertPlacement(t, agent, pf.k8sBroker.ID, store.PlacementSourceInherited, "local", store.PlacementSourceFlag)
}

// --broker alone ignores the agent-create profile setting.
func TestAgentCreatePlacement_ExplicitBrokerIgnoresProfileSetting(t *testing.T) {
	pf := newPlacementFixture(t)
	pf.setAnnotations(t, map[string]string{projectSettingAgentCreateProfile: "docker-only"})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "broker-ignores-setting", RuntimeBrokerID: pf.dockerBroker.ID})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceFlag, "", store.PlacementSourceDefault)
	assert.NotEqual(t, "docker-only", agent.AppliedConfig.Profile)
}

// With the experiment on, a profile setting the creator's broker does not
// offer does not inherit the broker; the regular chain picks one that
// offers it.
func TestAgentCreatePlacement_ProfileSettingNotOnCreatorBrokerUsesRegularChain(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.setAnnotations(t, map[string]string{projectSettingAgentCreateProfile: "docker-only"})

	agent := pf.createAsCreator(t, CreateAgentRequest{Name: "setting-regular-chain"})
	assertPlacement(t, agent, pf.dockerBroker.ID, store.PlacementSourceDefault, "docker-only", store.PlacementSourceSetting)
}

// Placement runs before the existing-agent check, like broker resolution
// does today: re-running a create for an existing agent name meets the
// same placement refusals as a new create. Here the inherited broker has
// gone offline, so the re-run gets 503 rather than starting the agent.
func TestAgentCreatePlacement_ExistingAgentRerunMeetsPlacementRefusal(t *testing.T) {
	pf := newPlacementFixture(t)
	enableInheritPlacement(t, pf.srv)
	pf.createAsCreator(t, CreateAgentRequest{Name: "rerun-child"})

	ctx := context.Background()
	k8s, err := pf.store.GetRuntimeBroker(ctx, pf.k8sBroker.ID)
	require.NoError(t, err)
	k8s.Status = store.BrokerStatusOffline
	require.NoError(t, pf.store.UpdateRuntimeBroker(ctx, k8s))

	rec := createAsAgent(t, pf.bypassAgentsFixture, pf.caller.ID, CreateAgentRequest{Name: "rerun-child"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "creating agent's broker")
	pf.assertNoIDs(t, rec)
}
