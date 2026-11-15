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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// vertexGatherFunc answers a reprovision or move-provision gather like a broker
// provisioning an agent onto a vertex-ai harness-config: the first pass
// needs the project and the region (accepting GOOGLE_CLOUD_LOCATION for the
// region, as claude's vertex-ai key group does), and the replay is accepted
// with the reprovision and home-reset echoes when requested.
func vertexGatherFunc() func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		if req.ResolvedEnv["GOOGLE_CLOUD_PROJECT"] == "" {
			return nil, &RemoteEnvRequirementsResponse{
				AgentID:      req.ID,
				Needs:        []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"},
				Alternatives: map[string][]string{"GOOGLE_CLOUD_REGION": {"CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}},
			}, nil
		}
		return &RemoteAgentResponse{
			Agent:         &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name},
			Created:       true,
			Reprovisioned: req.Reprovision,
			HomeReset:     req.ResetHome,
		}, nil, nil
	}
}

// asNeededEnv stores an as_needed env var row for the fixture agent.
func (m *ldFixture) asNeededEnv(t *testing.T, key, value, scope string) {
	t.Helper()
	v := store.EnvVar{
		ID: api.NewUUID(), Key: key, Value: value, Scope: scope, ScopeID: m.scopeID(scope),
		InjectionMode: store.InjectionModeAsNeeded, CreatedBy: m.projectOwnerID,
	}
	require.NoError(t, m.store.CreateEnvVar(context.Background(), &v))
}

// asNeededSecret stores an as_needed environment secret for the fixture agent.
func (m *ldFixture) asNeededSecret(t *testing.T, name, value, scope string) {
	t.Helper()
	_, _, err := m.backend.Set(context.Background(), &secret.SetSecretInput{
		Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Target: name,
		Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: store.InjectionModeAsNeeded,
		CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)
}

// reincarnateStart runs a reprovision and its start the way the reincarnate
// worker does, carrying a reprovisionGatheredEnv holder for every
// reincarnation (with or without a home reset).
func reincarnateStart(t *testing.T, m *ldFixture, homeReset bool) {
	t.Helper()
	m.agent.AppliedConfig.HarnessConfig = "claude"
	m.client.createWithGatherFunc = vertexGatherFunc()
	ctx := withReprovisionGatheredEnv(context.Background())
	opts := ReprovisionOptions{}
	if homeReset {
		opts = ReprovisionOptions{ResetHome: true, ResetID: "reset-1"}
	}
	require.NoError(t, m.d.DispatchAgentReprovision(ctx, m.agent, opts))
	require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "task", false))
	require.True(t, m.client.startCalled)
}

// reincarnateMoveStart runs a cross-broker move provision and its start the way
// the reincarnate worker does, carrying a reprovisionGatheredEnv holder.
func reincarnateMoveStart(t *testing.T, m *ldFixture) {
	t.Helper()
	m.broker.Capabilities = &store.BrokerCapabilities{AgentMove: true}
	require.NoError(t, m.store.UpdateRuntimeBroker(context.Background(), m.broker))
	m.agent.AppliedConfig.HarnessConfig = "claude"
	m.client.createWithGatherFunc = vertexGatherFunc()
	ctx := withReprovisionGatheredEnv(context.Background())
	require.NoError(t, m.d.DispatchAgentProvisionForMove(ctx, m.agent, "agent-dir", ""))
	require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "task", false))
	require.True(t, m.client.startCalled)
}

