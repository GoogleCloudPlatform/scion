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
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hostedDevAuthRefusalText = "dev auth is not allowed in hosted mode"

// hostedDevAuthTestHubConfig is the config of a hub that passes the
// member-level test mode gate for tiaCmdHubID: the hub id pinned in
// server.hub.hub_id and server.hub.class: test.
func hostedDevAuthTestHubConfig() *config.GlobalConfig {
	cfg := &config.GlobalConfig{}
	cfg.Hub.HubID = tiaCmdHubID
	cfg.Hub.HubClass = config.HubClassTest
	return cfg
}

// TestValidateHostedDevAuthTestHub: on a hosted server dev auth is allowed
// only with --test-mode and every member-level gate condition met (user
// store, hub id binding, pinned hub id, hub class test, the marker for this
// hub id); without --test-mode, or with any condition unmet, startup is
// refused. Workstation mode and hosted without dev auth are not affected.
func TestValidateHostedDevAuthTestHub(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	savedHub := enableHub
	t.Cleanup(func() { enableHub = savedHub })
	enableHub = true
	t.Setenv(config.HubIDEnvVar, "")

	request := func(t *testing.T, args ...string) {
		t.Helper()
		require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, append([]string{"--hosted"}, args...)...)))
	}

	t.Run("all conditions met: allowed", func(t *testing.T) {
		request(t, "--test-mode=member:"+tiaCmdHubID)
		require.NoError(t, validateHostedDevAuthTestHub(hostedDevAuthTestHubConfig(), true, true, true, tiaMarker))
	})
	t.Run("admin level, member conditions met: allowed here", func(t *testing.T) {
		// The admin preconditions are checked again at hub construction.
		request(t, "--test-mode=admin:"+tiaCmdHubID)
		require.NoError(t, validateHostedDevAuthTestHub(hostedDevAuthTestHubConfig(), true, true, true, tiaMarker))
	})
	t.Run("no --test-mode: refused", func(t *testing.T) {
		request(t)
		err := validateHostedDevAuthTestHub(hostedDevAuthTestHubConfig(), true, true, true, tiaMarker)
		require.Error(t, err)
		assert.Contains(t, err.Error(), hostedDevAuthRefusalText)
	})
	t.Run("--test-mode=off: refused", func(t *testing.T) {
		request(t, "--test-mode=off")
		err := validateHostedDevAuthTestHub(hostedDevAuthTestHubConfig(), true, true, true, tiaMarker)
		require.Error(t, err)
		assert.Contains(t, err.Error(), hostedDevAuthRefusalText)
	})

	for _, tc := range []struct {
		name     string
		binding  string
		mutate   func(cfg *config.GlobalConfig)
		hasStore bool
		marker   func() (*testinfra.Marker, error)
		want     error
	}{
		{name: "no user store", binding: tiaCmdHubID, hasStore: false, marker: tiaMarker, want: hub.ErrTestAdminRequiresUserStore},
		{name: "binding differs from the hub id", binding: "another-hub", hasStore: true, marker: tiaMarker, want: hub.ErrTestAdminHubIDMismatch},
		{name: "hub id not pinned", binding: tiaCmdHubID, mutate: func(cfg *config.GlobalConfig) { cfg.Hub.HubID = "" }, hasStore: true, marker: tiaMarker},
		{name: "hub class not test", binding: tiaCmdHubID, mutate: func(cfg *config.GlobalConfig) { cfg.Hub.HubClass = "" }, hasStore: true, marker: tiaMarker, want: hub.ErrTestAdminHubClassNotTest},
		{name: "marker absent", binding: tiaCmdHubID, hasStore: true, marker: func() (*testinfra.Marker, error) { return nil, testinfra.ErrMarkerAbsent }, want: hub.ErrTestAdminMarkerAbsent},
		{name: "marker unreadable", binding: tiaCmdHubID, hasStore: true, marker: func() (*testinfra.Marker, error) { return nil, errors.New("synthetic read failure") }, want: hub.ErrTestAdminMarkerUnreadable},
		{name: "marker for another hub", binding: tiaCmdHubID, hasStore: true, marker: func() (*testinfra.Marker, error) {
			return &testinfra.Marker{HubID: "another-hub", Class: config.HubClassTest}, nil
		}},
	} {
		t.Run(tc.name+": refused", func(t *testing.T) {
			request(t, "--test-mode=member:"+tc.binding)
			cfg := hostedDevAuthTestHubConfig()
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			err := validateHostedDevAuthTestHub(cfg, true, true, tc.hasStore, tc.marker)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "dev auth in hosted mode is allowed only on a test hub")
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}

	t.Run("workstation or dev auth off: not affected", func(t *testing.T) {
		request(t)
		absent := func() (*testinfra.Marker, error) { return nil, testinfra.ErrMarkerAbsent }
		assert.NoError(t, validateHostedDevAuthTestHub(&config.GlobalConfig{}, false, true, false, absent))
		assert.NoError(t, validateHostedDevAuthTestHub(&config.GlobalConfig{}, true, false, false, absent))
	})
}

// TestValidateHostedDevAuth_DefersOnlyToTestMode: the load-time check
// refuses hosted dev auth unless --test-mode requested a level, in which case
// validateHostedDevAuthTestHub decides once the store is open.
func TestValidateHostedDevAuth_DefersOnlyToTestMode(t *testing.T) {
	err := validateHostedDevAuth(true, true, false)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), hostedDevAuthRefusalText))
	assert.NoError(t, validateHostedDevAuth(true, true, true))
	assert.NoError(t, validateHostedDevAuth(false, true, false))
	assert.NoError(t, validateHostedDevAuth(true, false, false))
}
