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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkDispatchCloneURLs refuses a per-agent or shared-workspace clone URL
// that provisioning refuses, with the refusal (no URL) after the mode, and
// accepts https, ssh, scp-style and git URLs and an empty spec.
func TestCheckDispatchCloneURLs(t *testing.T) {
	for _, u := range []string{"/srv/refused.example/repo", "file:///srv/refused.example/repo", "http://refused.example/org/repo.git", "ext::sh -c refused.example", "-urefused.example"} {
		err := checkDispatchCloneURLs(WorkspaceDispatchSpec{GitClone: &api.GitCloneConfig{URL: u}})
		require.ErrorIs(t, err, provision.ErrHostCloneURLRefused, u)
		assert.True(t, strings.HasPrefix(err.Error(), "clone-per-agent: "), err.Error())
		assert.NotContains(t, err.Error(), "refused.example")

		err = checkDispatchCloneURLs(WorkspaceDispatchSpec{SharedWorkspaceClone: &api.GitCloneConfig{URL: u}})
		require.ErrorIs(t, err, provision.ErrHostCloneURLRefused, u)
		assert.True(t, strings.HasPrefix(err.Error(), "shared-workspace: "), err.Error())
	}
	for _, u := range []string{"https://github.com/org/repo.git", "ssh://git@github.com/org/repo.git", "git@github.com:org/repo.git", "git://github.com/org/repo.git"} {
		assert.NoError(t, checkDispatchCloneURLs(WorkspaceDispatchSpec{GitClone: &api.GitCloneConfig{URL: u}, SharedWorkspaceClone: &api.GitCloneConfig{URL: u}}), u)
	}
	assert.NoError(t, checkDispatchCloneURLs(WorkspaceDispatchSpec{}))
}

// A create dispatch refused for its clone URL answers 422 with the fixed
// refusal text, naming no URL.
func TestDispatchCreateErrorResponse_RefusedCloneURL(t *testing.T) {
	w := httptest.NewRecorder()
	err := fmt.Errorf("DispatchAgentCreate: %w", checkDispatchCloneURLs(WorkspaceDispatchSpec{GitClone: &api.GitCloneConfig{URL: "http://refused.example/org/repo.git"}}))
	require.True(t, errors.Is(err, provision.ErrHostCloneURLRefused))
	dispatchCreateErrorResponse(w, err, "agent-1")
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), provision.ErrHostCloneURLRefused.Error())
	assert.NotContains(t, w.Body.String(), "refused.example")
}
