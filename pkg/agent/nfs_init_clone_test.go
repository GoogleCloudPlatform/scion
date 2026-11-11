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

package agent

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
)

func TestNFSInitGitClone(t *testing.T) {
	perAgent := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	shared := &api.GitCloneConfig{URL: "https://example.com/shared.git"}

	cases := []struct {
		name string
		opts api.StartOptions
		want *api.GitCloneConfig
	}{
		{"per-agent clone", api.StartOptions{GitClone: perAgent}, perAgent},
		{"per-agent clone wins over shared settings", api.StartOptions{GitClone: perAgent, SharedWorkspace: true, SharedWorkspaceClone: shared}, perAgent},
		{"shared-plain git workspace", api.StartOptions{SharedWorkspace: true, SharedWorkspaceClone: shared}, shared},
		{"shared settings without a shared workspace", api.StartOptions{SharedWorkspaceClone: shared}, nil},
		{"shared workspace without clone settings", api.StartOptions{SharedWorkspace: true}, nil},
		{"shared settings without a URL", api.StartOptions{SharedWorkspace: true, SharedWorkspaceClone: &api.GitCloneConfig{}}, nil},
		{"nothing", api.StartOptions{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Same(t, tc.want, nfsInitGitClone(tc.opts))
		})
	}
}
