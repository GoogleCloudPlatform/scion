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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	hsdTelemetryName   = "scion-telemetry-gcp-credentials"
	hsdTelemetryTarget = "~/.scion/telemetry-gcp-credentials.json"
)

// seedHubScopeDeliveryMix stores one secret of each hub-scope delivery
// class, a broker-scope row shadowed today by an excluded hub row, and
// project-scope rows of every type.
func seedHubScopeDeliveryMix(t *testing.T, m *ldFixture) {
	t.Helper()
	ctx := context.Background()
	set := func(name, typ, target, mode, scope string) {
		t.Helper()
		_, _, err := m.backend.Set(ctx, &secret.SetSecretInput{
			Name: name, Value: "v-" + scope + "-" + name, SecretType: typ, Target: target,
			Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: mode, CreatedBy: m.projectOwnerID,
		})
		require.NoError(t, err)
	}
	always, asNeeded := store.InjectionModeAlways, store.InjectionModeAsNeeded
	set("HSD_HUB_ENV_ALWAYS", store.SecretTypeEnvironment, "HSD_HUB_ENV_ALWAYS", always, store.ScopeHub)
	set("HSD_HUB_ENV_AS_NEEDED", store.SecretTypeEnvironment, "HSD_HUB_ENV_AS_NEEDED", asNeeded, store.ScopeHub)
	set("HSD_HUB_VARIABLE", store.SecretTypeVariable, "HSD_HUB_VARIABLE", always, store.ScopeHub)
	set("HSD_HUB_FILE", store.SecretTypeFile, "/etc/hsd/hub-file", always, store.ScopeHub)
	set("HSD_HUB_INTERNAL", store.SecretTypeInternal, "HSD_HUB_INTERNAL", always, store.ScopeHub)
	set(hsdTelemetryName, store.SecretTypeFile, hsdTelemetryTarget, always, store.ScopeHub)
	set("HSD_SHADOWED", store.SecretTypeVariable, "HSD_SHADOWED", always, store.ScopeHub)
	set("HSD_SHADOWED", store.SecretTypeVariable, "HSD_SHADOWED", always, store.ScopeRuntimeBroker)
	set("HSD_PROJECT_ENV", store.SecretTypeEnvironment, "HSD_PROJECT_ENV", always, store.ScopeProject)
	set("HSD_PROJECT_VARIABLE", store.SecretTypeVariable, "HSD_PROJECT_VARIABLE", always, store.ScopeProject)
	set("HSD_PROJECT_FILE", store.SecretTypeFile, "/etc/hsd/project-file", always, store.ScopeProject)
}

// assertHubScopeDeliveryMix checks a delivered secret list against
// seedHubScopeDeliveryMix.
func assertHubScopeDeliveryMix(t *testing.T, got []ResolvedSecret) {
	t.Helper()
	bySource := make(map[string]string, len(got))
	for _, s := range got {
		bySource[s.Name] = s.Source
	}
	want := map[string]string{
		"HSD_HUB_ENV_ALWAYS":   store.ScopeHub,
		hsdTelemetryName:       store.ScopeHub,
		"HSD_SHADOWED":         store.ScopeRuntimeBroker,
		"HSD_PROJECT_ENV":      store.ScopeProject,
		"HSD_PROJECT_VARIABLE": store.ScopeProject,
		"HSD_PROJECT_FILE":     store.ScopeProject,
	}
	assert.Equal(t, want, bySource, "delivered secrets by source scope")
	for _, s := range got {
		if s.Name == "HSD_SHADOWED" {
			assert.Equal(t, "v-"+store.ScopeRuntimeBroker+"-HSD_SHADOWED", s.Value)
		}
		if s.Name == hsdTelemetryName {
			assert.Equal(t, store.SecretTypeFile, s.Type)
			assert.Equal(t, hsdTelemetryTarget, s.Target)
		}
	}
}

// TestHubSecretDeliveryScope_CreateStartRestart: agent create (Resolve) and
// start and restart (the delivery selection) deliver the same hub-scope
// secrets: always-mode environment secrets and the telemetry credential
// file only. An excluded hub row no longer shadows the broker row of the
// same name, and project-scope delivery is unchanged.
func TestHubSecretDeliveryScope_CreateStartRestart(t *testing.T) {
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		m := newLDFixture(t, "hsd-create", ldOptions{})
		seedHubScopeDeliveryMix(t, m)
		got, _, err := m.d.resolveSecrets(ctx, m.agent)
		require.NoError(t, err)
		assertHubScopeDeliveryMix(t, got)
	})

	t.Run("selection", func(t *testing.T) {
		m := newLDFixture(t, "hsd-select", ldOptions{})
		seedHubScopeDeliveryMix(t, m)
		got, _, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
		require.NoError(t, err)
		assertHubScopeDeliveryMix(t, got)
	})

	t.Run("restart", func(t *testing.T) {
		m := newLDFixture(t, "hsd-restart", ldOptions{})
		seedHubScopeDeliveryMix(t, m)
		require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
		require.True(t, m.client.restartCalled)
		env := m.client.lastRestartResolvedEnv
		assert.Equal(t, "v-hub-HSD_HUB_ENV_ALWAYS", env["HSD_HUB_ENV_ALWAYS"])
		assert.NotContains(t, env, "HSD_HUB_ENV_AS_NEEDED")
		assert.NotContains(t, env, "HSD_HUB_INTERNAL")
		assertHubScopeDeliveryMix(t, m.client.lastRestartExtras.ResolvedSecrets)
	})
}

// TestHubSecretDeliveryScope_AsNeededPassExcludesHub: the second env pass no
// longer satisfies a needed key from an as-needed hub-scope secret, while an
// as-needed project-scope secret still does.
func TestHubSecretDeliveryScope_AsNeededPassExcludesHub(t *testing.T) {
	m := newLDFixture(t, "hsd-as-needed", ldOptions{})
	ctx := context.Background()
	for _, scope := range []string{store.ScopeHub, store.ScopeProject} {
		_, _, err := m.backend.Set(ctx, &secret.SetSecretInput{
			Name: "HSD_NEED_" + scope, Value: "v-" + scope, SecretType: store.SecretTypeEnvironment,
			Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: store.InjectionModeAsNeeded, CreatedBy: m.projectOwnerID,
		})
		require.NoError(t, err)
	}
	got, err := m.d.resolveAsNeededForKeys(ctx, m.agent, []string{"HSD_NEED_hub", "HSD_NEED_project"}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"HSD_NEED_project": "v-project"}, got)
}
