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
// (provision.CloneTokenBound); it is not even resolved otherwise. For any
// other URL git runs without the token, with the hub host user's credential
// helpers and extra HTTP headers reset and askpass disabled
// (util.HostGitHardening.NoCredentialHelpers; see util.NoCredentialHelperEnv
// for what it does not cover).
func (s *Server) hostGitAccess(ctx context.Context, project *store.Project, cloneURL string) (string, util.HostGitHardening) {
	h := util.HostGitHardening{
		Config: append(provision.HostCloneGitConfig(), "protocol.http.allow=never"),
		Env:    []string{"GIT_ALLOW_PROTOCOL=" + provision.HostCloneProtocols},
	}
	if !provision.CloneTokenBound(project.GitRemote, cloneURL) {
		h.NoCredentialHelpers = true
		return "", h
	}
	return s.resolveCloneToken(ctx, project), h
}
