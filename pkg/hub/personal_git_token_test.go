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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	personalTestUserToken    = "personal-user-token"
	personalTestProjectToken = "project-scope-token"
	personalTestCallerToken  = "caller-shell-token"
)

// personalTestOwner is the agent owner's user ID (a UUID, as the store requires).
var personalTestOwner = tid("personal-token-owner")

// personalTokenFixture is a dispatcher with a secret backend over a real
// store, one project with gitRemote, and one stored agent of that project
// whose per-agent clone (or, with shared, shared workspace) names gitRemote.
type personalTokenFixture struct {
	st      store.Store
	client  *mockRuntimeBrokerClient
	d       *HTTPAgentDispatcher
	project *store.Project
	agent   *store.Agent
}

type personalTokenOpts struct {
	gitRemote    string
	shared       bool
	installation bool   // the project has its own GitHub App installation
	sourceLabel  bool   // the App token comes from another project (label)
	apiBaseURL   string // configured GitHub API base URL ("" = github.com)
	userSecret   bool   // the agent owner has a user-scope GITHUB_TOKEN
	brokerSecret bool   // the broker has a broker-scope GITHUB_TOKEN
	projSecret   bool   // the project has a project-scope GITHUB_TOKEN
	configEnv    string // GITHUB_TOKEN in the agent's applied config env
	noAuth       bool
}

func newPersonalTokenFixture(t *testing.T, o personalTokenOpts) *personalTokenFixture {
	t.Helper()
	ctx := context.Background()
	st := createTestStore(t)
	labels := map[string]string{}
	if o.shared {
		labels[store.LabelWorkspaceMode] = store.WorkspaceModeShared
	}
	project := &store.Project{
		ID: tid("project-personal-token"), Name: "Personal Token", Slug: "personal-token",
		GitRemote: o.gitRemote, Labels: labels,
	}
	installID := int64(4242)
	if o.installation || o.sourceLabel {
		require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: installID, AccountLogin: "org", AccountType: "Organization",
			AppID: 1, Status: store.GitHubInstallationStatusActive,
		}))
	}
	if o.installation {
		project.GitHubInstallationID = &installID
	}
	require.NoError(t, st.CreateProject(ctx, project))
	agentLabels := map[string]string{}
	if o.sourceLabel {
		source := &store.Project{
			ID: tid("project-token-source"), Name: "Token Source", Slug: "token-source",
			GitRemote: "github.com/org/source", GitHubInstallationID: &installID,
		}
		require.NoError(t, st.CreateProject(ctx, source))
		agentLabels["scion.dev/github-token-source-project"] = source.ID
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("broker-personal-token"), Name: "test-broker", Slug: "test-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	addSecret := func(id, value, scope, scopeID string) {
		require.NoError(t, st.CreateSecret(ctx, &store.Secret{
			ID: tid(id), Key: "GITHUB_TOKEN", EncryptedValue: value,
			SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
			// GITHUB_TOKEN secrets are delivered in the first pass.
			InjectionMode: store.InjectionModeAlways,
			Scope:         scope, ScopeID: scopeID,
		}))
	}
	if o.userSecret {
		addSecret("sec-personal-user", personalTestUserToken, store.ScopeUser, personalTestOwner)
	}
	if o.brokerSecret {
		addSecret("sec-personal-broker", "broker-scope-token", store.ScopeRuntimeBroker, tid("broker-personal-token"))
	}
	if o.projSecret {
		addSecret("sec-personal-project", personalTestProjectToken, store.ScopeProject, project.ID)
	}

	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(st, client, false, slog.Default())
	d.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
	d.SetGitHubAppMinter(&recordingGitHubAppMinter{})
	apiBase := o.apiBaseURL
	d.SetGitHubWebHostProvider(func() string { return githubapp.WebHostForAPIBaseURL(apiBase) })

	applied := &store.AgentAppliedConfig{HarnessConfig: "claude", NoAuth: o.noAuth}
	if o.configEnv != "" {
		applied.Env = map[string]string{"GITHUB_TOKEN": o.configEnv}
	}
	if o.shared {
		applied.Workspace = "/home/user/.scion/projects/personal-token"
	} else {
		depth := 1
		applied.GitClone = &api.GitCloneConfig{URL: resolveCloneURL("", o.gitRemote), Branch: "main", Depth: &depth}
	}
	agent := &store.Agent{
		ID: tid("agent-personal-token"), Name: "personal-agent", Slug: "personal-agent",
		ProjectID: project.ID, OwnerID: personalTestOwner, RuntimeBrokerID: tid("broker-personal-token"),
		Labels: agentLabels, AppliedConfig: applied,
	}
	require.NoError(t, st.CreateAgent(ctx, agent))
	agent, err := st.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	// Start and restart deliver launch secrets only under a delivery grant.
	grantLaunchDelivery(t, st, d, agent)
	return &personalTokenFixture{st: st, client: client, d: d, project: project, agent: agent}
}

