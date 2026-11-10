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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Launch material for an agent without credentials (noauth_delivery.go):
// start, resume and restart deliver what create delivers. The fixture is an
// ldFixture agent (delivery-eligible, owned by the project owner) with
// test values ("canaries") in every secret class and scope and in stored
// settings, plus the git token at project and user scope.

const (
	naGitProject = "na-git-token-project"
	naGitUser    = "na-git-token-user"
	naPlainKey   = "NA_PLAIN_SETTING"
	naPlainValue = "na-plain-setting-value"
	// A project setting on the non-secret list: kept for agents without
	// credentials.
	naProjectSettingValue = "na-gcp-project-value"
)

// naCanary is a stored item an agent without credentials must not receive.
type naCanary struct {
	key, value string
}

// naFixture is the canary fixture.
type naFixture struct {
	*ldFixture
	canaries []naCanary
}

// naSetSecret stores a secret through the fixture's local backend.
func naSetSecret(t *testing.T, m *ldFixture, name, value, secretType, scope string, allowProgeny bool) {
	t.Helper()
	_, _, err := m.backend.Set(context.Background(), &secret.SetSecretInput{
		Name: name, Value: value, SecretType: secretType, Target: name,
		Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: store.InjectionModeAlways,
		AllowProgeny: allowProgeny, CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)
}

// newNAFixture builds the canary fixture. noAuth sets the agent's
// no-credential intent; the same fixture without it is the control. Each
// mutate adjusts the agent's applied config before it is stored.
func newNAFixture(t *testing.T, name string, noAuth bool, opts ldOptions, mutate ...func(ac *store.AgentAppliedConfig)) *naFixture {
	t.Helper()
	m := newLDFixture(t, name, opts)
	f := &naFixture{ldFixture: m}
	add := func(key, value string) {
		f.canaries = append(f.canaries, naCanary{key, value})
	}
	scopes := []string{store.ScopeRuntimeBroker, store.ScopeHub, store.ScopeProject, store.ScopeUser}
	for _, scope := range scopes {
		up := strings.ToUpper(scope)
		// Environment and file secrets at every scope.
		k := "NA_ENV_SECRET_" + up
		naSetSecret(t, m, k, "na-canary-env-"+scope, store.SecretTypeEnvironment, scope, false)
		add(k, "na-canary-env-"+scope)
		k = "NA_FILE_SECRET_" + up
		naSetSecret(t, m, k, "na-canary-file-"+scope, store.SecretTypeFile, scope, false)
		add(k, "na-canary-file-"+scope)
		// A stored setting marked secret at every scope.
		k = "NA_SECRET_SETTING_" + up
		m.env(t, k, "na-canary-setting-"+scope, scope, m.scopeID(scope), true)
		add(k, "na-canary-setting-"+scope)
	}
	// Variable secrets at project and user scope.
	for _, scope := range []string{store.ScopeProject, store.ScopeUser} {
		k := "NA_VAR_SECRET_" + strings.ToUpper(scope)
		naSetSecret(t, m, k, "na-canary-var-"+scope, store.SecretTypeVariable, scope, false)
		add(k, "na-canary-var-"+scope)
	}
	// The owner's user-scope rows shared with progeny, too.
	naSetSecret(t, m, "NA_ENV_SECRET_USER_SHARED", "na-canary-env-user-shared", store.SecretTypeEnvironment, store.ScopeUser, true)
	add("NA_ENV_SECRET_USER_SHARED", "na-canary-env-user-shared")
	// Plain settings named like a harness credential (built-in names).
	m.env(t, "ANTHROPIC_API_KEY", "na-canary-plain-anthropic", store.ScopeProject, m.agent.ProjectID, false)
	add("ANTHROPIC_API_KEY", "na-canary-plain-anthropic")
	m.env(t, "COPILOT_GITHUB_TOKEN", "na-canary-plain-copilot", store.ScopeUser, m.agent.OwnerID, false)
	add("COPILOT_GITHUB_TOKEN", "na-canary-plain-copilot")
	m.env(t, "GH_TOKEN", "na-canary-plain-gh", store.ScopeHub, m.hubID, false)
	add("GH_TOKEN", "na-canary-plain-gh")

	// Kept: a plain setting with an ordinary name, a project setting on
	// the non-secret list, and the git token at project and user scope.
	m.env(t, naPlainKey, naPlainValue, store.ScopeProject, m.agent.ProjectID, false)
	m.env(t, "GOOGLE_CLOUD_PROJECT", naProjectSettingValue, store.ScopeProject, m.agent.ProjectID, false)
	naSetSecret(t, m, noAuthGitTokenKey, naGitProject, store.SecretTypeEnvironment, store.ScopeProject, false)
	naSetSecret(t, m, noAuthGitTokenKey, naGitUser, store.SecretTypeEnvironment, store.ScopeUser, false)

	m.agent.AppliedConfig.NoAuth = noAuth
	for _, fn := range mutate {
		fn(m.agent.AppliedConfig)
	}
	require.NoError(t, m.store.UpdateAgent(context.Background(), m.agent))
	return f
}

// naSent is what one start, resume or restart sent the broker.
type naSent struct {
	env     map[string]string
	secrets []ResolvedSecret
	extras  StartExtras
}

type naOp struct {
	name string
	run  func(t *testing.T, m *ldFixture) naSent
}

var naOps = []naOp{
	{"start", func(t *testing.T, m *ldFixture) naSent {
		require.NoError(t, m.d.DispatchAgentStart(context.Background(), m.agent, "task", false))
		require.True(t, m.client.startCalled)
		return naSent{m.client.lastResolvedEnv, m.client.lastStartResolvedSecrets, m.client.lastStartExtras}
	}},
	{"resume", func(t *testing.T, m *ldFixture) naSent {
		require.NoError(t, m.d.DispatchAgentStart(context.Background(), m.agent, "", true))
		require.True(t, m.client.startCalled)
		return naSent{m.client.lastResolvedEnv, m.client.lastStartResolvedSecrets, m.client.lastStartExtras}
	}},
	{"restart", func(t *testing.T, m *ldFixture) naSent {
		require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
		require.True(t, m.client.restartCalled)
		return naSent{m.client.lastRestartResolvedEnv, m.client.lastRestartExtras.ResolvedSecrets, m.client.lastRestartExtras}
	}},
}

// assertNoCanary checks no canary value reached the broker: not in the env,
// not in a resolved secret, and not anywhere in the extras.
func assertNoCanary(t *testing.T, sent naSent, canaries []naCanary) {
	t.Helper()
	extrasJSON, err := json.Marshal(sent.extras)
	require.NoError(t, err)
	for _, c := range canaries {
		assert.NotContains(t, sent.env, c.key, "%s reached the env", c.key)
		for k, v := range sent.env {
			assert.NotEqual(t, c.value, v, "the value of %s reached the env as %s", c.key, k)
		}
		for _, s := range sent.secrets {
			assert.NotEqual(t, c.value, s.Value, "the value of %s reached the resolved secrets", c.key)
			assert.NotEqual(t, c.key, s.Name, "%s reached the resolved secrets", c.key)
		}
		assert.NotContains(t, string(extrasJSON), c.value, "the value of %s reached the extras", c.key)
	}
}

// TestStartNoAuth_DeliversNoStoredSecrets (T1, T2, T3, T5): start, resume
// and restart of an agent without credentials send no canary, no resolved
// secret, the project's git token (over the owner's), the plain settings,
// and the no-credential intent.
func TestStartNoAuth_DeliversNoStoredSecrets(t *testing.T) {
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			f := newNAFixture(t, "na-"+op.name, true, ldOptions{})
			sent := op.run(t, f.ldFixture)

			assertNoCanary(t, sent, f.canaries)
			assert.Empty(t, sent.secrets, "no resolved secret is sent")
			assert.Equal(t, naGitProject, sent.env[noAuthGitTokenKey], "the project's git token wins")
			assert.Equal(t, api.EnvKindSecretFetchable, sent.extras.EnvClassifications[noAuthGitTokenKey])
			assert.Equal(t, naPlainValue, sent.env[naPlainKey], "plain settings still apply")
			assert.Equal(t, naProjectSettingValue, sent.env["GOOGLE_CLOUD_PROJECT"], "a project setting still applies")
			assert.True(t, sent.extras.NoAuth, "the start carries the no-credential intent")
		})
	}
}

