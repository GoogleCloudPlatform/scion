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
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// provisionTestRefs are the merged skill references of the provision
// credential fixtures: a bare gh:// ref (repo key, owner key and
// GITHUB_TOKEN), a ?token= ref (only its named secret) and a registry ref
// (nothing).
var provisionTestRefs = []string{
	"gh://acme/tools/review",
	"gh://beta/repo/deploy?token=BETA_TOKEN",
	"skill://scion/global/test@1.0.0",
}

// newProvisionCredsFixture is a launch delivery fixture (newLDFixture) whose
// dispatcher reads provisionTestRefs as the agent's merged skill
// references and whose secret backend records what it is asked to fetch.
func newProvisionCredsFixture(t *testing.T, name string, opts ldOptions) (*ldFixture, *ldRecordingSecretBackend, *lockedBuffer) {
	t.Helper()
	m := newLDFixture(t, name, opts)
	rec := &ldRecordingSecretBackend{SecretBackend: m.backend}
	m.d.SetSecretBackend(rec)
	m.d.SetSkillRefLister(provisionSkillRefs(provisionTestRefs...))
	buf := &lockedBuffer{}
	m.d.log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return m, rec, buf
}

// fetchedNames returns the names of the secrets rec was asked to fetch.
func fetchedNames(rec *ldRecordingSecretBackend) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	names := make([]string, 0, len(rec.fetched))
	for _, m := range rec.fetched {
		names = append(names, m.Name)
	}
	return names
}

// TestProvisionCredentials_LimitedToSkillFetchKeys: only the project
// secrets the agent's gh:// refs can use are fetched and sent, whatever
// their type or injection mode; secrets outside the key set (including the
// owner key of a ?token= ref) are neither fetched nor sent. Each sent entry
// is the secret's name and stored value, as before.
func TestProvisionCredentials_LimitedToSkillFetchKeys(t *testing.T) {
	m, rec, _ := newProvisionCredsFixture(t, "pc-limited", ldOptions{})
	project := m.agent.ProjectID
	m.secret(t, "GH_ACME__TOOLS", "repo-value", store.SecretTypeEnvironment, store.ScopeProject, project)
	m.secret(t, "GH_ACME", "owner-value", store.SecretTypeFile, store.ScopeProject, project)
	m.secret(t, "GITHUB_TOKEN", "default-value", store.SecretTypeVariable, store.ScopeProject, project)
	m.secret(t, "BETA_TOKEN", "ghp_namedvalue", store.SecretTypeEnvironment, store.ScopeProject, project)
	// Outside the key set.
	m.secret(t, "GH_BETA", "beta-owner-value", store.SecretTypeEnvironment, store.ScopeProject, project)
	m.secret(t, "GH_BETA__REPO", "beta-repo-value", store.SecretTypeEnvironment, store.ScopeProject, project)
	m.secret(t, "ANTHROPIC_API_KEY", "llm-value", store.SecretTypeEnvironment, store.ScopeProject, project)
	m.secret(t, "DEPLOY_PASSWORD", "deploy-value", store.SecretTypeFile, store.ScopeProject, project)

	got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
	assert.Equal(t, map[string]string{
		"GH_ACME__TOOLS": "repo-value",
		"GH_ACME":        "owner-value",
		"GITHUB_TOKEN":   "default-value",
		"BETA_TOKEN":     "ghp_namedvalue",
	}, got)
	assert.ElementsMatch(t, []string{"GH_ACME__TOOLS", "GH_ACME", "GITHUB_TOKEN", "BETA_TOKEN"}, fetchedNames(rec),
		"only key-set secrets are fetched")
	assert.Zero(t, rec.gets, "no value is read by name")
	assert.Zero(t, rec.resolves, "Resolve is never called")
}

