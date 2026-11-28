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

package cmd

import (
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// remoteFetchCredential returns the GitHub credential the CLI sends when it
// fetches source itself, or an anonymous githubfetch.Credential. For a GitHub
// folder URL it reads the caller's environment in the same order the Hub uses
// for project credentials: GH_{OWNER}__{REPO}, then GH_{OWNER} (see
// pkg/ghauth), then GITHUB_TOKEN. Other sources (archives, rclone, other
// hosts) read nothing. Only the variable name is logged, never its value.
func remoteFetchCredential(source string) githubfetch.Credential {
	cred, name := config.EnvGitHubCredentialWithName(source)
	if name != "" {
		owner, repo, _ := config.GitHubRepoForURL(source)
		util.Debugf("github: using credential %s for %s/%s", name, owner, repo)
	}
	return cred
}