// TestStartNoAuth_OwnerScopeOnly (T1b, the owner-scope mutant's killer):
// with canaries only in the owner's user scope, not shared with progeny
// (the rows the owner-path check always passes), none reaches the broker.
func TestStartNoAuth_OwnerScopeOnly(t *testing.T) {
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			m := newLDFixture(t, "na-owner-"+op.name, ldOptions{})
			var canaries []naCanary
			for _, typ := range []string{store.SecretTypeEnvironment, store.SecretTypeFile, store.SecretTypeVariable} {
				k := "NA_OWNER_" + strings.ToUpper(typ)
				v := "na-canary-owner-" + typ
				naSetSecret(t, m, k, v, typ, store.ScopeUser, false)
				canaries = append(canaries, naCanary{k, v})
			}
			m.agent.AppliedConfig.NoAuth = true
			require.NoError(t, m.store.UpdateAgent(context.Background(), m.agent))

			sent := op.run(t, m)
			assertNoCanary(t, sent, canaries)
			assert.Empty(t, sent.secrets)
			assert.NotContains(t, sent.env, noAuthGitTokenKey, "no git token is stored")
		})
	}
}

// TestStartWithAuth_Unchanged (T11): the same fixture for an agent with
// credentials delivers every canary as before, and no intent is sent.
func TestStartWithAuth_Unchanged(t *testing.T) {
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			f := newNAFixture(t, "na-auth-"+op.name, false, ldOptions{})
			sent := op.run(t, f.ldFixture)

			assert.False(t, sent.extras.NoAuth)
			delivered := map[string]string{}
			for k, v := range sent.env {
				delivered[k] = v
			}
			for _, s := range sent.secrets {
				delivered[s.Name] = s.Value
			}
			for _, c := range f.canaries {
				if c.key == "NA_ENV_SECRET_USER_SHARED" {
					continue // decided by the owner-path chain check, not by this change
				}
				assert.Equal(t, c.value, delivered[c.key], "%s is delivered to an agent with credentials", c.key)
			}
			// The normal order: the owner's token outranks the project's.
			assert.Equal(t, naGitUser, sent.env[noAuthGitTokenKey])
		})
	}
}

