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
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Dispatch-side tests for the agent TZ chain: the resolver is the only
// source of TZ in the env a broker receives on create, start and restart
// (I3), legacy env TZ is adopted before resolving, and TZ is never gathered
// from the CLI (I4).

const tzDispatchHubID = "tz-dispatch-hub"

type tzDispatchFixture struct {
	d      *HTTPAgentDispatcher
	store  store.Store
	client *mockRuntimeBrokerClient
	agent  *store.Agent
	// hubDefault is read live by the dispatcher's agent-defaults provider.
	hubDefault string
}

func newTZDispatchFixture(t *testing.T, hubDefault string) *tzDispatchFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID:       tid("tz-dispatch-broker-" + t.Name()),
		Name:     "tz-dispatch-broker",
		Slug:     "tz-dispatch-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	f := &tzDispatchFixture{store: s, client: &mockRuntimeBrokerClient{}, hubDefault: hubDefault}
	f.d = NewHTTPAgentDispatcherWithClient(s, f.client, false, slog.Default())
	f.d.SetHubID(tzDispatchHubID)
	f.d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: f.hubDefault}
	})
	f.agent = &store.Agent{
		ID:              tid("tz-dispatch-agent-" + t.Name()),
		Name:            "tz-dispatch-agent",
		Slug:            "tz-dispatch-agent",
		ProjectID:       tid("tz-dispatch-project"),
		OwnerID:         tid("tz-dispatch-user"),
		RuntimeBrokerID: broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	return f
}

func (f *tzDispatchFixture) seedEnv(t *testing.T, scope, scopeID, value, mode string) {
	t.Helper()
	tzTestEnvVar(t, f.store, store.EnvVar{Scope: scope, ScopeID: scopeID, Value: value, InjectionMode: mode})
}

func (f *tzDispatchFixture) createTZ(t *testing.T) string {
	t.Helper()
	_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	require.NotNil(t, f.client.lastCreateReq)
	return f.client.lastCreateReq.ResolvedEnv["TZ"]
}

func (f *tzDispatchFixture) startTZ(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	return f.client.lastResolvedEnv["TZ"]
}

func (f *tzDispatchFixture) restartTZ(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.d.DispatchAgentRestart(context.Background(), f.agent))
	return f.client.lastRestartResolvedEnv["TZ"]
}

// TestAgentTZDispatch_ChainOnCreateStartRestart covers each rung of the
// chain on all three dispatch paths (AC8, AC9).
func TestAgentTZDispatch_ChainOnCreateStartRestart(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *tzDispatchFixture)
		want  string
	}{
		{
			name: "explicit pin beats storage and hub default",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.agent.AppliedConfig.ExplicitTimezone = "Asia/Kathmandu"
				f.seedEnv(t, store.ScopeUser, f.agent.OwnerID, "Europe/Paris", "")
			},
			want: "Asia/Kathmandu",
		},
		{
			name: "user storage beats hub default",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.seedEnv(t, store.ScopeUser, f.agent.OwnerID, "Europe/Paris", "")
			},
			want: "Europe/Paris",
		},
		{
			name: "project storage beats hub default",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.seedEnv(t, store.ScopeProject, f.agent.ProjectID, "Europe/Lisbon", "")
			},
			want: "Europe/Lisbon",
		},
		{
			name:  "hub default when no other rung",
			setup: func(t *testing.T, f *tzDispatchFixture) {},
			want:  "Asia/Tokyo",
		},
		{
			name: "unpin tombstone falls through to hub default",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.agent.AppliedConfig.ExplicitTimezoneUnpinned = true
			},
			want: "Asia/Tokyo",
		},
		{
			name: "no rungs sends no TZ",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.hubDefault = ""
			},
			want: "",
		},
		{
			name: "as_needed storage TZ is not a rung",
			setup: func(t *testing.T, f *tzDispatchFixture) {
				f.seedEnv(t, store.ScopeUser, f.agent.OwnerID, "America/Denver", store.InjectionModeAsNeeded)
			},
			want: "Asia/Tokyo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTZDispatchFixture(t, "Asia/Tokyo")
			tt.setup(t, f)
			assert.Equal(t, tt.want, f.createTZ(t), "create")
			assert.Equal(t, tt.want, f.startTZ(t), "start")
			assert.Equal(t, tt.want, f.restartTZ(t), "restart")

			_, inEnv := f.client.lastCreateReq.ResolvedEnv["TZ"]
			_, classified := f.client.lastCreateReq.EnvClassifications["TZ"]
			assert.Equal(t, tt.want != "", inEnv, "TZ present in the create env only when resolved")
			assert.Equal(t, inEnv, classified, "a dispatched TZ is classified, and only then")
			if tt.want != "" {
				assert.Equal(t, api.EnvKindPlain, f.client.lastCreateReq.EnvClassifications["TZ"])
			}
		})
	}
}

