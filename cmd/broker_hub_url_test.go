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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A broker registration takes its hub URL with the hub list's rule: a URL
// with user info is refused before any request, and nothing is saved.
func TestBrokerJoin_HubURLWithUserInfoRefused(t *testing.T) {
	brokerHubURLRefused(t, func() error {
		t.Setenv(envBrokerJoinToken, "scion_join_abc")
		brokerJoinBrokerID = "11111111-2222-3333-4444-555555555555"
		return runBrokerJoin(brokerJoinCmd, nil)
	})
}

func TestBrokerRegister_HubURLWithUserInfoRefused(t *testing.T) {
	brokerHubURLRefused(t, func() error { return runBrokerRegister(brokerRegisterCmd, nil) })
}

func TestBrokerRegisterInstance_HubURLWithUserInfoRefused(t *testing.T) {
	brokerHubURLRefused(t, func() error { return runBrokerRegisterInstance(brokerRegisterCmd, "inst") })
}

// brokerHubURLRefused runs a registration against a hub URL with user info
// and checks that it fails before any request and saves nothing. It does
// not call t.Helper, so a failure names its own line.
func brokerHubURLRefused(t *testing.T, run func() error) {
	isolateJoinEnv(t)
	origInstance := brokerRegisterInstance
	t.Cleanup(func() { brokerRegisterInstance = origInstance })
	brokerRegisterInstance = ""
	if f := brokerRegisterCmd.Flags().Lookup("broker-name"); f != nil {
		origChanged := f.Changed
		t.Cleanup(func() { f.Changed = origChanged })
		f.Changed = false
	}
	home, _ := brokerTestHome(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	projectPath = setupSecretProject(t, home, srv.URL)
	// The hub of the invocation (as --hub would set it); not written to
	// any file.
	useHubForTest(t, strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1))

	err := run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid hub URL")
	assert.Contains(t, err.Error(), "http://user:xxxxx@127.0.0.1:")
	assert.NotContains(t, err.Error(), "s3cret")
	assert.Zero(t, requests.Load(), "no request is sent to the hub")
	require.NoError(t, filepath.WalkDir(home, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		assert.NotContains(t, string(data), "s3cret", "%s holds the user info", path)
		return nil
	}))
}