// TestReincarnateHomeResetStartCarriesReprovisionGatheredEnv is the
// ptone/scion#7757 regression test: after a harness switch, the start that
// follows the home-reset reprovision must carry the as_needed env the
// reprovision gather resolved for the new harness. A start resolves no
// as_needed env itself, and the broker's recorded secrets belong to the old
// harness-config, so without this the new harness starts without its Vertex
// AI project and region and its provisioner fails in the pre-start hook. The
// region is stored under GOOGLE_CLOUD_LOCATION, as on the hub where the
// defect was found, and reaches the start under the canonical key the broker
// asked for.
func TestReincarnateHomeResetStartCarriesReprovisionGatheredEnv(t *testing.T) {
	m := newLDFixture(t, "switch-gathered", ldOptions{})
	m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
	m.asNeededEnv(t, "GOOGLE_CLOUD_LOCATION", "vertex-location-value", store.ScopeProject)

	reincarnateStart(t, m, true)

	env := m.client.lastResolvedEnv
	assert.Equal(t, "vertex-project-value", env["GOOGLE_CLOUD_PROJECT"])
	assert.Equal(t, "vertex-location-value", env["GOOGLE_CLOUD_REGION"], "the alternative name fills the canonical key")
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		kind, ok := api.ClassifyEnvKey(m.client.lastStartExtras.EnvClassifications, k)
		require.True(t, ok, "%s must carry a classification", k)
		assert.Equal(t, api.EnvKindSecretFetchable, kind, "%s is as_needed storage env", k)
	}
	// Nothing is persisted to the applied config.
	assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_PROJECT")
	assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_REGION")
}

// TestPlainReincarnateStartCarriesReprovisionGatheredEnv pins ptone/scion#8084:
// an in-place reincarnation without a home reset carries the as_needed env and
// environment secrets its reprovision gather resolved into DispatchAgentStart,
// so reincarnating an agent refreshes hub as_needed credentials and heals an
// already-overwritten broker secrets record.
func TestPlainReincarnateStartCarriesReprovisionGatheredEnv(t *testing.T) {
	m := newLDFixture(t, "plain-gathered", ldOptions{})
	m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
	m.asNeededSecret(t, "GOOGLE_CLOUD_REGION", "vertex-region-value", store.ScopeProject)

	reincarnateStart(t, m, false)

	env := m.client.lastResolvedEnv
	assert.Equal(t, "vertex-project-value", env["GOOGLE_CLOUD_PROJECT"])
	assert.Equal(t, "vertex-region-value", env["GOOGLE_CLOUD_REGION"])
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		kind, ok := api.ClassifyEnvKey(m.client.lastStartExtras.EnvClassifications, k)
		require.True(t, ok, "%s must carry a classification", k)
		assert.Equal(t, api.EnvKindSecretFetchable, kind, "%s is as_needed storage env", k)
	}
	assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_PROJECT")
	assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_REGION")
}

// TestReincarnateMoveStartCarriesProvisionGatheredEnv pins ptone/scion#8084
// on the cross-broker move path: DispatchAgentProvisionForMove runs with
// Reprovision=false, yet records its gathered as_needed names into the
// context holder so DispatchAgentStart populates the target broker's empty
// secrets record. A standalone DispatchAgentProvision (no holder in ctx)
// remains a no-op.
func TestReincarnateMoveStartCarriesProvisionGatheredEnv(t *testing.T) {
	t.Run("move provision carries gathered env to start", func(t *testing.T) {
		m := newLDFixture(t, "move-gathered", ldOptions{})
		m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
		m.asNeededSecret(t, "GOOGLE_CLOUD_LOCATION", "vertex-location-value", store.ScopeProject)

		reincarnateMoveStart(t, m)

		env := m.client.lastResolvedEnv
		assert.Equal(t, "vertex-project-value", env["GOOGLE_CLOUD_PROJECT"])
		assert.Equal(t, "vertex-location-value", env["GOOGLE_CLOUD_REGION"], "the alternative secret name fills the canonical key")
		for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
			kind, ok := api.ClassifyEnvKey(m.client.lastStartExtras.EnvClassifications, k)
			require.True(t, ok, "%s must carry a classification", k)
			assert.Equal(t, api.EnvKindSecretFetchable, kind, "%s is as_needed storage env", k)
		}
		assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_PROJECT")
		assert.NotContains(t, m.agent.AppliedConfig.Env, "GOOGLE_CLOUD_REGION")
	})

	t.Run("plain provision without holder leaves start unchanged", func(t *testing.T) {
		m := newLDFixture(t, "provision-no-holder", ldOptions{})
		m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
		m.asNeededSecret(t, "GOOGLE_CLOUD_REGION", "vertex-region-value", store.ScopeProject)

		m.agent.AppliedConfig.HarnessConfig = "claude"
		m.client.createWithGatherFunc = vertexGatherFunc()
		ctx := context.Background()
		require.NoError(t, m.d.DispatchAgentProvision(ctx, m.agent))
		require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "task", false))
		require.True(t, m.client.startCalled)

		assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_PROJECT")
		assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_REGION")
	})
}

