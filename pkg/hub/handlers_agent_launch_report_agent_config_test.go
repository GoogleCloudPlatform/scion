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

// This file covers ptone/scion#4818 at the report endpoint: a succeeded
// async launch report records the config the broker resolved, the same as
// a synchronous create's broker answer does.
package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedRemoteAgentInfo is a broker's answer for a start that took the
// image from the harness-config default.
func resolvedRemoteAgentInfo() *RemoteAgentInfo {
	return &RemoteAgentInfo{
		Template:            "resolved-tmpl",
		HarnessConfig:       "claude",
		HarnessAuth:         "api-key",
		HarnessConfigSource: "template",
		Image:               "img:harness-default",
		Profile:             "resolved-prof",
		Runtime:             "kubernetes",
	}
}

func seedConfiguredLaunch(t *testing.T, s store.Store, slug string) (*store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{ID: tid(slug + "-project"), Slug: slug + "-project", Name: slug, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: project.ID, Template: "requested-tmpl",
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "pre-dispatch", HarnessAuth: "pre-auth"},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	return agent, launchID
}

func TestAgentLaunchReport_SucceededRecordsResolvedConfig(t *testing.T) {
	srv, s := testServer(t)
	agent, launchID := seedConfiguredLaunch(t, s, "lr-cfg-agent")

	// What a synchronous create records for the same broker answer.
	syncAgent := *mustAgent(t, s, agent.ID)
	syncCfg := *syncAgent.AppliedConfig
	syncAgent.AppliedConfig = &syncCfg
	applyBrokerAgentConfig(&syncAgent, resolvedRemoteAgentInfo())

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded", Agent: resolvedRemoteAgentInfo(),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := mustAgent(t, s, agent.ID)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, "img:harness-default", got.AppliedConfig.Image)
	assert.Equal(t, "claude", got.AppliedConfig.HarnessConfig)
	assert.Equal(t, "api-key", got.AppliedConfig.HarnessAuth)
	assert.Equal(t, "template", got.AppliedConfig.HarnessConfigSource)
	assert.Equal(t, "resolved-prof", got.AppliedConfig.Profile)
	assert.Equal(t, "resolved-tmpl", got.Template)
	assert.Equal(t, "kubernetes", got.Runtime)

	// Parity with the synchronous path.
	assert.Equal(t, *syncAgent.AppliedConfig, *got.AppliedConfig)
	assert.Equal(t, syncAgent.Template, got.Template)
	assert.Equal(t, syncAgent.Runtime, got.Runtime)
}

func TestAgentLaunchReport_StaleSucceededDoesNotRecordConfig(t *testing.T) {
	srv, s := testServer(t)
	agent, _ := seedConfiguredLaunch(t, s, "lr-cfg-stale")
	before := mustAgent(t, s, agent.ID)

	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: "an-earlier-launch", InstanceID: "i1", State: "succeeded", Agent: resolvedRemoteAgentInfo(),
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	got := mustAgent(t, s, agent.ID)
	assert.Equal(t, *before.AppliedConfig, *got.AppliedConfig, "a superseded report must not write the config")
	assert.Equal(t, before.Template, got.Template)
	assert.Equal(t, before.Runtime, got.Runtime)
}
