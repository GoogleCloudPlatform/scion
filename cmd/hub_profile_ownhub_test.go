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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
)

// X5 (ptone/scion#4240): the own-hub check by hub identity, end to end in
// agent mode. The agent's own hub is another local server than the
// target, so the names differ; the hub ids decide.
func TestHubProfileOwnHubByID_AgentMode(t *testing.T) {
	e := newX3bEnv(t)
	id := e.issue(t, "owner")
	dir := resolvedTempDir(t)

	t.Run("same hub id under another name: refused, nothing sent or written", func(t *testing.T) {
		own := hubprofiletest.NewHealthHub(t, "hub-shared")
		e.target.SetHubID("hub-shared")
		t.Cleanup(func() { e.target.SetHubID("") })
		t.Setenv("SCION_HUB_ENDPOINT", own.Srv.URL)
		before := len(e.target.Recorded())

		out := filepath.Join(dir, "own-alias.txt")
		_, err := e.run(t, "--hub-profile", "t1", "hub", "test-identity", "web-code", id, "--out", out)
		require.ErrorIs(t, err, hubprofile.ErrOwnHub)
		_, statErr := os.Lstat(out)
		assert.True(t, os.IsNotExist(statErr), "no code file")
		assert.Len(t, e.target.WebCodes(), 0)
		for _, rq := range e.target.Recorded()[before:] {
			assert.Empty(t, rq.Header.Get("Authorization"), "no profile credential reached the target: %s", rq.Path)
		}

		_, err = e.run(t, "--hub-profile", "t1", "hub", "test-identity", "list")
		require.ErrorIs(t, err, hubprofile.ErrOwnHub)

		for _, rq := range own.Recorded() {
			assert.Equal(t, "/healthz", rq.Path)
			assert.Empty(t, credentialHeaderNamesCmd(rq.Header), "no credential to the agent's own hub")
		}
	})

	t.Run("different hub ids: allowed", func(t *testing.T) {
		own := hubprofiletest.NewHealthHub(t, "hub-own")
		e.target.SetHubID("hub-t1")
		t.Cleanup(func() { e.target.SetHubID("") })
		t.Setenv("SCION_HUB_ENDPOINT", own.Srv.URL)

		out := filepath.Join(dir, "other.txt")
		r, err := e.run(t, "--hub-profile", "t1", "hub", "test-identity", "web-code", id, "--out", out)
		require.NoError(t, err, r.stdout+r.stderr)
		assert.Len(t, e.target.WebCodes(), 1)
		require.Len(t, own.Recorded(), 1)
		assert.Empty(t, credentialHeaderNamesCmd(own.Recorded()[0].Header))
	})

	e.assertClean(t)
}

// credentialHeaderNamesCmd returns the names of the headers of hd that can
// carry a credential.
func credentialHeaderNamesCmd(hd map[string][]string) []string {
	var out []string
	for k := range hd {
		switch k {
		case "Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Scion-Agent-Token", "X-Scion-Broker-Token", "X-Scion-Federation-Token", "X-Scion-Dev-Token", "X-Scion-Issuer-Delegation":
			out = append(out, k)
		}
	}
	return out
}
