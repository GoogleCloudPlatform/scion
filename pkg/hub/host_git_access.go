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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// hostGitAccess returns the token and the hardening for a git clone or
// fetch the hub runs on its own host against cloneURL, a project's clone
// URL (already accepted by provision.CheckHostCloneURL).
//
// The transport is limited to https, ssh and git (provision.HostCloneGitConfig,
// plus protocol.http.allow=never over the fetch's network allow-list, and
// GIT_ALLOW_PROTOCOL). The project's token is offered only when cloneURL
// resolves to the project's recorded git remote over https
// (provision.CloneTokenBound); it is not even resolved otherwise, and the
// creator's personal token only for a remote on the GitHub host
// (resolveCloneToken). Whenever no project credential is offered (any other
// URL, or the recorded remote itself with none resolved), git runs
// anonymously (util.HostGitHardening.NoCredentialHelpers). On every clone and
// fetch, with or without a token, git offers no credential of the hub host's
// own (util.HostGitRemoteEnv).
func (s *Server) hostGitAccess(ctx context.Context, project *store.Project, cloneURL string) (string, util.HostGitHardening) {
	h := util.HostGitHardening{
		Config: append(provision.HostCloneGitConfig(), "protocol.http.allow=never"),
		Env:    []string{"GIT_ALLOW_PROTOCOL=" + provision.HostCloneProtocols},
	}
	if !provision.CloneTokenBound(project.GitRemote, cloneURL) {
		h.NoCredentialHelpers = true
		return "", h
	}
	token := s.resolveCloneToken(ctx, project)
	if token == "" {
		// No project credential: the fetch runs anonymously.
		h.NoCredentialHelpers = true
	}
	return token, h
}

// errSSHCloneURLNeedsProjectSecret refuses a hub-host clone or fetch of an ssh
// repository URL when the project has no git credential. Over ssh, git would
// authenticate with the hub host's own ssh identity (its agent and keys), and
// git operations without a project secret use no host credentials. The text
// names no URL.
var errSSHCloneURLNeedsProjectSecret = errors.New("an ssh repository URL requires a project git secret")

// isSSHCloneURL reports whether cloneURL uses the ssh transport: an ssh://
// URL or one of the scheme aliases git also runs over ssh (git+ssh://,
// ssh+git://), in any letter case, or an scp-style [user@]host:path address
// (the only scheme-less form provision.CheckHostCloneURL accepts). Other
// forms git could reach a remote with (ext::, file://, local paths) are
// refused by CheckHostCloneURL before this is consulted.
func isSSHCloneURL(cloneURL string) bool {
	u := strings.TrimSpace(cloneURL)
	if scheme, _, ok := strings.Cut(u, "://"); ok {
		switch strings.ToLower(scheme) {
		case "ssh", "git+ssh", "ssh+git":
			return true
		}
		return false
	}
	return strings.Contains(u, ":")
}

// sshCloneURLRefused returns errSSHCloneURLNeedsProjectSecret for an ssh clone
// URL when the project has no git credential (haveCredential false), and nil
// otherwise.
func sshCloneURLRefused(cloneURL string, haveCredential bool) error {
	if isSSHCloneURL(cloneURL) && !haveCredential {
		return errSSHCloneURLNeedsProjectSecret
	}
	return nil
}

// refuseSSHWithoutProjectSecret applies sshCloneURLRefused to a hub-host
// clone or fetch of project from cloneURL, resolving the project's git
// credential only for an ssh URL.
func (s *Server) refuseSSHWithoutProjectSecret(ctx context.Context, project *store.Project, cloneURL string) error {
	if !isSSHCloneURL(cloneURL) {
		return nil
	}
	return sshCloneURLRefused(cloneURL, s.resolveCloneToken(ctx, project) != "")
}
