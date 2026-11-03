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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The startup setting server.hub.test_identities.offline_admin_delegations,
// whose value is the hub id it is bound to.

// Below an admin level (off, unset or member) the setting refuses startup
// with a named error and resolves nothing; it is never ignored.
func TestOfflineAdminDelegations_RefusedBelowAdminLevel(t *testing.T) {
	for _, level := range []hub.TestMode{"", hub.TestModeOff, hub.TestModeMember} {
		in := tmMemberInputs()
		in.Level = level
		in.OfflineAdminDelegations = tiaCmdHubID
		res, err := resolveTestMode(in)
		require.Error(t, err, "level %q", level)
		assert.ErrorIs(t, err, hub.ErrOfflineAdminDelegationsBelowAdmin, "level %q", level)
		assert.Contains(t, err.Error(), "server.hub.test_identities.offline_admin_delegations", "level %q", level)
		assert.Equal(t, testModeResolution{}, res, "level %q", level)
	}
}

// At an admin level the value must be this hub's resolved hub id.
func TestOfflineAdminDelegations_RefusedWithAnotherHubID(t *testing.T) {
	for _, level := range []hub.TestMode{hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		for _, value := range []string{"another-hub", tiaCmdHubID + " ", "TI-A0-HUB"} {
			in := tiaValidInputs()
			in.Level = level
			in.OfflineAdminDelegations = value
			in.PolicyComplete = func() error { return nil }
			res, err := resolveTestMode(in)
			require.Error(t, err, "level %q value %q", level, value)
			assert.ErrorIs(t, err, hub.ErrOfflineAdminDelegationsHubIDMismatch, "level %q value %q", level, value)
			assert.Equal(t, testModeResolution{}, res, "level %q value %q", level, value)
		}
	}
}

// At admin and super-admin the setting bound to the resolved hub id
// resolves into the hub configuration; unset, it stays off. It is
// independent of the admin web session setting.
func TestOfflineAdminDelegations_AdmittedAtAdminLevels(t *testing.T) {
	for _, level := range []hub.TestMode{hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		in := tiaValidInputs()
		in.Level = level
		in.OfflineAdminDelegations = tiaCmdHubID
		in.PolicyComplete = func() error { return nil }
		res, err := resolveTestMode(in)
		require.NoError(t, err, level)
		assert.Equal(t, hub.OfflineAdminDelegationsConfig{HubID: tiaCmdHubID}, res.OfflineAdminDelegations, level)
		assert.Equal(t, hub.TestAdminWebSessionsConfig{}, res.TestAdminWebSessions, level)

		in.OfflineAdminDelegations = ""
		res, err = resolveTestMode(in)
		require.NoError(t, err, level)
		assert.Equal(t, hub.OfflineAdminDelegationsConfig{}, res.OfflineAdminDelegations, level)
	}
}

// Keyring signing refuses the admin levels (G7), so startup with the
// offline hub-admin delegation setting set is refused there too: the
// setting can never be on in keyring mode.
func TestOfflineAdminDelegations_RefusedWithKeyringSigning(t *testing.T) {
	for _, level := range []hub.TestMode{hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		in := tiaValidInputs()
		in.Level = level
		in.KeyringSigning = true
		in.OfflineAdminDelegations = tiaCmdHubID
		in.PolicyComplete = func() error { return nil }
		res, err := resolveTestMode(in)
		require.Error(t, err, level)
		assert.ErrorIs(t, err, hub.ErrTestAdminKeyringSigning, level)
		assert.Equal(t, testModeResolution{}, res, level)
		assert.Equal(t, hub.OfflineAdminDelegationsConfig{}, res.OfflineAdminDelegations, level)
	}
}
