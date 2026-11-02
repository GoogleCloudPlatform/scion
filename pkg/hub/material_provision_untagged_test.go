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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// provisionCredsBackend serves a fixed project-scope listing and records
// the calls resolveProvisionCredentials makes. It never serves a value by
// name: Get fails the test.
type provisionCredsBackend struct {
	secret.SecretBackend
	t       *testing.T
	listErr error
	metas   []secret.SecretMeta
	lists   int
	fetches int
}

func (b *provisionCredsBackend) List(context.Context, secret.Filter) ([]secret.SecretMeta, error) {
	b.lists++
	return b.metas, b.listErr
}

func (b *provisionCredsBackend) FetchValues(context.Context, []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.fetches++
	return nil, errors.New("not expected")
}

func (b *provisionCredsBackend) Get(context.Context, string, string, string) (*secret.SecretWithValue, error) {
	b.t.Errorf("provision credentials must not read a value by name")
	return nil, errors.New("not expected")
}

// provisionSkillRefs returns a skill reference lister that returns uris as
// the merged skill references of every agent.
func provisionSkillRefs(uris ...string) func(context.Context, *store.Agent) []api.SkillReference {
	return func(context.Context, *store.Agent) []api.SkillReference {
		refs := make([]api.SkillReference, 0, len(uris))
		for _, u := range uris {
			refs = append(refs, api.SkillReference{URI: u})
		}
		return refs
	}
}

func newProvisionCredsDispatcher(backend secret.SecretBackend) (*HTTPAgentDispatcher, *lockedBuffer) {
	buf := &lockedBuffer{}
	// debug=false: the failure warnings and the count must not depend on it.
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	d.secretBackend = backend
	return d, buf
}

func findProvisionCredsRecord(recs []map[string]any, level, msgSuffix string) map[string]any {
	for _, r := range recs {
		if r["level"] == level && strings.HasSuffix(r["msg"].(string), msgSuffix) {
			return r
		}
	}
	return nil
}

// TestGHCredentialKeysForSkills pins the key set to the broker resolver's
// lookup: ?token= names only its secret; any other GitHub ref names its
// repo key, its owner key and GITHUB_TOKEN; other refs name nothing. A
// ?token= name is token-only unless a bare ref also uses it as a
// convention key.
func TestGHCredentialKeysForSkills(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uris          []string
		want          []string
		wantTokenOnly map[string]bool
	}{
		{"none", nil, nil, nil},
		{"registry_and_gcp_only", []string{"skill://scion/global/test@1.0.0", "gcp-skill://reg/skill", ""}, nil, nil},
		{"unparseable_gh", []string{"gh://only-owner"}, nil, nil},
		{"gh_shorthand", []string{"gh://Acme-Corp/tools/review@v1"},
			[]string{"GH_ACME_CORP", "GH_ACME_CORP__TOOLS", "GITHUB_TOKEN"}, nil},
		{"github_url", []string{"https://github.com/acme/tools/tree/main/skills/review"},
			[]string{"GH_ACME", "GH_ACME__TOOLS", "GITHUB_TOKEN"}, nil},
		{"token_param_only", []string{"gh://acme/tools/review?token=MY_SKILL_TOKEN"},
			[]string{"MY_SKILL_TOKEN"}, map[string]bool{"MY_SKILL_TOKEN": true}},
		{"merged_and_deduplicated", []string{
			"gh://acme/tools/a",
			"gh://acme/tools/b",
			"gh://acme/other/c",
			"gh://beta/repo/d?token=BETA_TOKEN",
			"skill://scion/global/test",
		}, []string{"BETA_TOKEN", "GH_ACME", "GH_ACME__OTHER", "GH_ACME__TOOLS", "GITHUB_TOKEN"},
			map[string]bool{"BETA_TOKEN": true}},
		{"token_name_is_also_convention_key", []string{
			"gh://acme/tools/a",
			"gh://beta/repo/d?token=GH_ACME",
			"gh://beta/repo/e?token=GITHUB_TOKEN",
		}, []string{"GH_ACME", "GH_ACME__TOOLS", "GITHUB_TOKEN"}, nil},
		{"convention_shaped_token_name_without_bare_ref", []string{"gh://beta/repo/d?token=GH_ACME"},
			[]string{"GH_ACME"}, map[string]bool{"GH_ACME": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refs []api.SkillReference
			for _, u := range tc.uris {
				refs = append(refs, api.SkillReference{URI: u})
			}
			keys, tokenOnly := ghCredentialKeysForSkills(refs)
			assert.Equal(t, tc.want, keys)
			assert.Equal(t, tc.wantTokenOnly, tokenOnly)
		})
	}
}

// TestGitHubTokenShaped: a documented GitHub token prefix followed by one
// or more letters, digits or underscores, and nothing else.
func TestGitHubTokenShaped(t *testing.T) {
	// Token-shaped vectors are assembled at run time from low-entropy parts,
	// so the source holds no literal that secret scanners match.
	ghpToken := "ghp_" + strings.Repeat("aB3_", 9)
	patToken := "github" + "_pat_" + "11" + strings.Repeat("A", 20) + "_" + strings.Repeat("b9", 29) + "c"
	awsKeyID := "AK" + "IA" + strings.Repeat("Z", 16)
	pemHeader := "-----" + "BEGIN " + "PRIVATE" + " KEY-----"
	for _, v := range []string{
		ghpToken,
		"gho_abc123",
		"ghu_abc123",
		"ghs_abc123",
		"ghr_abc123",
		patToken,
	} {
		assert.True(t, githubTokenShaped(v), v)
	}
	for _, v := range []string{
		"",
		"ghp_",
		"github_pat_",
		"GHP_abc123",
		"ghx_abc123",
		"sk-ant-api03-abcdef",
		awsKeyID,
		"0123456789abcdef0123456789abcdef01234567", // legacy 40-hex form
		"ghp_abc 123",
		"ghp_abc123\n",
		"ghp_abc-123",
		"Bearer ghp_abc123",
		pemHeader,
	} {
		assert.False(t, githubTokenShaped(v), v)
	}
}

