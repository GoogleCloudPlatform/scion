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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The env-gather hubHas labels follow the dispatch merge: a non-empty config
// value, then the env var storage scopes from envScopePrecedence (highest
// first, broker included), then user/project secrets, then the "hub"
// fallback.

const (
	provenanceHubID    = "test-hub-id" // testServer's hub ID
	provenanceOwner    = "prov-owner"
	provenanceProject  = "prov-project"
	provenanceBrokerID = "prov-broker"
)

func provenanceAgent(env map[string]string) *store.Agent {
	a := &store.Agent{
		ID:              "agent-provenance",
		Name:            "provenance-agent",
		OwnerID:         provenanceOwner,
		ProjectID:       provenanceProject,
		RuntimeBrokerID: provenanceBrokerID,
	}
	if env != nil {
		a.AppliedConfig = &store.AgentAppliedConfig{Env: env}
	}
	return a
}

func provenanceScopeID(scope string) string {
	switch scope {
	case store.ScopeUser:
		return provenanceOwner
	case store.ScopeProject:
		return provenanceProject
	case store.ScopeHub:
		return provenanceHubID
	case store.ScopeRuntimeBroker:
		return provenanceBrokerID
	}
	return ""
}

func putProvenanceEnv(t *testing.T, st store.Store, key, scope, mode string) {
	t.Helper()
	if err := st.CreateEnvVar(context.Background(), &store.EnvVar{
		ID:            tid("env-prov-" + scope + "-" + key),
		Key:           key,
		Value:         scope + "-value",
		Scope:         scope,
		ScopeID:       provenanceScopeID(scope),
		InjectionMode: mode,
	}); err != nil {
		t.Fatalf("create %s env var %s: %v", scope, key, err)
	}
}

func hubHasLabel(t *testing.T, resp *EnvGatherResponse, key string) string {
	t.Helper()
	for _, src := range resp.HubHas {
		if src.Key == key {
			return src.Scope
		}
	}
	t.Fatalf("key %s missing from hubHas %+v", key, resp.HubHas)
	return ""
}

func TestBuildEnvGatherResponse_ProvenanceFollowsDispatch(t *testing.T) {
	always := store.InjectionModeAlways
	tests := []struct {
		name   string
		scopes []string          // env var scopes holding the key (always)
		config map[string]string // agent applied config env
		secret string            // scope of a secret of that name ("" for none)
		want   string
	}{
		{name: "broker only", scopes: []string{store.ScopeRuntimeBroker}, want: "broker"},
		{name: "hub only", scopes: []string{store.ScopeHub}, want: "hub"},
		{name: "project beats broker", scopes: []string{store.ScopeRuntimeBroker, store.ScopeProject}, want: "project"},
		{name: "hub beats broker", scopes: []string{store.ScopeRuntimeBroker, store.ScopeHub}, want: "hub"},
		{name: "user beats project", scopes: []string{store.ScopeProject, store.ScopeUser}, want: "user"},
		{name: "project beats hub", scopes: []string{store.ScopeHub, store.ScopeProject}, want: "project"},
		{name: "non-empty config beats user", scopes: []string{store.ScopeUser}, config: map[string]string{"K": "cfg"}, want: "config"},
		{name: "empty config yields to project", scopes: []string{store.ScopeProject}, config: map[string]string{"K": ""}, want: "project"},
		{name: "empty config with nothing stored falls back to hub", config: map[string]string{"K": ""}, want: "hub"},
		{name: "env storage beats secret", scopes: []string{store.ScopeRuntimeBroker}, secret: store.ScopeUser, want: "broker"},
		{name: "user secret only", secret: store.ScopeUser, want: "secret"},
		{name: "project secret only", secret: store.ScopeProject, want: "secret"},
		{name: "nothing stored", want: "hub"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := testServer(t)
			ctx := context.Background()
			for _, scope := range tc.scopes {
				putProvenanceEnv(t, st, "K", scope, always)
			}
			if tc.secret != "" {
				if err := st.CreateSecret(ctx, &store.Secret{
					ID:             tid("sec-prov-K"),
					Key:            "K",
					EncryptedValue: "encrypted",
					SecretType:     store.SecretTypeEnvironment,
					Target:         "K",
					Scope:          tc.secret,
					ScopeID:        provenanceScopeID(tc.secret),
				}); err != nil {
					t.Fatal(err)
				}
				srv.SetSecretBackend(secret.NewLocalBackend(st, provenanceHubID, "test-secret"))
			}
			resp := srv.buildEnvGatherResponse(ctx, provenanceAgent(tc.config), &RemoteEnvRequirementsResponse{
				Required: []string{"K"},
				HubHas:   []string{"K"},
			})
			if got := hubHasLabel(t, resp, "K"); got != tc.want {
				t.Errorf("hubHas label = %q, want %q", got, tc.want)
			}
		})
	}
}