// TestStartNoAuth_GitTokenFallthrough (T6): the owner's token stands in
// when the project has none, when the project's token cannot be read (the
// skip is reported), and when the project's token is not authorized for
// delivery.
func TestStartNoAuth_GitTokenFallthrough(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, name string, opts ldOptions, withProject bool) *ldFixture {
		m := newLDFixture(t, name, opts)
		if withProject {
			naSetSecret(t, m, noAuthGitTokenKey, naGitProject, store.SecretTypeEnvironment, store.ScopeProject, false)
		}
		naSetSecret(t, m, noAuthGitTokenKey, naGitUser, store.SecretTypeEnvironment, store.ScopeUser, false)
		m.agent.AppliedConfig.NoAuth = true
		require.NoError(t, m.store.UpdateAgent(ctx, m.agent))
		return m
	}

	t.Run("no project token", func(t *testing.T) {
		m := setup(t, "na-ft-none", ldOptions{}, false)
		require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
		assert.Equal(t, naGitUser, m.client.lastRestartResolvedEnv[noAuthGitTokenKey])
	})

	t.Run("project token unreadable", func(t *testing.T) {
		m := setup(t, "na-ft-read", ldOptions{}, true)
		m.d.SetSecretBackend(&naScopeFaultBackend{SecretBackend: m.backend, faultScope: store.ScopeProject})
		secrets, skipped, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
		require.NoError(t, err)
		require.Len(t, skipped, 1, "the unreadable project token is reported")
		assert.Equal(t, noAuthGitTokenKey, skipped[0].Name)
		assert.Equal(t, store.ScopeProject, skipped[0].Scope)
		assert.Equal(t, naGitUser, pickNoAuthGitTokenFrom(secrets).value)

		require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
		assert.Equal(t, naGitUser, m.client.lastRestartResolvedEnv[noAuthGitTokenKey])
	})

	t.Run("project token not authorized", func(t *testing.T) {
		m := setup(t, "na-ft-authz", ldOptions{edge: withBoundedCeiling("agent.read")}, true)
		require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
		assert.Equal(t, naGitUser, m.client.lastRestartResolvedEnv[noAuthGitTokenKey])
	})
}

