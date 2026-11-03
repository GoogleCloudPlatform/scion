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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitRemoteLookupContext returns a HubContext whose hub lists no projects,
// as the hub answers a user access token without project:list, and changes
// into a checkout whose origin is remote.
func gitRemoteLookupContext(t *testing.T, remote string, kind hubsync.CredentialKind) *HubContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}, "totalCount": 0})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	t.Chdir(repo)
	return &HubContext{Client: client, Endpoint: srv.URL, CredentialKind: kind}
}

// TestResolveProjectIDByGitRemote_ScopedTokenNamesMissingScopes covers the
// git-remote lookup of a checkout under a user access token
// (ptone/scion#7068): an empty result names project:read and project:list
// instead of only suggesting 'scion hub link'.
func TestResolveProjectIDByGitRemote_ScopedTokenNamesMissingScopes(t *testing.T) {
	const remote = "https://github.com/acme/widgets.git"
	hubCtx := gitRemoteLookupContext(t, remote, hubsync.CredentialKindHubToken)
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_testtoken")

	_, err := resolveProjectIDByGitRemote(hubCtx, false)
	require.True(t, errors.Is(err, hubsync.ErrScopedTokenProjectNotFound), "err = %v", err)
	for _, want := range []string{remote, "project:read", "project:list", "project:manage does not include them"} {
		assert.Contains(t, err.Error(), want)
	}
}

// TestResolveProjectIDByGitRemote_SessionKeepsLinkHint pins that a caller
// without a user access token keeps the 'scion hub link' message.
func TestResolveProjectIDByGitRemote_SessionKeepsLinkHint(t *testing.T) {
	const remote = "https://github.com/acme/widgets.git"
	hubCtx := gitRemoteLookupContext(t, remote, hubsync.CredentialKindOAuth)
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_testtoken") // set but not the credential in use

	_, err := resolveProjectIDByGitRemote(hubCtx, false)
	require.Error(t, err)
	assert.False(t, errors.Is(err, hubsync.ErrScopedTokenProjectNotFound), "err = %v", err)
	assert.Contains(t, err.Error(), "no project found for git remote: "+remote)
	assert.NotContains(t, err.Error(), "project:list")
}