// An as_needed row is not part of the first dispatch, so it does not win the
// hubHas label over a lower scope that was dispatched.
func TestBuildEnvGatherResponse_ProvenanceSkipsAsNeeded(t *testing.T) {
	srv, st := testServer(t)
	putProvenanceEnv(t, st, "K", store.ScopeUser, store.InjectionModeAsNeeded)
	putProvenanceEnv(t, st, "K", store.ScopeRuntimeBroker, store.InjectionModeAlways)
	resp := srv.buildEnvGatherResponse(context.Background(), provenanceAgent(nil), &RemoteEnvRequirementsResponse{
		Required: []string{"K"},
		HubHas:   []string{"K"},
	})
	if got := hubHasLabel(t, resp, "K"); got != "broker" {
		t.Errorf("hubHas label = %q, want %q", got, "broker")
	}
}

// The needs cross-check walks the same scopes, so a broker-scope key the
// dispatch left out (typically as_needed) now gets a warning naming that
// scope, and the highest scope holding the key is the one named.
func TestBuildEnvGatherResponse_NeedsWarningCoversBrokerScope(t *testing.T) {
	srv, st := testServer(t)
	putProvenanceEnv(t, st, "BROKER_ONLY", store.ScopeRuntimeBroker, store.InjectionModeAsNeeded)
	putProvenanceEnv(t, st, "TWO_SCOPES", store.ScopeRuntimeBroker, store.InjectionModeAsNeeded)
	putProvenanceEnv(t, st, "TWO_SCOPES", store.ScopeProject, store.InjectionModeAsNeeded)
	resp := srv.buildEnvGatherResponse(context.Background(), provenanceAgent(nil), &RemoteEnvRequirementsResponse{
		Required: []string{"BROKER_ONLY", "TWO_SCOPES"},
		Needs:    []string{"BROKER_ONLY", "TWO_SCOPES"},
	})
	if len(resp.HubWarnings) != 2 {
		t.Fatalf("HubWarnings = %v, want one per needed key", resp.HubWarnings)
	}
	if w := resp.HubWarnings[0]; !strings.Contains(w, "BROKER_ONLY") || !strings.Contains(w, "(runtime_broker scope)") || !strings.Contains(w, "as_needed") {
		t.Errorf("broker warning = %q, want BROKER_ONLY at runtime_broker scope with the as_needed hint", w)
	}
	if w := resp.HubWarnings[1]; !strings.Contains(w, "TWO_SCOPES") || !strings.Contains(w, "(project scope)") {
		t.Errorf("two-scope warning = %q, want TWO_SCOPES at project scope", w)
	}
}

// envScopeFilters is the list both the dispatcher and the env-gather
// reporter walk: it must be envScopePrecedence itself, with each scope keyed
// as the dispatcher keys it.
func TestEnvScopeFilters_MatchesPrecedenceAndDispatcher(t *testing.T) {
	agent := provenanceAgent(nil)
	got := envScopeFilters(agent, provenanceHubID)
	var scopes []string
	for _, f := range got {
		scopes = append(scopes, f.Scope)
		if want := provenanceScopeID(f.Scope); f.ScopeID != want {
			t.Errorf("scope %s keyed by %q, want %q", f.Scope, f.ScopeID, want)
		}
	}
	if !slices.Equal(scopes, envScopePrecedence) {
		t.Errorf("envScopeFilters scopes = %v, want envScopePrecedence %v", scopes, envScopePrecedence)
	}

	d := &HTTPAgentDispatcher{hubID: provenanceHubID}
	if fromDispatcher := d.envScopesInPrecedenceOrder(agent); !slices.Equal(fromDispatcher, got) {
		t.Errorf("dispatcher filters %v differ from envScopeFilters %v", fromDispatcher, got)
	}

	// Scopes with no ID on the agent are omitted, except hub.
	bare := &store.Agent{ID: "bare"}
	if got := envScopeFilters(bare, ""); len(got) != 1 || got[0].Scope != store.ScopeHub {
		t.Errorf("envScopeFilters(no IDs) = %v, want only the hub scope", got)
	}
}
