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
	"crypto/subtle"
	"slices"

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
// see in the agent: the env and secrets it sends, the App installation token
// it minted for the agent's own project ("" when none), and the project-scope
// GITHUB_TOKEN it put into the env itself ("" when none; the no-auth path).
type gitTokenDispatch struct {
	projectID    string
	gitRemote    string
	env          map[string]string
	secrets      []ResolvedSecret
	appToken     string
	projectToken string
}

// withholdTokenRemote reports whether the dispatch must send no token remote
// (api.GitCloneConfig.TokenRemote), so that sciontool init, sciontool
// provision and the shared-workspace helper offer git no token at all. It is
// true when the recorded remote is not on the GitHub host and the token git
// would see is not positively project-bound (gitTokenProjectBound).
func (d *HTTPAgentDispatcher) withholdTokenRemote(td gitTokenDispatch) bool {
	if td.gitRemote == "" {
		return false // no token remote is sent anyway
	}
	if personalTokenHostAllowed(td.gitRemote, d.personalTokenGitHubHost()) {
		return false
	}
	return !gitTokenProjectBound(td)
}

// gitTokenProjectBound reports whether every GITHUB_TOKEN the agent can see
// is project-bound. The candidates are the env value and every
// environment-type secret projected to GITHUB_TOKEN. A secret candidate is
// project-bound only when the hub resolved it from the project scope. The env
// value is project-bound only when it equals the App installation token
// minted for the agent's own project in this dispatch, the project-scope
// GITHUB_TOKEN the dispatch itself put into the env, or a project-scope
// secret candidate. No candidate at all is not project-bound: the token then
// comes from somewhere the hub does not see (the caller's env at gather, or
// the broker).
func gitTokenProjectBound(td gitTokenDispatch) bool {
	bound := []string{td.appToken, td.projectToken}
	candidates := 0
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
		if s.Source != secret.ScopeProject {
			return false
		}
		bound = append(bound, s.Value)
	}
	if v := td.env[provision.GitTokenEnv]; v != "" {
		candidates++
		if !slices.ContainsFunc(bound, func(b string) bool {
			return b != "" && subtle.ConstantTimeCompare([]byte(v), []byte(b)) == 1
		}) {
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
func (d *HTTPAgentDispatcher) restrictPersonalTokenRemote(td gitTokenDispatch, gitClone, sharedClone **api.GitCloneConfig) {
	if (*gitClone == nil || (*gitClone).TokenRemote == "") && (*sharedClone == nil || (*sharedClone).TokenRemote == "") {
		return
	}
	if !d.withholdTokenRemote(td) {
		return
	}
	d.log.Info("git token not offered: the project's git remote is not on the GitHub host and the token is not the project's own",
		"project_id", td.projectID)
	*gitClone = withoutTokenRemote(*gitClone)
	*sharedClone = withoutTokenRemote(*sharedClone)
}

// restrictCreateRequestTokenRemote applies restrictPersonalTokenRemote to a
// create request, from the env and secrets it carries.
func (d *HTTPAgentDispatcher) restrictCreateRequestTokenRemote(req *RemoteCreateAgentRequest) {
	if req == nil || req.Config == nil {
		return
	}
	d.restrictPersonalTokenRemote(gitTokenDispatch{
		projectID:    req.ProjectID,
		gitRemote:    req.projectGitRemote,
		env:          req.ResolvedEnv,
		secrets:      req.ResolvedSecrets,
		appToken:     req.githubAppToken,
		projectToken: req.noAuthProjectToken,
	}, &req.Config.GitClone, &req.Config.SharedWorkspaceClone)
}
