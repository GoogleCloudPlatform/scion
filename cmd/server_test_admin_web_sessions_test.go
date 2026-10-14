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
	"regexp"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin-kind test identity web sessions (ptone/scion#4240, phase WS4): the
// startup setting server.hub.test_identities.admin_web_sessions, whose
// value is the hub id it is bound to.

// B2: the setting with a level below admin (off, unset or member) refuses
// startup with a named error, and resolves nothing; it is never ignored.
func TestAdminWebSessions_RefusedBelowAdminLevel(t *testing.T) {
	for _, level := range []hub.TestMode{"", hub.TestModeOff, hub.TestModeMember} {
		in := tmMemberInputs()
		in.Level = level
		in.AdminWebSessions = tiaCmdHubID
		res, err := resolveTestMode(in)
		require.Error(t, err, "level %q", level)
		assert.ErrorIs(t, err, hub.ErrTestAdminWebSessionsBelowAdmin, "level %q", level)
		assert.Contains(t, err.Error(), "server.hub.test_identities.admin_web_sessions", "level %q", level)
		assert.Equal(t, testModeResolution{}, res, "level %q", level)
	}
}

// B2: at an admin level the value must be this hub's resolved hub id.
func TestAdminWebSessions_RefusedWithAnotherHubID(t *testing.T) {
	for _, level := range []hub.TestMode{hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		for _, value := range []string{"another-hub", tiaCmdHubID + " ", "TI-A0-HUB"} {
			in := tiaValidInputs()
			in.Level = level
			in.AdminWebSessions = value
			in.PolicyComplete = func() error { return nil }
			res, err := resolveTestMode(in)
			require.Error(t, err, "level %q value %q", level, value)
			assert.ErrorIs(t, err, hub.ErrTestAdminWebSessionsHubIDMismatch, "level %q value %q", level, value)
			assert.Equal(t, testModeResolution{}, res, "level %q value %q", level, value)
		}
	}
}

// B3 (startup half): at admin and super-admin the setting bound to the
// resolved hub id resolves into the hub configuration, next to the admin
// gate; unset, it stays off.
func TestAdminWebSessions_AdmittedAtAdminLevels(t *testing.T) {
	for _, level := range []hub.TestMode{hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		in := tiaValidInputs()
		in.Level = level
		in.AdminWebSessions = tiaCmdHubID
		// G3 is not under test here: the super-admin level is admitted
		// whatever the classification state of this binary.
		in.PolicyComplete = func() error { return nil }
		res, err := resolveTestMode(in)
		require.NoError(t, err, level)
		assert.Equal(t, hub.TestAdminWebSessionsConfig{HubID: tiaCmdHubID}, res.TestAdminWebSessions, level)
		assert.True(t, res.TestAdminGate.HubAdmin, level)
		assert.Equal(t, level == hub.TestModeSuperAdmin, res.TestAdminGate.SuperAdmin, level)

		in.AdminWebSessions = ""
		res, err = resolveTestMode(in)
		require.NoError(t, err, level)
		assert.Equal(t, hub.TestAdminWebSessionsConfig{}, res.TestAdminWebSessions, level)
	}
}

// B9: admin web sessions are refused on a Helm-deployed hub. The chart
// reserves --test-mode in hub.args ($unsafeToPass) and renders it only as
// --test-mode=member:<hub id> (G9), and the setting at the member level
// refuses startup. So a chart-rendered hub never starts with admin web
// sessions, whatever settings file it is given.
func TestAdminWebSessions_RefusedOnHelm(t *testing.T) {
	raw, err := os.ReadFile(chartDir + "/templates/_helpers.tpl")
	require.NoError(t, err)
	helpers := string(raw)

	unsafe := regexp.MustCompile(`\$unsafeToPass := list ([^\n]*)\}\}`).FindStringSubmatch(helpers)
	require.Len(t, unsafe, 2, "the chart's $unsafeToPass list")
	assert.Contains(t, unsafe[1], `"test-mode"`, "--test-mode is never passed through hub.args")

	rendered := regexp.MustCompile(`printf "--test-mode=([^:"]*)`).FindAllStringSubmatch(helpers, -1)
	require.NotEmpty(t, rendered, "the chart renders --test-mode somewhere")
	for _, m := range rendered {
		assert.Equal(t, string(hub.TestModeMember), m[1], "the chart renders only the member level")
	}

	in := tmMemberInputs()
	in.AdminWebSessions = tiaCmdHubID
	_, err = resolveTestMode(in)
	assert.ErrorIs(t, err, hub.ErrTestAdminWebSessionsBelowAdmin)
}