// ldHiddenKeyStore answers not-found for the record of one secret or env
// var key, so the delivery association refuses that item only.
type ldHiddenKeyStore struct {
	store.Store
	hidden string
}

func (s *ldHiddenKeyStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	if key == s.hidden {
		return nil, store.ErrNotFound
	}
	return s.Store.GetSecret(ctx, key, scope, scopeID)
}

func (s *ldHiddenKeyStore) GetEnvVar(ctx context.Context, key, scope, scopeID string) (*store.EnvVar, error) {
	if key == s.hidden {
		return nil, store.ErrNotFound
	}
	return s.Store.GetEnvVar(ctx, key, scope, scopeID)
}

func assertReincarnateStartAuthorizesGatheredEnv(t *testing.T, prefix string, run func(t *testing.T, m *ldFixture)) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		store func(m *ldFixture)
	}{
		{name: "secret", store: func(m *ldFixture) {
			m.asNeededSecret(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
			m.asNeededSecret(t, "GOOGLE_CLOUD_REGION", "vertex-region-value", store.ScopeProject)
		}},
		{name: "env var", store: func(m *ldFixture) {
			m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
			m.asNeededEnv(t, "GOOGLE_CLOUD_REGION", "vertex-region-value", store.ScopeProject)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLDFixture(t, prefix+"-"+strings.ReplaceAll(tc.name, " ", "-"), ldOptions{})
			tc.store(m)
			// The delivery decision refuses the region item only.
			hdWithStore(t, m.authz, &ldHiddenKeyStore{Store: m.store, hidden: "GOOGLE_CLOUD_REGION"})

			run(t, m)

			assert.Equal(t, "vertex-project-value", m.client.lastResolvedEnv["GOOGLE_CLOUD_PROJECT"], "the admitted item is delivered")
			assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_REGION", "the refused item is not delivered")
			for _, s := range m.client.lastStartResolvedSecrets {
				assert.NotEqual(t, "GOOGLE_CLOUD_REGION", s.Target, "the refused item is not delivered as a secret either")
			}
		})
	}
}

// TestReincarnateHomeResetStartAuthorizesGatheredEnv pins that the carried
// names are admitted only through the start's delivery authorization: an
// as_needed project secret the delivery decision refuses is not delivered
// to the switch's start, though the reprovision gather resolved it, while an
// admitted one is. The same holds for an as_needed env var.
func TestReincarnateHomeResetStartAuthorizesGatheredEnv(t *testing.T) {
	assertReincarnateStartAuthorizesGatheredEnv(t, "switch-denied", func(t *testing.T, m *ldFixture) {
		reincarnateStart(t, m, true)
	})
}

// TestPlainReincarnateStartAuthorizesGatheredEnv is the homeReset=false twin
// of TestReincarnateHomeResetStartAuthorizesGatheredEnv: unauthorized
// as_needed secrets and env vars remain denied by deliverySelector on plain
// reincarnate starts.
func TestPlainReincarnateStartAuthorizesGatheredEnv(t *testing.T) {
	assertReincarnateStartAuthorizesGatheredEnv(t, "plain-denied", func(t *testing.T, m *ldFixture) {
		reincarnateStart(t, m, false)
	})
}