// TestProvisionCredentials_UserScopeTokenNotSent: only project scope is
// sent. A matching user-scope (owner), hub-scope or broker-scope secret is
// neither fetched nor sent, alone or beside a project secret of the same
// name.
func TestProvisionCredentials_UserScopeTokenNotSent(t *testing.T) {
	seedOtherScopes := func(t *testing.T, m *ldFixture) {
		t.Helper()
		m.secret(t, "GITHUB_TOKEN", "user-value", store.SecretTypeEnvironment, store.ScopeUser, m.agent.OwnerID)
		m.secret(t, "GH_ACME", "user-owner-value", store.SecretTypeEnvironment, store.ScopeUser, m.agent.OwnerID)
		m.secret(t, "GH_ACME__TOOLS", "hub-value", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
		m.secret(t, "BETA_TOKEN", "broker-value", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
	}
	t.Run("no_project_secret", func(t *testing.T) {
		m, rec, _ := newProvisionCredsFixture(t, "pc-user-only", ldOptions{})
		seedOtherScopes(t, m)

		assert.Nil(t, m.d.resolveProvisionCredentials(context.Background(), m.agent, "test"))
		assert.Empty(t, fetchedNames(rec), "nothing is fetched")
	})
	t.Run("beside_project_secret", func(t *testing.T) {
		m, rec, _ := newProvisionCredsFixture(t, "pc-user-beside", ldOptions{})
		seedOtherScopes(t, m)
		m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
		assert.Equal(t, map[string]string{"GH_ACME": "project-value"}, got)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		require.Len(t, rec.fetched, 1)
		assert.Equal(t, store.ScopeProject, rec.fetched[0].Scope)
	})
}

// TestProvisionCredentials_RefusedAssociationNotSentOrFetched: when the
// agent's project association is refused for delivery (each row a single
// fault on an admitted fixture), no matching project secret is sent and
// none is fetched.
func TestProvisionCredentials_RefusedAssociationNotSentOrFetched(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ldOptions
		prep func(t *testing.T, m *ldFixture)
	}{
		// A recorded ceiling without the deliver permissions; an agent with
		// no edge at all is a legacy agent the fallback serves (below).
		{name: "recorded_ceiling_without_deliver", opts: ldOptions{edge: withBoundedCeiling("agent.read")}},
		{name: "root_not_admitted_to_project", opts: ldOptions{rootMemberNone: true}},
		{name: "agent_held", prep: func(t *testing.T, m *ldFixture) {
			holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rec, buf := newProvisionCredsFixture(t, "pc-refused-"+tc.name, tc.opts)
			m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			m.secret(t, "GITHUB_TOKEN", "default-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			if tc.prep != nil {
				tc.prep(t, m)
			}

			assert.Nil(t, m.d.resolveProvisionCredentials(context.Background(), m.agent, "DispatchAgentStart"))
			assert.Empty(t, fetchedNames(rec), "a refused secret is never fetched")
			assert.Zero(t, rec.gets)
			// The refusal is reported without debug, by name only.
			warn := findProvisionCredsRecord(buf.records(t), "WARN", "ProvisionCredentials not authorized for delivery to the agent; not sent")
			require.NotNil(t, warn, "log: %s", buf.String())
			assert.Equal(t, "DispatchAgentStart: ProvisionCredentials not authorized for delivery to the agent; not sent", warn["msg"])
			assert.Equal(t, []any{"GH_ACME", "GITHUB_TOKEN"}, warn["secrets"])
			assert.NotContains(t, buf.String(), "project-value")
			assert.NotContains(t, buf.String(), "default-value")
		})
	}
}

// TestProvisionCredentials_LegacyAgentServedWithWarn: an agent whose
// delegation records are legacy (no edge) still gets its skill-fetch
// credentials through the legacy fallback, with a fallback Warn per secret
// naming the agent and cohort, and no value.
func TestProvisionCredentials_LegacyAgentServedWithWarn(t *testing.T) {
	m, rec, buf := newProvisionCredsFixture(t, "pc-legacy", ldOptions{noEdge: true})
	m.authz.logger = m.d.log
	m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	m.secret(t, "GITHUB_TOKEN", "default-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

	got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "DispatchAgentStart")
	assert.Equal(t, map[string]string{"GH_ACME": "project-value", "GITHUB_TOKEN": "default-value"}, got)
	assert.ElementsMatch(t, []string{"GH_ACME", "GITHUB_TOKEN"}, fetchedNames(rec))
	assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", "ProvisionCredentials not authorized for delivery to the agent; not sent"))
	keys := map[string]bool{}
	for _, r := range buf.records(t) {
		if r["level"] == "WARN" && r["msg"] == "launch material delivered through the legacy fallback" {
			assert.Equal(t, m.agent.ID, r["agent_id"])
			assert.Equal(t, legacyDeliveryCohortNoEdge, r["cohort"])
			assert.Equal(t, string(PurposeProvisionCredential), r["purpose"])
			k, _ := r["key"].(string)
			keys[k] = true
		}
	}
	assert.Equal(t, map[string]bool{"GH_ACME": true, "GITHUB_TOKEN": true}, keys, "log: %s", buf.String())
	assert.NotContains(t, buf.String(), "project-value")
	assert.NotContains(t, buf.String(), "default-value")
}

// TestProvisionCredentials_AdmittedControl is the admitted control for the
// refused rows above: the same fixture and secrets, unchanged, are sent.
func TestProvisionCredentials_AdmittedControl(t *testing.T) {
	m, rec, buf := newProvisionCredsFixture(t, "pc-control", ldOptions{})
	m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	m.secret(t, "GITHUB_TOKEN", "default-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

	got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
	assert.Equal(t, map[string]string{"GH_ACME": "project-value", "GITHUB_TOKEN": "default-value"}, got)
	assert.ElementsMatch(t, []string{"GH_ACME", "GITHUB_TOKEN"}, fetchedNames(rec))
	assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", "ProvisionCredentials not authorized for delivery to the agent; not sent"),
		"no refusal is reported when every secret is admitted; log: %s", buf.String())
}

// TestProvisionCredentials_MintBeforeFetch: the delivery credential is
// minted from the stored agent and every matching item admitted before
// any value is fetched.
func TestProvisionCredentials_MintBeforeFetch(t *testing.T) {
	m, _, _ := newProvisionCredsFixture(t, "pc-order", ldOptions{})
	var log []string
	m.d.SetSecretBackend(&ldRecordingSecretBackend{SecretBackend: m.backend, log: &log})
	m.secret(t, "GH_ACME", "a", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	m.secret(t, "GH_ACME__TOOLS", "b", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	hdWithStore(t, m.authz, &ldOrderStore{Store: m.store, target: m.agent.ID, log: &log})

	got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
	require.Len(t, got, 2)

	firstAgent, lastEdges, firstFetch := -1, -1, -1
	for i, e := range log {
		switch e {
		case "agent":
			if firstAgent < 0 {
				firstAgent = i
			}
		case "edges":
			lastEdges = i
		case "fetch":
			if firstFetch < 0 {
				firstFetch = i
			}
		}
	}
	require.GreaterOrEqual(t, firstAgent, 0, "the credential was minted from the stored agent: %v", log)
	require.GreaterOrEqual(t, lastEdges, 0, "the admission walk ran: %v", log)
	require.GreaterOrEqual(t, firstFetch, 0, "values were fetched: %v", log)
	assert.Less(t, firstAgent, firstFetch, "mint precedes fetch: %v", log)
	assert.Less(t, lastEdges, firstFetch, "every admission precedes fetch: %v", log)
}

// TestProvisionCredentials_FetchFailures: a failed item is left out and
// logged by name only; a whole-call fetch error sends nothing; secrets the
// fetch could not read because the request ended are reported in one Warn.
func TestProvisionCredentials_FetchFailures(t *testing.T) {
	seed := func(t *testing.T, m *ldFixture) (failing, ok secret.SecretMeta) {
		t.Helper()
		failing = m.secret(t, "GH_ACME", "value-that-must-not-be-logged-1", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		ok = m.secret(t, "GITHUB_TOKEN", "value-that-must-not-be-logged-2", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		return failing, ok
	}
	t.Run("item_error", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-item-err", ldOptions{})
		failing, _ := seed(t, m)
		m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, itemErrID: failing.ID})

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "buildCreateRequest")
		assert.Equal(t, map[string]string{"GITHUB_TOKEN": "value-that-must-not-be-logged-2"}, got)
		recs := buf.records(t)
		warn := findProvisionCredsRecord(recs, "WARN", "failed to get project secret for ProvisionCredentials")
		require.NotNil(t, warn, "log: %s", buf.String())
		assert.Equal(t, "GH_ACME", warn["secret"])
		info := findProvisionCredsRecord(recs, "INFO", "ProvisionCredentials resolved")
		require.NotNil(t, info, "log: %s", buf.String())
		assert.EqualValues(t, 1, info["count"])
		assert.NotContains(t, buf.String(), "value-that-must-not-be-logged", "secret values must never be logged")
	})
	t.Run("whole_fetch_error", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-fetch-err", ldOptions{})
		seed(t, m)
		m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, fetchErr: errors.New("backend unavailable")})

		assert.Nil(t, m.d.resolveProvisionCredentials(context.Background(), m.agent, "DispatchAgentStart"))
		warn := findProvisionCredsRecord(buf.records(t), "WARN", "failed to fetch project secrets for ProvisionCredentials")
		require.NotNil(t, warn, "log: %s", buf.String())
	})
	t.Run("whole_fetch_error_request_ended", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-fetch-ended", ldOptions{})
		seed(t, m)
		ctx := &provisionEndableCtx{Context: context.Background(), err: context.Canceled}
		m.d.SetSecretBackend(&provisionEndThenFailBackend{SecretBackend: m.backend, end: ctx.end})

		assert.Nil(t, m.d.resolveProvisionCredentials(ctx, m.agent, "DispatchAgentStart"))
		assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", "failed to fetch project secrets for ProvisionCredentials"),
			"no Warn once the request has ended; log: %s", buf.String())
		assert.NotContains(t, buf.String(), "value-that-must-not-be-logged", "secret values must never be logged")
	})
	t.Run("missing_entry_live_request", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-missing", ldOptions{})
		missing, _ := seed(t, m)
		m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, dropID: missing.ID})

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "DispatchAgentStart")
		assert.Equal(t, map[string]string{"GITHUB_TOKEN": "value-that-must-not-be-logged-2"}, got)
		recs := buf.records(t)
		warn := findProvisionCredsRecord(recs, "WARN", "failed to get project secret for ProvisionCredentials")
		require.NotNil(t, warn, "a secret missing from the fetch result is reported; log: %s", buf.String())
		assert.Equal(t, "GH_ACME", warn["secret"])
		assert.Equal(t, "no result returned", warn["error"])
		assert.Nil(t, findProvisionCredsRecord(recs, "WARN", "ProvisionCredentials fetch stopped early; some project secrets were not fetched"),
			"a live request is not reported as ended; log: %s", buf.String())
		assert.NotContains(t, buf.String(), "value-that-must-not-be-logged", "secret values must never be logged")
	})
	cancelled := func(t *testing.T) (context.Context, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx, cancel
	}
	for _, tc := range []struct {
		name string
		// end returns the context to resolve with and the function the
		// backend calls, after fetching, to end it.
		end func(t *testing.T) (context.Context, func())
		// itemErr, when set, is the error of every returned item.
		itemErr error
		// dropFirst leaves the first seeded secret out of the result.
		dropFirst   bool
		want        map[string]string
		notFetched  int
		errContains string
	}{
		{
			name:        "cancelled",
			end:         cancelled,
			itemErr:     context.Canceled,
			notFetched:  2,
			errContains: "context canceled",
		},
		{
			name: "deadline_exceeded",
			end: func(t *testing.T) (context.Context, func()) {
				ctx := &provisionEndableCtx{Context: context.Background(), err: context.DeadlineExceeded}
				return ctx, ctx.end
			},
			itemErr:     context.DeadlineExceeded,
			notFetched:  2,
			errContains: "deadline exceeded",
		},
		{
			name: "deadline_item_errors_live_request",
			end: func(t *testing.T) (context.Context, func()) {
				return context.Background(), func() {}
			},
			itemErr:     context.DeadlineExceeded,
			notFetched:  2,
			errContains: "deadline exceeded",
		},
		{
			name:        "partial_result_after_cancel",
			end:         cancelled,
			dropFirst:   true,
			want:        map[string]string{"GITHUB_TOKEN": "value-that-must-not-be-logged-2"},
			notFetched:  1,
			errContains: "context canceled",
		},
	} {
		t.Run("request_ended_"+tc.name, func(t *testing.T) {
			m, _, buf := newProvisionCredsFixture(t, "pc-end-"+tc.name, ldOptions{})
			first, _ := seed(t, m)
			ctx, end := tc.end(t)
			b := &provisionEndingBackend{SecretBackend: m.backend, end: end, itemErr: tc.itemErr}
			if tc.dropFirst {
				b.dropID = first.ID
			}
			m.d.SetSecretBackend(b)

			got := m.d.resolveProvisionCredentials(ctx, m.agent, "DispatchAgentRestart")
			if tc.want == nil {
				assert.Nil(t, got)
			} else {
				assert.Equal(t, tc.want, got)
			}
			var perSecret, summary int
			var rec map[string]any
			for _, r := range buf.records(t) {
				if r["level"] != "WARN" {
					continue
				}
				switch msg := r["msg"].(string); {
				case msg == "DispatchAgentRestart: failed to get project secret for ProvisionCredentials":
					perSecret++
				case msg == "DispatchAgentRestart: ProvisionCredentials fetch stopped early; some project secrets were not fetched":
					summary++
					rec = r
				}
			}
			assert.Zero(t, perSecret, "no per-secret Warn when the request ended; log: %s", buf.String())
			require.Equal(t, 1, summary, "log: %s", buf.String())
			assert.EqualValues(t, tc.notFetched, rec["not_fetched"])
			assert.Contains(t, rec["error"], tc.errContains)
			assert.NotContains(t, buf.String(), "value-that-must-not-be-logged", "secret values must never be logged")
		})
	}
}

