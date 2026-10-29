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
	"crypto/subtle"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
)

// A PERSONAL git token is a GITHUB_TOKEN that belongs to a user rather than
// to the project: the agent owner's or the project creator's user-scope
// secret, a token from the agent's config or storage env, a token the caller
// supplies at env-gather, or a broker- or hub-scope secret. A project's
// recorded remote (store.Project.GitRemote) is chosen by whoever created the
// project, and a project clone inherits it, so it cannot decide alone where
// such a token goes. A personal token is offered to git only when the
// recorded remote is an owner/repo on the GitHub host: github.com, or the
// host served by the configured GitHub API base URL (GHES / GHE.com). The
// existing binding to the recorded remote (https, same host and owner/repo)
// still applies on top.
//
// Only two tokens are project-bound and keep their existing binding alone:
// a GitHub App installation token minted for the agent's own project in this
// dispatch, and the project's own project-scope GITHUB_TOKEN secret.

// personalTokenHostAllowed reports whether a personal token may be offered to
// gitRemote: true only when gitRemote names an owner/repo on gitHubHost,
// compared with githubapp.RemoteOwnerRepo (canonical host; an ssh:// port is
// ignored, an https port is part of the host, so only the default port
// matches). An empty gitRemote or gitHubHost is never allowed.
func personalTokenHostAllowed(gitRemote, gitHubHost string) bool {
	if gitRemote == "" || gitHubHost == "" {
		return false
	}
	_, _, ok := githubapp.RemoteOwnerRepo(gitRemote, gitHubHost)
	return ok
}

// personalTokenGitHubHost returns the GitHub host personal tokens may go to:
// the web host of the configured GitHub API base URL, github.com when none
// is configured.
func (s *Server) personalTokenGitHubHost() string {
	s.mu.RLock()
	apiBase := s.config.GitHubAppConfig.APIBaseURL
	s.mu.RUnlock()
	return githubapp.WebHostForAPIBaseURL(apiBase)
}

// SetGitHubWebHostProvider sets the source of the GitHub host personal git
// tokens may go to (see personalTokenHostAllowed). Without one, the host is
// github.com.
func (d *HTTPAgentDispatcher) SetGitHubWebHostProvider(fn func() string) {
	d.gitHubWebHost = fn
}

// personalTokenGitHubHost returns the GitHub host personal git tokens may go
// to for this dispatcher.
func (d *HTTPAgentDispatcher) personalTokenGitHubHost() string {
	if d.gitHubWebHost == nil {
		return githubapp.WebHostForAPIBaseURL("")
	}
	return d.gitHubWebHost()
}

// gitTokenDispatch is what a dispatch knows about the GITHUB_TOKEN git will
// see in the agent: the env and secrets it sends, and the App installation
// token it minted for the agent's own project ("" when none).
type gitTokenDispatch struct {
	projectID string
	gitRemote string
	env       map[string]string
	secrets   []ResolvedSecret
	appToken  string
}

// withholdTokenRemote reports whether the dispatch must send no token remote
// (api.GitCloneConfig.TokenRemote), so that sciontool init, sciontool
// provision and the shared-workspace helper offer git no token at all. It is
// true when the recorded remote is not on the GitHub host and the token git
// would see is not positively project-bound (gitTokenProjectBound).
func (d *HTTPAgentDispatcher) withholdTokenRemote(ctx context.Context, td gitTokenDispatch) bool {
	if td.gitRemote == "" {
		return false // no token remote is sent anyway
	}
	if personalTokenHostAllowed(td.gitRemote, d.personalTokenGitHubHost()) {
		return false
	}
	return !d.gitTokenProjectBound(ctx, td)
}

// gitTokenProjectBound reports whether every GITHUB_TOKEN the agent can see
// is project-bound. The candidates are the env value and every
// environment-type secret projected to GITHUB_TOKEN. A candidate is
// project-bound only when it is the App installation token minted for the
// agent's own project in this dispatch, a secret the hub resolved from the
// project scope, or equal to the project's project-scope GITHUB_TOKEN secret.
// No candidate at all is not project-bound: the token then comes from
// somewhere the hub does not see (the caller's env at gather, or the broker).
func (d *HTTPAgentDispatcher) gitTokenProjectBound(ctx context.Context, td gitTokenDispatch) bool {
	var projectSecret string
	projectSecretLoaded := false
	equalsProjectSecret := func(v string) bool {
		if !projectSecretLoaded {
			projectSecretLoaded = true
			if d.secretBackend != nil && td.projectID != "" {
				if sv, err := d.secretBackend.Get(ctx, provision.GitTokenEnv, secret.ScopeProject, td.projectID); err == nil && sv != nil {
					projectSecret = sv.Value
				}
			}
		}
		return projectSecret != "" && subtle.ConstantTimeCompare([]byte(v), []byte(projectSecret)) == 1
	}
	isAppToken := func(v string) bool {
		return td.appToken != "" && subtle.ConstantTimeCompare([]byte(v), []byte(td.appToken)) == 1
	}

	candidates := 0
	if v := td.env[provision.GitTokenEnv]; v != "" {
		candidates++
		if !isAppToken(v) && !equalsProjectSecret(v) {
			return false
		}
	}
	for _, s := range td.secrets {
		if s.Type != "" && s.Type != "environment" {
			continue
		}
		target := s.Target
		if target == "" {
			target = s.Name
		}
		if target != provision.GitTokenEnv {
			continue
		}
		candidates++
		if s.Source == secret.ScopeProject {
			continue
		}
		if s.Value == "" || !equalsProjectSecret(s.Value) {
			return false
		}
	}
	return candidates > 0
}

// withoutTokenRemote returns a copy of gc with no TokenRemote, or nil when gc
// is nil.
func withoutTokenRemote(gc *api.GitCloneConfig) *api.GitCloneConfig {
	if gc == nil {
		return nil
	}
	out := *gc
	out.TokenRemote = ""
	return &out
}

// restrictPersonalTokenRemote clears the token remote of both clone settings
// when withholdTokenRemote says so. It only ever clears, so running it again
// after more env is merged (finalize-env) is safe.
func (d *HTTPAgentDispatcher) restrictPersonalTokenRemote(ctx context.Context, td gitTokenDispatch, gitClone, sharedClone **api.GitCloneConfig) {
	if (*gitClone == nil || (*gitClone).TokenRemote == "") && (*sharedClone == nil || (*sharedClone).TokenRemote == "") {
		return
	}
	if !d.withholdTokenRemote(ctx, td) {
		return
	}
	d.log.Info("git token not offered: the project's git remote is not on the GitHub host and the token is not the project's own",
		"project_id", td.projectID)
	*gitClone = withoutTokenRemote(*gitClone)
	*sharedClone = withoutTokenRemote(*sharedClone)
}

// restrictCreateRequestTokenRemote applies restrictPersonalTokenRemote to a
// create request, from the env and secrets it carries.
func (d *HTTPAgentDispatcher) restrictCreateRequestTokenRemote(ctx context.Context, req *RemoteCreateAgentRequest) {
	if req == nil || req.Config == nil {
		return
	}
	d.restrictPersonalTokenRemote(ctx, gitTokenDispatch{
		projectID: req.ProjectID,
		gitRemote: req.projectGitRemote,
		env:       req.ResolvedEnv,
		secrets:   req.ResolvedSecrets,
		appToken:  req.githubAppToken,
	}, &req.Config.GitClone, &req.Config.SharedWorkspaceClone)
}