// naScopeFaultBackend fails the value read of every item of faultScope with
// a Secret Manager access error.
type naScopeFaultBackend struct {
	secret.SecretBackend
	faultScope string
}

func (b *naScopeFaultBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	var pass []secret.SecretMeta
	out := map[string]secret.FetchResult{}
	for _, m := range metas {
		if m.Scope == b.faultScope {
			out[m.ID] = secret.FetchResult{Err: fmt.Errorf("%w: na test", secret.ErrValueAccess)}
			continue
		}
		pass = append(pass, m)
	}
	if len(pass) > 0 {
		got, err := b.SecretBackend.FetchValues(ctx, pass)
		if err != nil {
			return nil, err
		}
		for id, r := range got {
			out[id] = r
		}
	}
	return out, nil
}

// naRecordingBackend records the names whose values were read.
type naRecordingBackend struct {
	secret.SecretBackend
	mu      sync.Mutex
	fetched []string
}

func (b *naRecordingBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.mu.Lock()
	for _, m := range metas {
		b.fetched = append(b.fetched, m.Name)
	}
	b.mu.Unlock()
	return b.SecretBackend.FetchValues(ctx, metas)
}

// TestStartNoAuth_NoValueFetchedForSuppressed (T10): only the git token
// candidates' values are read for an agent without credentials.
func TestStartNoAuth_NoValueFetchedForSuppressed(t *testing.T) {
	f := newNAFixture(t, "na-fetch", true, ldOptions{})
	rec := &naRecordingBackend{SecretBackend: f.backend}
	f.d.SetSecretBackend(rec)
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "task", false))
	require.NotEmpty(t, rec.fetched, "precondition: the git token was read")
	for _, name := range rec.fetched {
		assert.Equal(t, noAuthGitTokenKey, name, "no other value is read")
	}
}

// TestStartNoAuth_GitTokenOverwrites (T9): the picked git token replaces a
// GITHUB_TOKEN in the agent's config env, at create and at start alike.
func TestStartNoAuth_GitTokenOverwrites(t *testing.T) {
	ctx := context.Background()
	f := newNAFixture(t, "na-overwrite", true, ldOptions{}, func(ac *store.AgentAppliedConfig) {
		ac.Env = map[string]string{noAuthGitTokenKey: "na-config-git-token"}
	})

	req, err := f.d.buildCreateRequest(ctx, f.agent, "TestStartNoAuth_GitTokenOverwrites")
	require.NoError(t, err)
	assert.Equal(t, naGitProject, req.ResolvedEnv[noAuthGitTokenKey], "create")

	require.NoError(t, f.d.DispatchAgentStart(ctx, f.agent, "task", false))
	assert.Equal(t, naGitProject, f.client.lastResolvedEnv[noAuthGitTokenKey], "start")
}