// sentTokenRemote returns the token remote the broker received for the
// per-agent clone (or the shared workspace clone), and whether that clone
// config was sent at all.
type sentTokenRemote func(f *personalTokenFixture, shared bool) (string, bool)

func tokenRemoteOf(gc *api.GitCloneConfig) (string, bool) {
	if gc == nil {
		return "", false
	}
	return gc.TokenRemote, true
}

var personalTokenDispatches = []struct {
	name     string
	dispatch func(ctx context.Context, f *personalTokenFixture) error
	sent     sentTokenRemote
}{
	{
		name: "create",
		dispatch: func(ctx context.Context, f *personalTokenFixture) error {
			_, err := f.d.DispatchAgentCreate(ctx, f.agent)
			return err
		},
		sent: func(f *personalTokenFixture, shared bool) (string, bool) {
			if f.client.lastCreateReq == nil || f.client.lastCreateReq.Config == nil {
				return "", false
			}
			if shared {
				return tokenRemoteOf(f.client.lastCreateReq.Config.SharedWorkspaceClone)
			}
			return tokenRemoteOf(f.client.lastCreateReq.Config.GitClone)
		},
	},
	{
		name: "start",
		dispatch: func(ctx context.Context, f *personalTokenFixture) error {
			return f.d.DispatchAgentStart(ctx, f.agent, "", false)
		},
		sent: func(f *personalTokenFixture, shared bool) (string, bool) {
			if shared {
				return tokenRemoteOf(f.client.lastStartExtras.Workspace.SharedWorkspaceClone)
			}
			return tokenRemoteOf(f.client.lastStartExtras.Workspace.GitClone)
		},
	},
	{
		name: "restart",
		dispatch: func(ctx context.Context, f *personalTokenFixture) error {
			return f.d.DispatchAgentRestart(ctx, f.agent)
		},
		sent: func(f *personalTokenFixture, shared bool) (string, bool) {
			if shared {
				return tokenRemoteOf(f.client.lastRestartExtras.Workspace.SharedWorkspaceClone)
			}
			return tokenRemoteOf(f.client.lastRestartExtras.Workspace.GitClone)
		},
	},
}

// On create, start and restart, the token remote is withheld when the
// recorded remote is off the GitHub host and the token git would see is
// personal; it is kept for the GitHub host and for project-bound tokens.
func TestDispatch_PersonalTokenOnlyToGitHubHost(t *testing.T) {
	const attacker = "attacker.example/org/repo"
	for _, tc := range []struct {
		name     string
		opts     personalTokenOpts
		withheld bool
	}{
		{"owner's user secret, attacker host", personalTokenOpts{gitRemote: attacker, userSecret: true}, true},
		{"broker-scope secret, attacker host", personalTokenOpts{gitRemote: attacker, brokerSecret: true}, true},
		{"config env token, attacker host", personalTokenOpts{gitRemote: attacker, configEnv: "config-env-token"}, true},
		{"no token at dispatch, attacker host", personalTokenOpts{gitRemote: attacker}, true},
		{"no-auth user token fallback, attacker host", personalTokenOpts{gitRemote: attacker, userSecret: true, noAuth: true}, true},
		{"user token precedence over the App, attacker host", personalTokenOpts{gitRemote: attacker, installation: true, userSecret: true}, true},
		{"App token from another project's installation, attacker host", personalTokenOpts{gitRemote: attacker, sourceLabel: true}, true},
		{"github.com when GHES is configured", personalTokenOpts{gitRemote: "github.com/org/repo", userSecret: true, apiBaseURL: "https://ghe.example.com/api/v3"}, true},
		{"spoofed github.com subdomain", personalTokenOpts{gitRemote: "github.com.attacker.example/org/repo", userSecret: true}, true},

		{"owner's user secret, github.com", personalTokenOpts{gitRemote: "github.com/org/repo", userSecret: true}, false},
		{"no token at dispatch, github.com", personalTokenOpts{gitRemote: "github.com/org/repo"}, false},
		{"owner's user secret, configured GHES host", personalTokenOpts{gitRemote: "ghe.example.com/team/app", userSecret: true, apiBaseURL: "https://ghe.example.com/api/v3"}, false},
		{"project secret, attacker host", personalTokenOpts{gitRemote: attacker, projSecret: true}, false},
		{"no-auth project secret, attacker host", personalTokenOpts{gitRemote: attacker, projSecret: true, noAuth: true}, false},
		{"App token for the project, attacker host", personalTokenOpts{gitRemote: attacker, installation: true}, false},
	} {
		for _, shared := range []bool{false, true} {
			for _, op := range personalTokenDispatches {
				mode := "clone-per-agent"
				if shared {
					mode = "shared workspace"
				}
				t.Run(tc.name+"/"+mode+"/"+op.name, func(t *testing.T) {
					o := tc.opts
					o.shared = shared
					f := newPersonalTokenFixture(t, o)
					require.NoError(t, op.dispatch(context.Background(), f))
					got, sent := op.sent(f, shared)
					require.True(t, sent, "fixture check: the clone settings were not sent")
					if tc.withheld {
						assert.Empty(t, got, "a personal token remote off the GitHub host must be withheld")
					} else {
						assert.Equal(t, f.project.GitRemote, got, "the token remote must be kept")
					}
				})
			}
		}
	}
}

