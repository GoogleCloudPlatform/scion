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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker and profile for creates made by an agent (ptone/scion#3933).
//
// An agent-launched create resolves its broker and profile exactly like any
// other create: an explicit --broker / -p wins, then the project default,
// then the hub default. The creating agent's own broker and profile are
// never filled in implicitly.
//
// Fixture: the project has two online providers. The creating agent runs on
// parentBroker under the "gke" profile. otherBroker offers only "local".
// Neither broker is a project or hub default unless a test sets one, so a
// create that names nothing has no broker to fall back on.

type agentCreateDefaultsFixture struct {
	*bypassAgentsFixture
	parentBroker *store.RuntimeBroker
	otherBroker  *store.RuntimeBroker
}

func newAgentCreateDefaultsFixture(t *testing.T) *agentCreateDefaultsFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
	ctx := context.Background()

	parent, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	parent.Name, parent.Slug = "parent-broker", "parent-broker"
	parent.Profiles = []store.BrokerProfile{
		{Name: "gke", Type: "kubernetes", Available: true},
		{Name: "local", Type: "docker", Available: true},
	}
	parent.DefaultProfile = "local"
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, parent))
	pv, err := f.store.GetProjectProvider(ctx, f.proj.ID, parent.ID)
	require.NoError(t, err)
	pv.BrokerName = parent.Name
	require.NoError(t, f.store.AddProjectProvider(ctx, pv))

	other := &store.RuntimeBroker{
		ID:             tid("defaults-other-broker"),
		Name:           "other-broker",
		Slug:           "other-broker",
		Status:         store.BrokerStatusOnline,
		AutoProvide:    true,
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
		DefaultProfile: "local",
		Created:        time.Now(),
		Updated:        time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, other))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.proj.ID, BrokerID: other.ID, BrokerName: other.Name, Status: store.BrokerStatusOnline,
	}))

	caller, err := f.store.GetAgent(ctx, f.caller.ID)
	require.NoError(t, err)
	caller.RuntimeBrokerID = parent.ID
	caller.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull), Profile: "gke"}
	require.NoError(t, f.store.UpdateAgent(ctx, caller))

	df := &agentCreateDefaultsFixture{bypassAgentsFixture: f, parentBroker: parent, otherBroker: other}
	df.setProjectDefaults(t, "", "")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{})
	return df
}

// setProjectDefaults sets the project's default broker and active profile;
// "" clears each.
func (df *agentCreateDefaultsFixture) setProjectDefaults(t *testing.T, brokerID, profile string) {
	t.Helper()
	ctx := context.Background()
	proj, err := df.store.GetProject(ctx, df.proj.ID)
	require.NoError(t, err)
	proj.DefaultRuntimeBrokerID = brokerID
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	setOrDelete(proj.Annotations, projectSettingActiveProfile, profile)
	require.NoError(t, df.store.UpdateProject(ctx, proj))
	df.proj = proj
}

// createAsParent creates an agent as the creating agent and returns the
// persisted record. It fails the test unless the create succeeds.
func (df *agentCreateDefaultsFixture) createAsParent(t *testing.T, req CreateAgentRequest) *store.Agent {
	t.Helper()
	rec := createAsAgent(t, df.bypassAgentsFixture, df.caller.ID, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	got, err := df.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	return got
}

// (a) With no flags, an agent's create takes the project default broker and
// profile, and the hub default broker when the project names none.
func TestAgentCreateDefaults_AgentCallerFollowsProjectThenHub(t *testing.T) {
	t.Run("project default", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		df.setProjectDefaults(t, df.otherBroker.ID, "local")
		// A hub default on the parent's broker must lose to the project.
		setHubAgentDefaults(df.srv, opsettings.AgentDefaultsSettings{DefaultRuntimeBroker: df.parentBroker.ID})

		agent := df.createAsParent(t, CreateAgentRequest{Name: "project-default-child"})
		assert.Equal(t, df.otherBroker.ID, agent.RuntimeBrokerID, "broker from the project default")
		assert.Equal(t, "local", agent.AppliedConfig.Profile, "profile from the project's active profile")
	})

	t.Run("hub default when the project has none", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		setHubAgentDefaults(df.srv, opsettings.AgentDefaultsSettings{DefaultRuntimeBroker: df.otherBroker.ID})

		agent := df.createAsParent(t, CreateAgentRequest{Name: "hub-default-child"})
		assert.Equal(t, df.otherBroker.ID, agent.RuntimeBrokerID, "broker from the hub default")
		assert.Empty(t, agent.AppliedConfig.Profile,
			"no profile is chosen at create; the broker applies its own default")
	})
}