// TestNoAuth_CreateStartParity (T8): for the canary fixture (including the
// credential-named plain settings and saved config env), create and start
// deliver the same value, or none, for every fixture key, and agree on
// whether the git token is the project's own.
func TestNoAuth_CreateStartParity(t *testing.T) {
	ctx := context.Background()
	// Saved config env: a credential-named key saved from an earlier run,
	// the same name set by the requester, and an ordinary key.
	f := newNAFixture(t, "na-parity", true, ldOptions{}, func(ac *store.AgentAppliedConfig) {
		ac.Env = map[string]string{
			"OPENAI_API_KEY":    "na-canary-saved-openai",
			"GEMINI_API_KEY":    "na-explicit-gemini",
			"NA_SAVED_ORDINARY": "na-saved-ordinary",
		}
		ac.InlineConfig = &api.ScionConfig{Env: map[string]string{"GEMINI_API_KEY": "na-explicit-gemini"}}
	})

	req, err := f.d.buildCreateRequest(ctx, f.agent, "TestNoAuth_CreateStartParity")
	require.NoError(t, err)
	require.NoError(t, f.d.DispatchAgentStart(ctx, f.agent, "task", false))
	start := f.client.lastResolvedEnv

	keys := []string{noAuthGitTokenKey, naPlainKey, "GOOGLE_CLOUD_PROJECT", "OPENAI_API_KEY", "GEMINI_API_KEY", "NA_SAVED_ORDINARY"}
	for _, c := range f.canaries {
		keys = append(keys, c.key)
	}
	for _, k := range keys {
		cv, cok := req.ResolvedEnv[k]
		sv, sok := start[k]
		assert.Equal(t, cok, sok, "%s: present at create %v, at start %v", k, cok, sok)
		assert.Equal(t, cv, sv, "%s: same value at create and start", k)
	}
	assert.Empty(t, req.ResolvedSecrets)
	assert.Empty(t, f.client.lastStartResolvedSecrets)
	assert.Equal(t, naGitProject, req.noAuthProjectToken, "create: the project's own token")
	assert.NotContains(t, start, "OPENAI_API_KEY", "a saved credential setting is not applied")
	assert.Equal(t, "na-explicit-gemini", start["GEMINI_API_KEY"], "a requester-set value still is")
}

// TestCreateNoAuth_SecretStoredEnvWithheld (T15): create leaves out stored
// settings marked secret for an agent without credentials and keeps plain
// ones and a stored git token setting.
func TestCreateNoAuth_SecretStoredEnvWithheld(t *testing.T) {
	ctx := context.Background()
	m := newLDFixture(t, "na-create-secret-setting", ldOptions{})
	m.env(t, "NA_SECRET_SETTING", "na-canary-secret-setting", store.ScopeProject, m.agent.ProjectID, true)
	m.env(t, naPlainKey, naPlainValue, store.ScopeProject, m.agent.ProjectID, false)
	m.env(t, noAuthGitTokenKey, "na-git-token-setting", store.ScopeUser, m.agent.OwnerID, true)
	m.agent.AppliedConfig.NoAuth = true

	req, err := m.d.buildCreateRequest(ctx, m.agent, "TestCreateNoAuth_SecretStoredEnvWithheld")
	require.NoError(t, err)
	assert.NotContains(t, req.ResolvedEnv, "NA_SECRET_SETTING")
	assert.Equal(t, naPlainValue, req.ResolvedEnv[naPlainKey])
	assert.Equal(t, "na-git-token-setting", req.ResolvedEnv[noAuthGitTokenKey], "a stored git token setting is kept")

	// With credentials, unchanged.
	m.agent.AppliedConfig.NoAuth = false
	req, err = m.d.buildCreateRequest(ctx, m.agent, "TestCreateNoAuth_SecretStoredEnvWithheld")
	require.NoError(t, err)
	assert.Equal(t, "na-canary-secret-setting", req.ResolvedEnv["NA_SECRET_SETTING"])
}