// finalize-env decides again after the caller's env is merged: a GITHUB_TOKEN
// from the caller replaces the project secret, so the token remote of an
// attacker-host project is withheld.
func TestDispatchFinalizeEnv_CallerTokenWithholdsTokenRemote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		withheld bool
	}{
		{"caller token", map[string]string{"GITHUB_TOKEN": personalTestCallerToken}, true},
		{"caller env without a token", map[string]string{"OTHER": "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersonalTokenFixture(t, personalTokenOpts{gitRemote: "attacker.example/org/repo", projSecret: true})
			_, err := f.d.DispatchFinalizeEnv(context.Background(), f.agent, tc.env)
			require.NoError(t, err)
			require.NotNil(t, f.client.lastCreateReq)
			require.NotNil(t, f.client.lastCreateReq.Config)
			got, sent := tokenRemoteOf(f.client.lastCreateReq.Config.GitClone)
			require.True(t, sent)
			if tc.withheld {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, "attacker.example/org/repo", got)
			}
		})
	}
}

// --- hub-host clone and fetch (resolveCloneToken, hostGitAccess) ---

func newPersonalCloneTokenServer(t *testing.T, apiBaseURL string) *Server {
	t.Helper()
	srv, st := testServer(t)
	ctx := context.Background()
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID: tid("sec-creator-gh"), Key: "GITHUB_TOKEN", EncryptedValue: personalTestUserToken,
		SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
		Scope: store.ScopeUser, ScopeID: "victim-user",
	}))
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID: tid("sec-project-gh-bound"), Key: "GITHUB_TOKEN", EncryptedValue: personalTestProjectToken,
		SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
		Scope: store.ScopeProject, ScopeID: "project-with-secret",
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
	srv.mu.Lock()
	srv.config.GitHubAppConfig.APIBaseURL = apiBaseURL
	srv.mu.Unlock()
	return srv
}

// The creator's personal token is the hub-host clone token only for a
// recorded remote on the GitHub host. A project clone that inherited an
// attacker-host remote (created by the victim, no project credential) gets
// none; a project secret keeps its binding.
func TestResolveCloneToken_PersonalTokenOnlyToGitHubHost(t *testing.T) {
	for _, tc := range []struct {
		name, apiBase, remote, projectID string
		want                             string
	}{
		{"inherited attacker host", "", "attacker.example/org/repo", "project-clone", ""},
		{"spoofed github.com subdomain", "", "github.com.attacker.example/org/repo", "project-clone", ""},
		{"github.com on another port", "", "github.com:8443/org/repo", "project-clone", ""},
		{"github.com when GHES is configured", "https://ghe.example.com/api/v3", "github.com/org/repo", "project-clone", ""},
		{"github.com", "", "github.com/org/repo", "project-clone", personalTestUserToken},
		{"configured GHES host", "https://ghe.example.com/api/v3", "ghe.example.com/team/app", "project-clone", personalTestUserToken},
		{"project secret, attacker host", "", "attacker.example/org/repo", "project-with-secret", personalTestProjectToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPersonalCloneTokenServer(t, tc.apiBase)
			project := &store.Project{ID: tc.projectID, GitRemote: tc.remote, CreatedBy: "victim-user"}
			assert.Equal(t, tc.want, srv.resolveCloneToken(context.Background(), project))
		})
	}
}

// The hub-host fetch (pull and local backends) offers the creator's personal
// token only to a GitHub-host remote; an attacker-host remote gets none,
// while a project secret keeps its binding to the recorded remote.
func TestHostGitAccess_PersonalTokenOnlyToGitHubHost(t *testing.T) {
	srv := newPersonalCloneTokenServer(t, "")
	ctx := context.Background()

	attacker := &store.Project{ID: "project-clone", GitRemote: "attacker.example/org/repo", CreatedBy: "victim-user"}
	token, _ := srv.hostGitAccess(ctx, attacker, "https://attacker.example/org/repo.git")
	assert.Empty(t, token, "an attacker-host remote gets no personal token")

	own := &store.Project{ID: "project-clone", GitRemote: "github.com/org/repo", CreatedBy: "victim-user"}
	token, _ = srv.hostGitAccess(ctx, own, "https://github.com/org/repo.git")
	assert.Equal(t, personalTestUserToken, token, "github.com keeps the creator's token")

	bound := &store.Project{ID: "project-with-secret", GitRemote: "attacker.example/org/repo", CreatedBy: "victim-user"}
	token, _ = srv.hostGitAccess(ctx, bound, "https://attacker.example/org/repo.git")
	assert.Equal(t, personalTestProjectToken, token, "a project secret keeps its binding")
}