// TestProvisionCredentials_NoGitHubRefsSendsNothing: with no gh:// skill
// reference (no lister, or only other references), nothing is listed,
// authorized or fetched, and the count 0 is logged.
func TestProvisionCredentials_NoGitHubRefsSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lister func(context.Context, *store.Agent) []api.SkillReference
	}{
		{"no_lister", nil},
		{"no_refs", provisionSkillRefs()},
		{"non_github_refs", provisionSkillRefs("skill://scion/global/test@1.0.0", "gcp-skill://reg/skill")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &provisionCredsBackend{t: t, metas: []secret.SecretMeta{
				{ID: "s1", Name: "GITHUB_TOKEN", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeProject},
			}}
			d, buf := newProvisionCredsDispatcher(backend)
			d.skillRefLister = tc.lister
			agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

			assert.Nil(t, d.resolveProvisionCredentials(context.Background(), agent, "DispatchAgentStart"))
			assert.Zero(t, backend.lists, "no secret is listed")
			assert.Zero(t, backend.fetches, "no secret is fetched")
			info := findProvisionCredsRecord(buf.records(t), "INFO", "ProvisionCredentials resolved")
			require.NotNil(t, info, "log: %s", buf.String())
			assert.EqualValues(t, 0, info["count"])
		})
	}
}

func TestResolveProvisionCredentials_ListFailureWarns(t *testing.T) {
	backend := &provisionCredsBackend{t: t, listErr: errors.New("backend unavailable")}
	d, buf := newProvisionCredsDispatcher(backend)
	d.skillRefLister = provisionSkillRefs("gh://acme/tools/review")
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	creds := d.resolveProvisionCredentials(context.Background(), agent, "buildCreateRequest")
	assert.Nil(t, creds)
	assert.Zero(t, backend.fetches)

	rec := findProvisionCredsRecord(buf.records(t), "WARN", "failed to list project secrets for ProvisionCredentials")
	require.NotNil(t, rec, "a List failure must be logged at Warn without debug; log: %s", buf.String())
	assert.Equal(t, "agent-1", rec["agent_id"])
	assert.Equal(t, "project-1", rec["project_id"])
}

// TestResolveProvisionCredentials_EndedRequestNoWarn: once the request has
// ended (ctx cancelled), a List failure or an authorization failure still
// sends nothing, but logs no Warn: the failure is the caller's cancel, not a
// backend or authorization fault. The live-request Warns are pinned by
// TestResolveProvisionCredentials_ListFailureWarns and
// TestProvisionCredentials_NoAuthzServiceSendsNothing.
func TestResolveProvisionCredentials_EndedRequestNoWarn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	t.Run("list", func(t *testing.T) {
		backend := &provisionCredsBackend{t: t, listErr: context.Canceled}
		d, buf := newProvisionCredsDispatcher(backend)
		d.skillRefLister = provisionSkillRefs("gh://acme/tools/review")

		assert.Nil(t, d.resolveProvisionCredentials(ctx, agent, "buildCreateRequest"))
		assert.Equal(t, 1, backend.lists)
		assert.Zero(t, backend.fetches)
		assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", "failed to list project secrets for ProvisionCredentials"),
			"no Warn once the request has ended; log: %s", buf.String())
	})
	t.Run("authorize", func(t *testing.T) {
		backend := &provisionCredsBackend{t: t, metas: []secret.SecretMeta{
			{ID: "s1", Name: "GH_ACME", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeProject, ScopeID: "project-1"},
		}}
		d, buf := newProvisionCredsDispatcher(backend)
		d.skillRefLister = provisionSkillRefs("gh://acme/tools/review")

		assert.Nil(t, d.resolveProvisionCredentials(ctx, agent, "DispatchAgentRestart"))
		assert.Equal(t, 1, backend.lists)
		assert.Zero(t, backend.fetches, "nothing is fetched without an authorization")
		assert.Nil(t, findProvisionCredsRecord(buf.records(t), "WARN", "ProvisionCredentials not authorized; none sent"),
			"no Warn once the request has ended; log: %s", buf.String())
	})
}

// TestProvisionCredentials_NoAuthzServiceSendsNothing: a matching project
// secret is not fetched or sent when the delivery credential cannot be
// minted (no authorization service); the failure is logged at Warn.
func TestProvisionCredentials_NoAuthzServiceSendsNothing(t *testing.T) {
	backend := &provisionCredsBackend{t: t, metas: []secret.SecretMeta{
		{ID: "s1", Name: "GH_ACME", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeProject, ScopeID: "project-1"},
	}}
	d, buf := newProvisionCredsDispatcher(backend)
	d.skillRefLister = provisionSkillRefs("gh://acme/tools/review")
	agent := &store.Agent{ID: "agent-1", ProjectID: "project-1"}

	assert.Nil(t, d.resolveProvisionCredentials(context.Background(), agent, "DispatchAgentRestart"))
	assert.Equal(t, 1, backend.lists)
	assert.Zero(t, backend.fetches, "nothing is fetched without an authorization")
	rec := findProvisionCredsRecord(buf.records(t), "WARN", "ProvisionCredentials not authorized; none sent")
	require.NotNil(t, rec, "log: %s", buf.String())
	assert.Equal(t, "agent-1", rec["agent_id"])
}