// TestNoAuth_CredentialNamedStoredEnvWithheld (T15b, the name mutant's
// killer): plain settings named like a harness credential are left out at
// create, start and restart; a plain git token setting and ordinary plain
// settings are kept.
func TestNoAuth_CredentialNamedStoredEnvWithheld(t *testing.T) {
	ctx := context.Background()
	build := func(t *testing.T, name string) *ldFixture {
		m := newLDFixture(t, name, ldOptions{})
		m.env(t, "COPILOT_GITHUB_TOKEN", "na-canary-copilot", store.ScopeProject, m.agent.ProjectID, false)
		m.env(t, "GH_TOKEN", "na-canary-gh", store.ScopeUser, m.agent.OwnerID, false)
		m.env(t, "ANTHROPIC_API_KEY", "na-canary-anthropic", store.ScopeHub, m.hubID, false)
		m.env(t, noAuthGitTokenKey, "na-plain-git-token", store.ScopeProject, m.agent.ProjectID, false)
		m.env(t, "HTTP_PROXY", "na-proxy", store.ScopeProject, m.agent.ProjectID, false)
		m.agent.AppliedConfig.NoAuth = true
		require.NoError(t, m.store.UpdateAgent(ctx, m.agent))
		return m
	}
	check := func(t *testing.T, env map[string]string) {
		for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "ANTHROPIC_API_KEY"} {
			assert.NotContains(t, env, k)
		}
		assert.Equal(t, "na-plain-git-token", env[noAuthGitTokenKey])
		assert.Equal(t, "na-proxy", env["HTTP_PROXY"])
	}

	t.Run("create", func(t *testing.T) {
		m := build(t, "na-names-create")
		req, err := m.d.buildCreateRequest(ctx, m.agent, "TestNoAuth_CredentialNamedStoredEnvWithheld")
		require.NoError(t, err)
		check(t, req.ResolvedEnv)
	})
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			m := build(t, "na-names-"+op.name)
			check(t, op.run(t, m).env)
		})
	}
}

// naHCErrStore fails the harness-config reads.
type naHCErrStore struct {
	store.Store
}

func (s *naHCErrStore) ListHarnessConfigs(context.Context, store.HarnessConfigFilter, store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
	return nil, errors.New("na harness config read failure")
}

// TestNoAuth_CredentialNamesFromMetadata (T15c): a stored harness config's
// declared name is withheld, so the names come from metadata; a harness
// config read error fails create and start with the fixed env settings
// message.
func TestNoAuth_CredentialNamesFromMetadata(t *testing.T) {
	ctx := context.Background()
	m := newLDFixture(t, "na-meta", ldOptions{})
	m.env(t, "ACME_KEY", "na-canary-acme", store.ScopeProject, m.agent.ProjectID, false)
	m.agent.AppliedConfig.NoAuth = true
	require.NoError(t, m.store.UpdateAgent(ctx, m.agent))

	// Without a harness config declaring it, ACME_KEY is an ordinary name.
	require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "task", false))
	assert.Equal(t, "na-canary-acme", m.client.lastResolvedEnv["ACME_KEY"])

	require.NoError(t, m.store.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("na-meta-hc"), Name: "acme", Slug: "acme", Harness: "acme",
		Scope: store.HarnessConfigScopeProject, ScopeID: m.agent.ProjectID, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{Harness: "acme", AuthMeta: &api.HarnessAuthMetadata{
			Types: map[string]api.HarnessAuthTypeMetadata{
				"api-key": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"ACME_KEY"}}}},
			},
		}},
	}))
	require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "task", false))
	assert.NotContains(t, m.client.lastResolvedEnv, "ACME_KEY", "a name a stored harness config declares is withheld")
	req, err := m.d.buildCreateRequest(ctx, m.agent, "TestNoAuth_CredentialNamesFromMetadata")
	require.NoError(t, err)
	assert.NotContains(t, req.ResolvedEnv, "ACME_KEY")

	// A harness config read error fails the dispatch, never withholding
	// nothing.
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(&naHCErrStore{Store: m.store}, client, false, slog.Default())
	d.SetAuthzService(m.authz)
	d.SetHubID(m.hubID)
	d.SetSecretBackend(m.backend)
	err = d.DispatchAgentStart(ctx, m.agent, "task", false)
	var sre *secretResolutionError
	require.ErrorAs(t, err, &sre)
	assert.Equal(t, resolutionSourceEnvSettings, sre.Source)
	assert.NotContains(t, err.Error(), "na harness config read failure")
	assert.False(t, client.startCalled, "nothing reaches the broker")
	_, err = d.buildCreateRequest(ctx, m.agent, "TestNoAuth_CredentialNamesFromMetadata")
	require.ErrorAs(t, err, &sre)
}

