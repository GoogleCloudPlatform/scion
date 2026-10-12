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

// This file covers ptone/scion#4818: a succeeded launch report records the
// config the broker resolved, as a synchronous create does, and only when
// the launch fencing accepts the report.
package entadapter

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedAgentConfig is what a broker reports after resolving the image
// from the harness-config default (no explicit image on the create).
func resolvedAgentConfig() store.BrokerAgentConfig {
	return store.BrokerAgentConfig{
		Template:            "resolved-tmpl",
		HarnessConfig:       "claude",
		HarnessAuth:         "api-key",
		HarnessConfigSource: "template",
		Image:               "img:harness-default",
		Profile:             "resolved-prof",
		Runtime:             "kubernetes",
	}
}

// beginConfiguredTestLaunch begins a launch for an agent whose applied
// config is what the hub recorded before dispatch: a pre-dispatch harness
// config and no image.
func beginConfiguredTestLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) (*store.Agent, string) {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.Phase = "created"
	a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "pre-dispatch", HarnessAuth: "pre-auth"}
	require.NoError(t, s.CreateAgent(ctx, a))
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	return a, launchID
}

func harnessConfigColumn(t *testing.T, ctx context.Context, s *AgentStore, agentID string) string {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	row, err := s.client.Agent.Get(ctx, uid)
	require.NoError(t, err)
	return row.HarnessConfig
}

func TestApplyLaunchReport_SucceededAppliesBrokerAgentConfig(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginConfiguredTestLaunch(t, ctx, s, projectID, "cfg-succeeded")

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: resolvedAgentConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, "img:harness-default", got.AppliedConfig.Image)
	assert.Equal(t, "claude", got.AppliedConfig.HarnessConfig)
	assert.Equal(t, "api-key", got.AppliedConfig.HarnessAuth)
	assert.Equal(t, "template", got.AppliedConfig.HarnessConfigSource)
	assert.Equal(t, "resolved-prof", got.AppliedConfig.Profile)
	assert.Equal(t, "resolved-tmpl", got.Template)
	assert.Equal(t, "kubernetes", got.Runtime)
	assert.Equal(t, "claude", harnessConfigColumn(t, ctx, s, a.ID), "the harness_config shadow column follows applied_config")
}

// The sync path and the report path apply the same helper, so the same
// broker answer yields the same agent record.
func TestApplyLaunchReport_SucceededMatchesSyncApply(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginConfiguredTestLaunch(t, ctx, s, projectID, "cfg-parity")

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	syncAgent := *before
	syncCfg := *before.AppliedConfig
	syncAgent.AppliedConfig = &syncCfg
	store.ApplyBrokerAgentConfig(&syncAgent, resolvedAgentConfig())

	_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: resolvedAgentConfig(),
	})
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, *syncAgent.AppliedConfig, *got.AppliedConfig)
	assert.Equal(t, syncAgent.Template, got.Template)
	assert.Equal(t, syncAgent.Runtime, got.Runtime)
}

func TestApplyLaunchReport_RunningObservedSucceededAppliesBrokerAgentConfig(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginConfiguredTestLaunch(t, ctx, s, projectID, "cfg-ro")
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: resolvedAgentConfig(),
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "img:harness-default", got.AppliedConfig.Image)
}

func TestApplyLaunchReport_StaleSucceededDoesNotApplyConfig(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport
	}{
		{"other launch", func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport {
			return store.LaunchReport{LaunchID: "another-launch", InstanceID: "i1"}
		}},
		{"other owner", func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport {
			_, err := s.MarkLaunchAccepted(ctx, a.ID, launchID, "i1")
			require.NoError(t, err)
			return store.LaunchReport{LaunchID: launchID, InstanceID: "i2"}
		}},
		{"superseded", func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport {
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonSuperseded))
			return store.LaunchReport{LaunchID: launchID, InstanceID: "i1"}
		}},
		{"failed", func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport {
			_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
			})
			require.NoError(t, err)
			return store.LaunchReport{LaunchID: launchID, InstanceID: "i1"}
		}},
		{"timed out", func(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID string) store.LaunchReport {
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonTimedOut))
			return store.LaunchReport{LaunchID: launchID, InstanceID: "i1"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			a, launchID := beginConfiguredTestLaunch(t, ctx, s, projectID, "cfg-stale")
			r := tc.setup(t, ctx, s, a, launchID)
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)

			r.State = store.LaunchReportStateSucceeded
			r.AgentConfig = resolvedAgentConfig()
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, r)
			require.NoError(t, err)
			assert.Equal(t, http.StatusConflict, answer.HTTPStatus)

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, *before.AppliedConfig, *got.AppliedConfig, "a rejected report must not write the config")
			assert.Empty(t, got.AppliedConfig.Image)
			assert.Equal(t, before.Template, got.Template)
			assert.Equal(t, before.Runtime, got.Runtime)
			assert.Equal(t, "pre-dispatch", harnessConfigColumn(t, ctx, s, a.ID))
		})
	}
}

func TestApplyLaunchReport_SucceededPinnedKeepsProfile(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginConfiguredTestLaunch(t, ctx, s, projectID, "cfg-pinned")
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPinnedRuntimeTargetID("target-1").Save(ctx)
	require.NoError(t, err)

	_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: resolvedAgentConfig(),
	})
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.AppliedConfig.Profile, "no broker-reported profile is written onto a pinned agent")
	assert.Equal(t, "img:harness-default", got.AppliedConfig.Image)
}

// An agent with no applied config gets none created, as on the sync path.
func TestApplyLaunchReport_SucceededWithoutAppliedConfigLeavesItEmpty(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "cfg-none")

	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: resolvedAgentConfig(),
	})
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.AppliedConfig)
	assert.Equal(t, "resolved-tmpl", got.Template)
	assert.Equal(t, "kubernetes", got.Runtime)
}