// TestAgentTZDispatch_HubDefaultIsLive checks a hub default edit reaches an
// existing unpinned agent at its next start, and not a pinned one (AC9).
func TestAgentTZDispatch_HubDefaultIsLive(t *testing.T) {
	f := newTZDispatchFixture(t, "Asia/Tokyo")
	assert.Equal(t, "Asia/Tokyo", f.createTZ(t))
	f.hubDefault = "Asia/Kathmandu"
	assert.Equal(t, "Asia/Kathmandu", f.startTZ(t))
	assert.Equal(t, "Asia/Kathmandu", f.restartTZ(t))

	f.agent.AppliedConfig.ExplicitTimezone = "UTC"
	f.hubDefault = "Europe/Paris"
	assert.Equal(t, "UTC", f.startTZ(t), "a pinned agent ignores the hub default")
}

// TestAgentTZDispatch_LegacyAdoption checks an agent written by an older hub
// with TZ in its env records keeps that zone as a legacy pin, whichever copy
// holds it, and the TZ leaves the env records (AC9, legacy adoption).
func TestAgentTZDispatch_LegacyAdoption(t *testing.T) {
	tests := []struct {
		name string
		ac   func() *store.AgentAppliedConfig
	}{
		{name: "env", ac: func() *store.AgentAppliedConfig {
			return &store.AgentAppliedConfig{HarnessConfig: "claude", Env: map[string]string{"TZ": "Europe/London", "FOO": "bar"}}
		}},
		{name: "inline config env only", ac: func() *store.AgentAppliedConfig {
			return &store.AgentAppliedConfig{HarnessConfig: "claude", InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Europe/London"}}}
		}},
	}
	for _, tt := range tests {
		for _, path := range []string{"create", "start", "restart"} {
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				f := newTZDispatchFixture(t, "Asia/Tokyo")
				f.agent.AppliedConfig = tt.ac()
				var got string
				switch path {
				case "create":
					got = f.createTZ(t)
				case "start":
					got = f.startTZ(t)
				case "restart":
					got = f.restartTZ(t)
				}
				assert.Equal(t, "Europe/London", got)
				ac := f.agent.AppliedConfig
				assert.Equal(t, "Europe/London", ac.ExplicitTimezone)
				assert.True(t, ac.ExplicitTimezoneLegacy)
				assert.Empty(t, legacyEnvTZ(ac), "the env records no longer carry TZ")
				assert.Equal(t, TZSourceLegacy, f.d.resolveAgentTZ(context.Background(), f.agent, false).Source)
			})
		}
	}
}

// TestAgentTZDispatch_TZSecretsDropped checks a secret targeting TZ never
// reaches the broker and is reported as a dispatch warning (I3).
func TestAgentTZDispatch_TZSecretsDropped(t *testing.T) {
	f := newTZDispatchFixture(t, "Asia/Tokyo")
	f.d.SetSecretBackend(&mockSecretBackend{secrets: []secret.SecretWithValue{
		{SecretMeta: secret.SecretMeta{Name: "tz-secret", SecretType: "environment", Target: "TZ", Scope: "user", ScopeID: f.agent.OwnerID, InjectionMode: "always"}, Value: "America/Denver"},
		{SecretMeta: secret.SecretMeta{Name: "other-secret", SecretType: "environment", Target: "OTHER", Scope: "user", ScopeID: f.agent.OwnerID, InjectionMode: "always"}, Value: "x"},
	}})
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.d.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	req := f.client.lastCreateReq
	assert.Equal(t, "Asia/Tokyo", req.ResolvedEnv["TZ"])
	var targets []string
	for _, rs := range req.ResolvedSecrets {
		targets = append(targets, rs.Target)
	}
	assert.NotContains(t, targets, "TZ")
	assert.Contains(t, targets, "OTHER")
	require.Len(t, warns.Warnings(), 1)
	assert.Contains(t, warns.Warnings()[0], `"tz-secret"`)

	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	assert.Equal(t, "Asia/Tokyo", f.client.lastResolvedEnv["TZ"], "start drops the TZ secret as well")
}