// TestNoAuthCredentialEnvNames_BuiltinsOnlyNonEmpty (T15e): with no stored
// harness config the set holds the built-in names, exactly.
func TestNoAuthCredentialEnvNames_BuiltinsOnlyNonEmpty(t *testing.T) {
	m := newLDFixture(t, "na-builtins", ldOptions{})
	names, err := m.d.noAuthCredentialEnvNames(context.Background(), m.agent)
	require.NoError(t, err)
	require.NotEmpty(t, names)
	for _, k := range []string{"ANTHROPIC_API_KEY", "COPILOT_GITHUB_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS"} {
		assert.True(t, names.has(k), k)
	}
	assert.False(t, names.has("copilot_github_token"), "names match exactly")
	assert.False(t, names.has("GOOGLE_CLOUD_PROJECT"), "a project setting is not a credential name")
}

// TestNoAuth_PersistedCredentialConfigEnvWithheld (T15d, the saved config
// env mutant's killer) and TestNoAuth_RequesterExplicitCredentialEnvDelivered
// (T15f) are the two sides of one rule: a credential-named key saved in the
// agent's config env is not applied, the same key set by the requester at
// create is.
func TestNoAuth_PersistedCredentialConfigEnvWithheld(t *testing.T) {
	ctx := context.Background()
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			m := newLDFixture(t, "na-saved-"+op.name, ldOptions{})
			m.agent.AppliedConfig.NoAuth = true
			m.agent.AppliedConfig.Env = map[string]string{
				"COPILOT_GITHUB_TOKEN": "na-canary-saved-copilot",
				"ANTHROPIC_API_KEY":    "na-canary-saved-anthropic",
				"NA_SAVED_ORDINARY":    "na-saved-ordinary",
			}
			require.NoError(t, m.store.UpdateAgent(ctx, m.agent))
			env := op.run(t, m).env
			assert.NotContains(t, env, "COPILOT_GITHUB_TOKEN")
			assert.NotContains(t, env, "ANTHROPIC_API_KEY")
			assert.Equal(t, "na-saved-ordinary", env["NA_SAVED_ORDINARY"])
		})
	}
	t.Run("create", func(t *testing.T) {
		m := newLDFixture(t, "na-saved-create", ldOptions{})
		m.agent.AppliedConfig.NoAuth = true
		m.agent.AppliedConfig.Env = map[string]string{"COPILOT_GITHUB_TOKEN": "na-canary-saved-copilot"}
		req, err := m.d.buildCreateRequest(ctx, m.agent, "TestNoAuth_PersistedCredentialConfigEnvWithheld")
		require.NoError(t, err)
		assert.NotContains(t, req.ResolvedEnv, "COPILOT_GITHUB_TOKEN")
		assert.NotContains(t, req.EnvClassifications, "COPILOT_GITHUB_TOKEN")
	})
}