// TestReincarnateMoveStartAuthorizesGatheredEnv is the cross-broker move twin
// of TestReincarnateHomeResetStartAuthorizesGatheredEnv: unauthorized
// as_needed secrets and env vars remain denied by deliverySelector when
// DispatchAgentProvisionForMove precedes DispatchAgentStart.
func TestReincarnateMoveStartAuthorizesGatheredEnv(t *testing.T) {
	assertReincarnateStartAuthorizesGatheredEnv(t, "move-denied", reincarnateMoveStart)
}

// TestReprovisionGatheredEnvNames pins the holder: it keeps names only,
// maps alternatives to their canonical key, never lets an alternative
// replace a canonical name, and a nil holder admits nothing.
func TestReprovisionGatheredEnvNames(t *testing.T) {
	ctx := withReprovisionGatheredEnv(context.Background())
	recordReprovisionGatheredEnv(ctx,
		map[string]string{"GOOGLE_CLOUD_PROJECT": "v1", "GOOGLE_CLOUD_REGION": "v2"},
		map[string][]string{
			"GOOGLE_CLOUD_REGION":    {"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_PROJECT"},
			"NOT_RESOLVED_CANONICAL": {"SOME_ALT"},
		})
	h := reprovisionGatheredEnvFrom(ctx)
	require.NotNil(t, h)
	assert.Equal(t, map[string]string{
		"GOOGLE_CLOUD_PROJECT":  "GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_REGION":   "GOOGLE_CLOUD_REGION",
		"GOOGLE_CLOUD_LOCATION": "GOOGLE_CLOUD_REGION",
	}, h.keys)

	plain := context.Background()
	recordReprovisionGatheredEnv(plain, map[string]string{"K": "v"}, nil)
	_, ok := reprovisionGatheredEnvFrom(plain).canonicalKey("K")
	assert.False(t, ok)
}

// TestReincarnateWorker_GatheredEnvHolderForEveryReincarnation pins the worker
// side: every reincarnation — home-reset harness switch, plain in-place
// reincarnation (homeReset=false), and cross-broker move (move!=nil) — hands
// its provision/reprovision leg and its start leg the same fresh
// reprovisionGatheredEnv holder.
func TestReincarnateWorker_GatheredEnvHolderForEveryReincarnation(t *testing.T) {
	t.Run("home reset and plain in-place", func(t *testing.T) {
		f := newHarnessSwitchFixture(t, nil, nil)
		rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc", Handoff: "HANDOFF-TEXT"})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		r := waitForReincarnationSettled(t, f.s, f.agent.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

		rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		waitForGeneration(t, f.s, f.agent.ID, 3)

		f.disp.mu.Lock()
		reprov := append([]*reprovisionGatheredEnv(nil), f.disp.reprovisionGatheredEnvHolders...)
		starts := append([]*reprovisionGatheredEnv(nil), f.disp.startGatheredEnvHolders...)
		f.disp.mu.Unlock()
		require.Len(t, reprov, 2)
		require.Len(t, starts, 2)
		require.NotNil(t, reprov[0], "a home-reset reprovision gets a holder")
		assert.Same(t, reprov[0], starts[0], "the home-reset start gets its reprovision's holder")
		require.NotNil(t, reprov[1], "a plain reincarnation reprovision gets a holder")
		assert.Same(t, reprov[1], starts[1], "the plain reincarnation start gets its reprovision's holder")
		assert.NotSame(t, reprov[0], reprov[1], "each reincarnation gets its own holder")
	})

	t.Run("cross-broker move", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		prepareMoveWorkerFixture(t, f)

		r, _ := startMove(t, f)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

		f.disp.mu.Lock()
		moveProv := append([]*reprovisionGatheredEnv(nil), f.disp.moveProvisionGatheredEnvHolders...)
		starts := append([]*reprovisionGatheredEnv(nil), f.disp.startGatheredEnvHolders...)
		f.disp.mu.Unlock()
		require.Len(t, moveProv, 1)
		require.Len(t, starts, 1)
		require.NotNil(t, moveProv[0], "a cross-broker move's provision gets a holder")
		assert.Same(t, moveProv[0], starts[0], "the move's start gets its provision's holder")
	})
}