// provisionEndingBackend fetches the values, then ends the request (end)
// and reports what a backend interrupted at that point would: every item
// failed with itemErr when it is set, and dropID left out of the result.
type provisionEndingBackend struct {
	secret.SecretBackend
	end     func()
	itemErr error
	dropID  string
}

func (b *provisionEndingBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	out, err := b.SecretBackend.FetchValues(ctx, metas)
	if err != nil {
		return nil, err
	}
	b.end()
	if b.itemErr != nil {
		for id := range out {
			out[id] = secret.FetchResult{Err: b.itemErr}
		}
	}
	if b.dropID != "" {
		delete(out, b.dropID)
	}
	return out, nil
}

// provisionEndThenFailBackend ends the request (end) inside FetchValues and
// then fails the whole call, as a backend whose request was cancelled does.
type provisionEndThenFailBackend struct {
	secret.SecretBackend
	end func()
}

func (b *provisionEndThenFailBackend) FetchValues(context.Context, []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.end()
	return nil, context.Canceled
}

// provisionEndableCtx is a context whose Err turns to err once end is
// called, so a test ends a request with a deadline at a chosen point
// without a timer. Done is the parent's.
type provisionEndableCtx struct {
	context.Context
	err   error
	ended atomic.Bool
}

func (c *provisionEndableCtx) end() { c.ended.Store(true) }