func TestNoAuth_RequesterExplicitCredentialEnvDelivered(t *testing.T) {
	ctx := context.Background()
	explicit := map[string]string{"COPILOT_GITHUB_TOKEN": "na-explicit-copilot"}
	for _, op := range naOps {
		t.Run(op.name, func(t *testing.T) {
			m := newLDFixture(t, "na-explicit-"+op.name, ldOptions{})
			m.agent.AppliedConfig.NoAuth = true
			m.agent.AppliedConfig.Env = map[string]string{"COPILOT_GITHUB_TOKEN": "na-explicit-copilot"}
			m.agent.AppliedConfig.InlineConfig = &api.ScionConfig{Env: explicit}
			require.NoError(t, m.store.UpdateAgent(ctx, m.agent))
			assert.Equal(t, "na-explicit-copilot", op.run(t, m).env["COPILOT_GITHUB_TOKEN"])
		})
	}
	t.Run("create", func(t *testing.T) {
		m := newLDFixture(t, "na-explicit-create", ldOptions{})
		m.agent.AppliedConfig.NoAuth = true
		m.agent.AppliedConfig.Env = map[string]string{"COPILOT_GITHUB_TOKEN": "na-explicit-copilot"}
		m.agent.AppliedConfig.InlineConfig = &api.ScionConfig{Env: explicit}
		req, err := m.d.buildCreateRequest(ctx, m.agent, "TestNoAuth_RequesterExplicitCredentialEnvDelivered")
		require.NoError(t, err)
		assert.Equal(t, "na-explicit-copilot", req.ResolvedEnv["COPILOT_GITHUB_TOKEN"])
	})
}

// TestApplyStartExtras_NoAuth (T13): the intent is written under the key
// create uses only when set.
func TestApplyStartExtras_NoAuth(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{NoAuth: true})
	assert.Equal(t, true, payload["noAuth"])
	empty := map[string]interface{}{}
	applyStartExtras(empty, StartExtras{})
	_, ok := empty["noAuth"]
	assert.False(t, ok)
}

// naOldBrokerStartBody is the start body as a broker without the noAuth
// field decodes it.
type naOldBrokerStartBody struct {
	ResolvedEnv     map[string]string    `json:"resolvedEnv"`
	ResolvedSecrets []api.ResolvedSecret `json:"resolvedSecrets,omitempty"`
}

// TestStartNoAuth_OldBrokerGetsNothing (T12): the body a broker without
// the field decodes from a start of an agent without credentials carries no
// canary; the field it does not know is ignored.
func TestStartNoAuth_OldBrokerGetsNothing(t *testing.T) {
	f := newNAFixture(t, "na-old-broker", true, ldOptions{})
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "task", false))

	payload := map[string]interface{}{"resolvedEnv": f.client.lastResolvedEnv}
	if len(f.client.lastStartResolvedSecrets) > 0 {
		payload["resolvedSecrets"] = f.client.lastStartResolvedSecrets
	}
	applyStartExtras(payload, f.client.lastStartExtras)
	require.Equal(t, true, payload["noAuth"])
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	var old naOldBrokerStartBody
	require.NoError(t, json.NewDecoder(strings.NewReader(string(body))).Decode(&old))
	assertNoCanary(t, naSent{env: old.ResolvedEnv}, f.canaries)
	assert.Empty(t, old.ResolvedSecrets)
	assert.Equal(t, naGitProject, old.ResolvedEnv[noAuthGitTokenKey])
}

// TestNoAuthAgentTokenScopes_CreateEqualsStart (T14): create, start and
// restart authorize the agent token through the same function, and the
// no-credential intent is not one of its inputs: the candidate scopes of
// an agent without credentials equal those of the same agent with
// credentials, for role none and for the create-time fallback (a full
// role with the "none" auth choice).
func TestNoAuthAgentTokenScopes_CreateEqualsStart(t *testing.T) {
	a := NewAuthzService(createTestStore(t), slog.Default())
	for _, role := range []AgentRole{AgentRoleNone, AgentRoleFull} {
		withCreds := &store.Agent{ID: tid("na-token-" + string(role)), AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)}}
		noCreds := &store.Agent{ID: withCreds.ID, AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role), NoAuth: true, HarnessAuth: "none"}}
		assert.Equal(t, a.mintCandidateScopes(withCreds), a.mintCandidateScopes(noCreds), "role %s", role)
	}
}