// (b) The creating agent's broker and profile are never used implicitly.
func TestAgentCreateDefaults_ParentPlacementNotInherited(t *testing.T) {
	t.Run("no default broker refuses rather than using the parent's", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)

		rec := createAsAgent(t, df.bypassAgentsFixture, df.caller.ID, CreateAgentRequest{Name: "no-default-child"})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeNoRuntimeBroker)
		_, err := df.store.GetAgentBySlug(context.Background(), df.proj.ID, "no-default-child")
		assert.Error(t, err, "no agent is created")
	})

	t.Run("parent's profile not carried to its own broker", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		// The project default is the parent's broker, but names no profile.
		df.setProjectDefaults(t, df.parentBroker.ID, "")

		agent := df.createAsParent(t, CreateAgentRequest{Name: "same-broker-child"})
		assert.Equal(t, df.parentBroker.ID, agent.RuntimeBrokerID)
		assert.NotEqual(t, "gke", agent.AppliedConfig.Profile, "the parent's profile must not be inherited")
		assert.Empty(t, agent.AppliedConfig.Profile)
	})

	t.Run("project active profile wins over the parent's profile", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		df.setProjectDefaults(t, df.parentBroker.ID, "local")

		agent := df.createAsParent(t, CreateAgentRequest{Name: "project-profile-child"})
		assert.Equal(t, df.parentBroker.ID, agent.RuntimeBrokerID)
		assert.Equal(t, "local", agent.AppliedConfig.Profile)
	})
}

// (c) An explicit broker and profile win over project and hub defaults.
func TestAgentCreateDefaults_ExplicitFlagsWin(t *testing.T) {
	t.Run("broker and profile", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		df.setProjectDefaults(t, df.otherBroker.ID, "local")
		setHubAgentDefaults(df.srv, opsettings.AgentDefaultsSettings{DefaultRuntimeBroker: df.otherBroker.ID})

		agent := df.createAsParent(t, CreateAgentRequest{
			Name: "explicit-child", RuntimeBrokerID: df.parentBroker.Name, Profile: "gke",
		})
		assert.Equal(t, df.parentBroker.ID, agent.RuntimeBrokerID)
		assert.Equal(t, "gke", agent.AppliedConfig.Profile)
	})

	t.Run("profile alone keeps the project default broker", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		df.setProjectDefaults(t, df.parentBroker.ID, "local")

		agent := df.createAsParent(t, CreateAgentRequest{Name: "explicit-profile-child", Profile: "gke"})
		assert.Equal(t, df.parentBroker.ID, agent.RuntimeBrokerID)
		assert.Equal(t, "gke", agent.AppliedConfig.Profile)
	})

	t.Run("broker alone", func(t *testing.T) {
		df := newAgentCreateDefaultsFixture(t)
		df.setProjectDefaults(t, df.parentBroker.ID, "")

		agent := df.createAsParent(t, CreateAgentRequest{Name: "explicit-broker-child", RuntimeBrokerID: df.otherBroker.ID})
		assert.Equal(t, df.otherBroker.ID, agent.RuntimeBrokerID)
		assert.Empty(t, agent.AppliedConfig.Profile)
	})
}