func (c *provisionEndableCtx) Err() error {
	if c.ended.Load() {
		return c.err
	}
	return c.Context.Err()
}

// TestProvisionCredentials_CallerParity: create, start and restart send
// the broker the same provision credentials for the same agent, refs and
// secrets: the key-set project secrets only.
func TestProvisionCredentials_CallerParity(t *testing.T) {
	want := map[string]string{"GH_ACME": "project-value", "BETA_TOKEN": "ghp_namedvalue"}
	seed := func(t *testing.T, m *ldFixture) {
		t.Helper()
		m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "BETA_TOKEN", "ghp_namedvalue", store.SecretTypeFile, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "UNRELATED_KEY", "unrelated", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "GITHUB_TOKEN", "user-value", store.SecretTypeEnvironment, store.ScopeUser, m.agent.OwnerID)
	}

	m, _, _ := newProvisionCredsFixture(t, "pc-parity", ldOptions{})
	seed(t, m)
	ctx := context.Background()

	req, err := m.d.buildCreateRequest(ctx, m.agent, "buildCreateRequest")
	require.NoError(t, err)
	assert.Equal(t, want, req.ProvisionCredentials, "create")

	require.NoError(t, m.d.DispatchAgentStart(ctx, m.agent, "", false))
	require.True(t, m.client.startCalled)
	assert.Equal(t, want, m.client.lastStartExtras.ProvisionCredentials, "start")

	require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
	require.True(t, m.client.restartCalled)
	assert.Equal(t, want, m.client.lastRestartExtras.ProvisionCredentials, "restart")
}

