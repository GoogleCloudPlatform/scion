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

// vertexGatherFunc answers a reprovision gather like a broker
// reprovisioning an agent onto a vertex-ai harness-config: the first pass
// needs the project and the region (accepting GOOGLE_CLOUD_LOCATION for the
// region, as claude's vertex-ai key group does), and the replay is accepted
// with the reprovision and home-reset echoes.
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
			Reprovisioned: true,
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

// switchStart runs a reprovision and its start the way the reincarnate
// worker does, with a reprovisionGatheredEnv holder when homeReset is set.
func switchStart(t *testing.T, m *ldFixture, homeReset bool) {
	t.Helper()
	m.agent.AppliedConfig.HarnessConfig = "claude"
	m.client.createWithGatherFunc = vertexGatherFunc()
	ctx := context.Background()
	opts := ReprovisionOptions{}
	if homeReset {
		ctx = withReprovisionGatheredEnv(ctx)
		opts = ReprovisionOptions{ResetHome: true, ResetID: "reset-1"}
	}
	require.NoError(t, m.d.DispatchAgentReprovision(ctx, m.agent, opts))
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

	switchStart(t, m, true)

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

// TestPlainReprovisionStartUnchanged pins that a reincarnation without a
// home reset (no holder in ctx) starts exactly as before: the start carries
// no as_needed env, whatever the reprovision gather resolved.
func TestPlainReprovisionStartUnchanged(t *testing.T) {
	m := newLDFixture(t, "plain-gathered", ldOptions{})
	m.asNeededEnv(t, "GOOGLE_CLOUD_PROJECT", "vertex-project-value", store.ScopeProject)
	m.asNeededSecret(t, "GOOGLE_CLOUD_REGION", "vertex-region-value", store.ScopeProject)

	switchStart(t, m, false)

	assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_PROJECT")
	assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_REGION")
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

// TestReincarnateHomeResetStartAuthorizesGatheredEnv pins that the carried
// names are admitted only through the start's delivery authorization: an
// as_needed project secret the delivery decision refuses is not delivered
// to the switch's start, though the reprovision gather resolved it, while an
// admitted one is. The same holds for an as_needed env var.
func TestReincarnateHomeResetStartAuthorizesGatheredEnv(t *testing.T) {
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
			m := newLDFixture(t, "switch-denied-"+strings.ReplaceAll(tc.name, " ", "-"), ldOptions{})
			tc.store(m)
			// The delivery decision refuses the region item only.
			hdWithStore(t, m.authz, &ldHiddenKeyStore{Store: m.store, hidden: "GOOGLE_CLOUD_REGION"})

			switchStart(t, m, true)

			assert.Equal(t, "vertex-project-value", m.client.lastResolvedEnv["GOOGLE_CLOUD_PROJECT"], "the admitted item is delivered")
			assert.NotContains(t, m.client.lastResolvedEnv, "GOOGLE_CLOUD_REGION", "the refused item is not delivered")
			for _, s := range m.client.lastStartResolvedSecrets {
				assert.NotEqual(t, "GOOGLE_CLOUD_REGION", s.Target, "the refused item is not delivered as a secret either")
			}
		})
	}
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

// TestReincarnateWorker_GatheredEnvHolderOnlyForHomeReset pins the worker
// side: a harness switch (home reset) hands its reprovision and its start
// the same reprovisionGatheredEnv holder; a later plain reincarnation hands
// neither one a holder, so it starts as before.
func TestReincarnateWorker_GatheredEnvHolderOnlyForHomeReset(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc", Handoff: "HANDOFF-TEXT"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

	f.disp.mu.Lock()
	reprov := append([]*reprovisionGatheredEnv(nil), f.disp.reprovisionGatheredEnvHolders...)
	starts := append([]*reprovisionGatheredEnv(nil), f.disp.startGatheredEnvHolders...)
	f.disp.mu.Unlock()
	require.Len(t, reprov, 1)
	require.Len(t, starts, 1)
	require.NotNil(t, reprov[0], "a home-reset reprovision gets a holder")
	assert.Same(t, reprov[0], starts[0], "the start gets the reprovision's holder")

	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForGeneration(t, f.s, f.agent.ID, 3)

	f.disp.mu.Lock()
	defer f.disp.mu.Unlock()
	require.Len(t, f.disp.reprovisionGatheredEnvHolders, 2)
	require.Len(t, f.disp.startGatheredEnvHolders, 2)
	assert.Nil(t, f.disp.reprovisionGatheredEnvHolders[1], "a plain reincarnation gets no holder")
	assert.Nil(t, f.disp.startGatheredEnvHolders[1], "a plain reincarnation's start gets no holder")
}