// TestAgentTZDispatch_NotPersisted checks TZ is never written back to the
// agent's env records by the resolved-env persistence step (I2).
func TestAgentTZDispatch_NotPersisted(t *testing.T) {
	assert.False(t, shouldPersistResolvedEnvKey("TZ", map[string]api.EnvKind{"TZ": api.EnvKindPlain}))
	f := newTZDispatchFixture(t, "Asia/Tokyo")
	f.seedEnv(t, store.ScopeUser, f.agent.OwnerID, "Europe/Paris", "")
	assert.Equal(t, "Europe/Paris", f.createTZ(t))
	assert.Empty(t, legacyEnvTZ(f.agent.AppliedConfig))
	assert.Empty(t, f.agent.AppliedConfig.ExplicitTimezone, "a storage TZ is never captured as a pin at dispatch")
}

// TestAgentTZDispatch_EnvSourcesLabel checks the TZ source shown for the
// dispatched env is the resolver's rung.
func TestAgentTZDispatch_EnvSourcesLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  string
		want string
	}{
		{name: "hub default", want: TZSourceHubDefault},
		{name: "explicit", pin: "UTC", want: TZSourceExplicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTZDispatchFixture(t, "Asia/Tokyo")
			f.agent.AppliedConfig.ExplicitTimezone = tc.pin
			_, err := f.d.DispatchAgentCreateWithGather(context.Background(), f.agent)
			require.NoError(t, err)
			assert.Equal(t, tc.want, f.client.lastCreateReq.EnvSources["TZ"])
		})
	}
}

// oldBrokerReportingTZ fakes a broker from before this change: it reports
// TZ as an unmet env-gather need until the request carries a TZ, and it
// records every create request it receives.
func oldBrokerReportingTZ(reqs *[]*RemoteCreateAgentRequest) func(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		*reqs = append(*reqs, req)
		if req.ResolvedEnv["TZ"] == "" {
			return nil, &RemoteEnvRequirementsResponse{
				AgentID:      req.ID,
				Required:     []string{"TZ"},
				Needs:        []string{"TZ"},
				SecretInfo:   map[string]SecretKeyInfo{"TZ": {Description: "timezone"}},
				Alternatives: map[string][]string{"TZ": {"TIMEZONE"}},
			}, nil
		}
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: "running"}, Created: true}, nil, nil
	}
}

// TestAgentTZDispatch_NoLaunderViaOldBrokerGather is the no-launder test:
// an old broker that reports TZ as needed must never turn a user as_needed
// TZ or a TZ-targeted secret into the agent's zone or its pin, the CLI is
// never asked for TZ, and a TZ submitted later is ignored (I4).
func TestAgentTZDispatch_NoLaunderViaOldBrokerGather(t *testing.T) {
	for _, hubDefault := range []string{"Asia/Kathmandu", ""} {
		name := "hub default " + hubDefault
		want := hubDefault
		if hubDefault == "" {
			name = "no rungs"
			want = "UTC"
		}
		t.Run(name, func(t *testing.T) {
			f := newTZDispatchFixture(t, hubDefault)
			f.seedEnv(t, store.ScopeUser, f.agent.OwnerID, "America/Denver", store.InjectionModeAsNeeded)
			f.d.SetSecretBackend(&mockSecretBackend{secrets: []secret.SecretWithValue{
				{SecretMeta: secret.SecretMeta{Name: "tz-secret-asneeded", SecretType: "environment", Target: "TZ", Scope: "user", ScopeID: f.agent.OwnerID, InjectionMode: "as_needed"}, Value: "America/Denver"},
				{SecretMeta: secret.SecretMeta{Name: "tz-secret-always", SecretType: "environment", Target: "TZ", Scope: "user", ScopeID: f.agent.OwnerID, InjectionMode: "always"}, Value: "America/Denver"},
			}})
			var reqs []*RemoteCreateAgentRequest
			f.client.createWithGatherFunc = oldBrokerReportingTZ(&reqs)
			ctx := context.Background()

			res, err := f.d.DispatchAgentCreateWithGather(ctx, f.agent)
			require.NoError(t, err)
			assert.Nil(t, res.EnvRequirements(), "the CLI must never be asked for TZ")
			require.NotEmpty(t, reqs)
			if hubDefault == "" {
				// The first request carries no TZ, so the old broker asks;
				// the hub answers with a replay instead of the CLI.
				require.GreaterOrEqual(t, len(reqs), 2, "the hub answers the old broker's TZ need with a replay")
			}
			last := reqs[len(reqs)-1]
			assert.Equal(t, want, last.ResolvedEnv["TZ"], "the container gets the chain's zone, never the as_needed or secret value")
			for _, r := range reqs {
				assert.NotEqual(t, "America/Denver", r.ResolvedEnv["TZ"])
				for _, rs := range r.ResolvedSecrets {
					assert.NotEqual(t, "TZ", rs.Target, "a TZ-targeted secret reached the broker")
				}
			}
			assert.Empty(t, f.agent.AppliedConfig.ExplicitTimezone, "the gather answer must not become a pin")
			assert.Empty(t, legacyEnvTZ(f.agent.AppliedConfig))

			// A later start resolves the same zone (the UTC gather answer is
			// only for the old broker; a start with no rungs sends no TZ).
			startWant := hubDefault
			assert.Equal(t, startWant, f.startTZ(t))

			// A submitted TZ (e.g. a CLI or reconcile replay) is ignored.
			reqs = nil
			ctx, warns := withDispatchWarnings(ctx)
			_, err = f.d.DispatchFinalizeEnv(ctx, f.agent, map[string]string{"TZ": "Europe/Berlin"})
			require.NoError(t, err)
			assert.Empty(t, f.agent.AppliedConfig.ExplicitTimezone)
			for _, r := range reqs {
				assert.NotEqual(t, "Europe/Berlin", r.ResolvedEnv["TZ"])
			}
			assert.Equal(t, want, reqs[len(reqs)-1].ResolvedEnv["TZ"])
			joined := strings.Join(warns.Warnings(), "\n")
			assert.Contains(t, joined, "TZ in submitted env is ignored")
		})
	}
}