// TestProvisionCredentials_NotInContainerEnv: a provision credential goes
// to the broker only. An as_needed project secret in the key set is sent
// as a provision credential and is not placed in the container env or
// secrets of the restart.
func TestProvisionCredentials_NotInContainerEnv(t *testing.T) {
	m, _, _ := newProvisionCredsFixture(t, "pc-not-env", ldOptions{})
	_, _, err := m.backend.Set(context.Background(), &secret.SetSecretInput{
		Name: "GH_ACME", Value: "provision-only", SecretType: store.SecretTypeEnvironment, Target: "GH_ACME",
		Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAsNeeded, CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)

	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	require.True(t, m.client.restartCalled)
	assert.Equal(t, map[string]string{"GH_ACME": "provision-only"}, m.client.lastRestartExtras.ProvisionCredentials)
	assert.NotContains(t, m.client.lastRestartResolvedEnv, "GH_ACME")
	for _, s := range m.client.lastRestartExtras.ResolvedSecrets {
		assert.NotEqual(t, "GH_ACME", s.Name)
		assert.NotEqual(t, "provision-only", s.Value)
	}
}

// TestProvisionCredentials_TokenParamRequiresGitHubToken: a secret named
// only by a ?token= reference is sent only when its value is shaped like a
// GitHub token. Any other value is dropped after the authorized fetch and
// reported by name only. Convention keys (GH_*, GITHUB_TOKEN) are sent
// whatever their value, including when a ?token= reference also names one.
func TestProvisionCredentials_TokenParamRequiresGitHubToken(t *testing.T) {
	const notShapedWarn = "ProvisionCredentials ?token= secret is not a GitHub token; not sent"
	t.Run("github_token_accepted", func(t *testing.T) {
		for _, v := range []string{"github_pat_11ABCDEFG0_abcdefXYZ789", "ghs_installation0123456789", "ghp_classic0123456789"} {
			m, _, buf := newProvisionCredsFixture(t, "pc-token-ok-"+v[:4], ldOptions{})
			m.secret(t, "BETA_TOKEN", v, store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

			got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
			assert.Equal(t, map[string]string{"BETA_TOKEN": v}, got)
			assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", notShapedWarn), "log: %s", buf.String())
		}
	})
	t.Run("non_github_value_rejected_and_never_logged", func(t *testing.T) {
		const value = "sk-ant-api03-ZQ9sentinelValueThatMustNotBeLogged"
		m, rec, buf := newProvisionCredsFixture(t, "pc-token-rejected", ldOptions{})
		m.secret(t, "BETA_TOKEN", value, store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "GH_ACME", "ghp_ownerkey0123456789", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "DispatchAgentStart")
		assert.Equal(t, map[string]string{"GH_ACME": "ghp_ownerkey0123456789"}, got)
		// The value is read only after the secret was authorized: the drop
		// happens on the fetched value, never before the decision.
		assert.ElementsMatch(t, []string{"BETA_TOKEN", "GH_ACME"}, fetchedNames(rec))
		recs := buf.records(t)
		warn := findProvisionCredsRecord(recs, "WARN", notShapedWarn)
		require.NotNil(t, warn, "log: %s", buf.String())
		assert.Equal(t, "DispatchAgentStart: "+notShapedWarn, warn["msg"])
		assert.Equal(t, "BETA_TOKEN", warn["secret"])
		info := findProvisionCredsRecord(recs, "INFO", "ProvisionCredentials resolved")
		require.NotNil(t, info, "log: %s", buf.String())
		assert.EqualValues(t, 1, info["count"])
		for _, part := range []string{value, "sk-ant", "ZQ9sentinel", "MustNotBeLogged"} {
			assert.NotContains(t, buf.String(), part, "no part of a rejected value is logged")
		}
	})
	t.Run("convention_keys_unaffected", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-token-convention", ldOptions{})
		m.secret(t, "GH_ACME", "legacy0123456789abcdef", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "GH_ACME__TOOLS", "not-shaped-repo-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "GITHUB_TOKEN", "not-shaped-default", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
		assert.Equal(t, map[string]string{
			"GH_ACME":        "legacy0123456789abcdef",
			"GH_ACME__TOOLS": "not-shaped-repo-value",
			"GITHUB_TOKEN":   "not-shaped-default",
		}, got)
		assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", notShapedWarn), "log: %s", buf.String())
	})
	t.Run("token_name_also_convention_key", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-token-overlap", ldOptions{})
		m.d.SetSkillRefLister(provisionSkillRefs("gh://acme/tools/review", "gh://beta/repo/deploy?token=GH_ACME"))
		m.secret(t, "GH_ACME", "legacy0123456789abcdef", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		got := m.d.resolveProvisionCredentials(context.Background(), m.agent, "test")
		assert.Equal(t, map[string]string{"GH_ACME": "legacy0123456789abcdef"}, got,
			"the bare ref's convention key is sent whatever its value")
		assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", notShapedWarn), "log: %s", buf.String())
	})
	t.Run("convention_shaped_name_without_bare_ref", func(t *testing.T) {
		m, _, buf := newProvisionCredsFixture(t, "pc-token-gh-name", ldOptions{})
		m.d.SetSkillRefLister(provisionSkillRefs("gh://beta/repo/deploy?token=GH_ACME"))
		m.secret(t, "GH_ACME", "not-shaped-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		assert.Nil(t, m.d.resolveProvisionCredentials(context.Background(), m.agent, "test"),
			"a name used only by ?token= is checked even when it looks like a convention key")
		warn := findProvisionCredsRecord(buf.records(t), "WARN", notShapedWarn)
		require.NotNil(t, warn, "log: %s", buf.String())
		assert.Equal(t, "GH_ACME", warn["secret"])
	})
}