// TestTakeTZGatherNeed checks TZ leaves the lists the CLI auto-fills and
// stays in the informational ones.
func TestTakeTZGatherNeed(t *testing.T) {
	assert.False(t, takeTZGatherNeed(nil))
	reqs := &RemoteEnvRequirementsResponse{
		Required:     []string{"TZ", "API_KEY"},
		HubHas:       []string{"TZ"},
		BrokerHas:    []string{"TZ"},
		Needs:        []string{"API_KEY", "TZ"},
		SecretInfo:   map[string]SecretKeyInfo{"TZ": {}, "API_KEY": {}},
		Alternatives: map[string][]string{"TZ": {"TIMEZONE"}, "API_KEY": {"TZ", "ALT_KEY"}},
	}
	assert.True(t, takeTZGatherNeed(reqs))
	assert.Equal(t, []string{"API_KEY"}, reqs.Needs)
	assert.NotContains(t, reqs.SecretInfo, "TZ")
	assert.NotContains(t, reqs.Alternatives, "TZ")
	assert.Equal(t, []string{"ALT_KEY"}, reqs.Alternatives["API_KEY"])
	assert.Equal(t, []string{"TZ", "API_KEY"}, reqs.Required)
	assert.Equal(t, []string{"TZ"}, reqs.HubHas)
	assert.Equal(t, []string{"TZ"}, reqs.BrokerHas)
	assert.False(t, takeTZGatherNeed(reqs), "a second call finds no TZ need")
}

// TestBrokerWarningsRelayedOnCreate checks hub-only env drop warnings a
// broker returns on create reach the dispatch warnings collector.
func TestBrokerWarningsRelayedOnCreate(t *testing.T) {
	f := newTZDispatchFixture(t, "")
	f.client.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Phase: "running", Warnings: []string{"broker dropped TZ"}}, Created: true}, nil, nil
	}
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.d.DispatchAgentCreateWithGather(ctx, f.agent)
	require.NoError(t, err)
	assert.Equal(t, []string{"broker dropped TZ"}, warns.Warnings())

	f.client.startReturnResp = &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: f.agent.ID, Phase: "running", Warnings: []string{"broker dropped TZ on start"}}}
	ctx, warns = withDispatchWarnings(context.Background())
	require.NoError(t, f.d.DispatchAgentStart(ctx, f.agent, "", false))
	assert.Equal(t, []string{"broker dropped TZ on start"}, warns.Warnings())
}

// TestDispatchWarnings checks the collector dedups, skips empties and is a
// no-op without one on the context.
func TestDispatchWarnings(t *testing.T) {
	addDispatchWarnings(context.Background(), "ignored")
	var nilW *dispatchWarnings
	assert.Nil(t, nilW.Warnings())

	ctx, w := withDispatchWarnings(context.Background())
	addDispatchWarnings(ctx, "a", "", "b", "a")
	assert.Equal(t, []string{"a", "b"}, w.Warnings())
}
