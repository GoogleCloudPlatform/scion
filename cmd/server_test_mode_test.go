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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tmMemberInputs passes every member precondition on a workstation hub
// with federation and a shared secret configured: none of those are member
// preconditions.
func tmMemberInputs() testModeInputs {
	in := tiaValidInputs()
	in.Level = hub.TestModeMember
	in.Workstation = true
	in.Federation.Enabled = true
	in.SharedSigningSecret = true
	in.PreconfiguredSigningKey = true
	return in
}

// C1 (with ti-adm-em's decision): the parser resolves an unrecognised
// value to off, and the flag check refuses to start on any non-empty
// unrecognised value with a named error that lists the four levels and does
// not echo the value. An empty value is off and starts.
func TestTestMode_FlagUnrecognisedValuesRefused(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	for _, v := range []string{"memebr:" + tiaCmdHubID, "Member:" + tiaCmdHubID, "ADMIN:" + tiaCmdHubID, " admin:" + tiaCmdHubID, "superadmin:" + tiaCmdHubID, "1", "true", "on", "OFF", ":" + tiaCmdHubID, "admin;" + tiaCmdHubID} {
		err := validateTestModeFlagsEarly(tiaStartCmd(t, "--test-mode="+v))
		require.Error(t, err, v)
		assert.ErrorIs(t, err, hub.ErrTestModeUnrecognized, v)
		for _, level := range []string{"off", "member", "admin", "super-admin"} {
			assert.Contains(t, err.Error(), level, v)
		}
		if v != "1" && v != "on" && v != "true" {
			assert.NotContains(t, err.Error(), v, "the value is not echoed")
		}
		assert.Equal(t, hub.TestModeOff, testModeLevel, "nothing is granted: %q", v)
		assert.False(t, testModeRequested(), v)
	}
	// With --production too: refused as unrecognised.
	assert.ErrorIs(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=Admin:"+tiaCmdHubID)), hub.ErrTestModeUnrecognized)
	// Empty (explicit or absent) is off and starts; so does off.
	for _, args := range [][]string{{"--test-mode="}, {}, {"--test-mode=off"}, {"--test-mode=off:" + tiaCmdHubID}} {
		require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, args...)), args)
		assert.Equal(t, hub.TestModeOff, testModeLevel, args)
		assert.NoError(t, validateTestModeRequiresHub(false), args)
		res, err := resolveTestMode(testModeInputs{Level: testModeLevel, BindingHubID: testModeHubID})
		require.NoError(t, err, args)
		assert.Equal(t, testModeResolution{}, res, args)
	}
}

// C2/C3 for member: every member precondition missing, one at a time,
// refuses startup with its named error; the level is never lowered.
func TestTestMode_MemberEachPreconditionRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testModeInputs)
		want   error
	}{
		{"no hub", func(in *testModeInputs) { in.HubEnabled = false }, hub.ErrTestAdminRequiresHub},
		{"no user store", func(in *testModeInputs) { in.HasUserStore = false }, hub.ErrTestAdminRequiresUserStore},
		{"explicit --production", func(in *testModeInputs) { in.ProductionFlagExplicit = true }, hub.ErrTestAdminProductionFlag},
		{"no hub id binding", func(in *testModeInputs) { in.BindingHubID = "" }, hub.ErrTestAdminHubIDMismatch},
		{"binding differs from the resolved hub id", func(in *testModeInputs) { in.BindingHubID = "another-hub" }, hub.ErrTestAdminHubIDMismatch},
		{"no resolved hub id", func(in *testModeInputs) { in.ResolvedHubID = "" }, hub.ErrTestAdminHubIDMismatch},
		{"hub id not pinned", func(in *testModeInputs) { in.HubIDPinned = false }, hub.ErrTestModeHubIDNotPinned},
		{"hub class absent", func(in *testModeInputs) { in.HubClass = "" }, hub.ErrTestAdminHubClassNotTest},
		{"hub class case", func(in *testModeInputs) { in.HubClass = "Test" }, hub.ErrTestAdminHubClassNotTest},
		{"marker absent", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, testinfra.ErrMarkerAbsent }
		}, hub.ErrTestAdminMarkerAbsent},
		{"marker unreadable", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, errors.New("synthetic read failure") }
		}, hub.ErrTestAdminMarkerUnreadable},
		{"marker for another hub", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: "another-hub", Class: config.HubClassTest}, nil
			}
		}, hub.ErrTestAdminMarkerHubMismatch},
		{"marker class", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: tiaCmdHubID, Class: "prod"}, nil
			}
		}, hub.ErrTestAdminMarkerClassNotTest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tmMemberInputs()
			tc.mutate(&in)
			res, err := resolveTestMode(in)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), "--test-mode=member")
			var named *hub.TestAdminGateError
			require.True(t, errors.As(err, &named), "the error is named")
			assert.Equal(t, testModeResolution{}, res, "a refused level is never lowered")
		})
	}
}

// The member level does not need the admin-only preconditions, and admits
// exactly the member preconditions.
func TestTestMode_MemberStartsWithoutAdminPreconditions(t *testing.T) {
	res, err := resolveTestMode(tmMemberInputs())
	require.NoError(t, err)
	assert.True(t, res.EnableTestIdentities)
	assert.Equal(t, hub.TestAdminGateConfig{}, res.TestAdminGate)
	assert.Equal(t, hub.TestModeMember, res.TestMode.Level)
	assert.Equal(t, hub.TestModeMemberPreconditions(), res.TestMode.Preconditions)

	adm, err := resolveTestMode(tiaValidInputs())
	require.NoError(t, err)
	assert.Equal(t, append(append([]string(nil), hub.TestModeMemberPreconditions()...), hub.TestModeAdminPreconditions()...), adm.TestMode.Preconditions)
}

// --production is refused at every level other than off, both at the flag
// and at hub construction.
func TestTestMode_ProductionRefusedAtEveryNonOffLevel(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	for _, level := range []hub.TestMode{hub.TestModeMember, hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		err := validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode="+string(level)+":"+tiaCmdHubID))
		require.Error(t, err, level)
		// With a complete test-admin policy (A2, G3) super-admin passes G3
		// and is refused by G5 like the other levels; the incomplete-policy
		// refusal is TestTestMode_SuperAdminNeedsCompletePolicy.
		assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, level)
		in := tiaValidInputs()
		in.Level = level
		in.ProductionFlagExplicit = true
		res, err := resolveTestMode(in)
		require.Error(t, err, level)
		assert.Equal(t, testModeResolution{}, res, level)
		assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, level)
	}
	require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=off")))
}

// Admin tier A2 (G3): super-admin starts on a binary whose test-admin
// policy classification is complete, with both admin kinds and the
// policy-complete precondition; with an incomplete classification it
// refuses with the same named error as before, at the early flag check and
// in resolveTestMode.
func TestTestMode_SuperAdminNeedsCompletePolicy(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	in := tiaValidInputs()
	in.Level = hub.TestModeSuperAdmin
	res, err := resolveTestMode(in)
	require.NoError(t, err)
	assert.True(t, res.EnableTestIdentities)
	assert.True(t, res.TestAdminGate.HubAdmin)
	assert.True(t, res.TestAdminGate.SuperAdmin)
	assert.Contains(t, res.TestMode.Preconditions, hub.TestModePreconditionPolicyComplete)
	require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=super-admin:"+tiaCmdHubID)))
	resetTestModeFlags()

	incomplete := func() error { return hub.ErrTestAdminPolicyIncomplete }
	in.PolicyComplete = incomplete
	_, err = resolveTestMode(in)
	assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry)
	prev := testAdminPolicyComplete
	testAdminPolicyComplete = incomplete
	t.Cleanup(func() { testAdminPolicyComplete = prev })
	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=super-admin:"+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry)
	// The admin level does not need the policy.
	resetTestModeFlags()
	in.Level = hub.TestModeAdmin
	_, err = resolveTestMode(in)
	require.NoError(t, err)
}

// C5: the replaced flags refuse startup with a named error that points at
// --test-mode, alone or together with --test-mode (whatever their value).
func TestTestMode_RemovedFlagsRefused(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	cases := [][]string{
		{"--enable-test-identities"},
		{"--enable-test-identities=true"},
		{"--enable-test-identities=false"},
		{"--enable-test-admin-identities=" + tiaCmdHubID},
		{"--enable-test-super-admin-identities=" + tiaCmdHubID},
		{"--test-mode=member:" + tiaCmdHubID, "--enable-test-identities"},
		{"--test-mode=off", "--enable-test-admin-identities=" + tiaCmdHubID},
		{"--test-mode=admin:" + tiaCmdHubID, "--enable-test-admin-identities=" + tiaCmdHubID},
	}
	for _, args := range cases {
		err := validateTestModeFlagsEarly(tiaStartCmd(t, args...))
		require.Error(t, err, args)
		assert.ErrorIs(t, err, hub.ErrTestModeRemovedFlag, args)
		assert.Contains(t, err.Error(), "--test-mode", args)
	}
}

// C6: the capabilities each level grants equal the old flag combination.
// off = no flag; member = --enable-test-identities; admin = member plus
// --enable-test-admin-identities; super-admin = admin plus
// --enable-test-super-admin-identities (admitted since admin tier A2 with
// a complete test-admin policy). Only the preconditions differ: every
// level now requires the member preconditions (stricter than the old
// member flag).
func TestTestMode_CapabilityEquivalenceWithOldFlags(t *testing.T) {
	type oldFlags struct {
		testIdentities, admin, superAdmin bool
	}
	cases := []struct {
		level   hub.TestMode
		old     oldFlags
		refused error
	}{
		{hub.TestModeOff, oldFlags{}, nil},
		{hub.TestModeMember, oldFlags{testIdentities: true}, nil},
		{hub.TestModeAdmin, oldFlags{testIdentities: true, admin: true}, nil},
		{hub.TestModeSuperAdmin, oldFlags{testIdentities: true, admin: true, superAdmin: true}, nil},
	}
	for _, tc := range cases {
		t.Run(string(tc.level), func(t *testing.T) {
			// The level's capability table is the old flag combination.
			assert.Equal(t, hub.TestModeCapabilities{
				TestIdentities: tc.old.testIdentities, HubAdmin: tc.old.admin, SuperAdmin: tc.old.superAdmin,
			}, tc.level.Capabilities())

			in := tiaValidInputs()
			in.Level = tc.level
			res, err := resolveTestMode(in)
			if tc.refused != nil {
				assert.ErrorIs(t, err, tc.refused)
				return
			}
			require.NoError(t, err)
			// What the resolved level sets on hub.ServerConfig is exactly
			// what the old flags set.
			assert.Equal(t, tc.old.testIdentities, res.EnableTestIdentities)
			assert.Equal(t, tc.old.admin, res.TestAdminGate.HubAdmin)
			assert.Equal(t, tc.old.superAdmin, res.TestAdminGate.SuperAdmin)
			// And the hub's own construction check (the one hub.New runs)
			// accepts the resolved configuration.
			assert.NoError(t, hub.ValidateTestModeConfig(hub.ServerConfig{
				HubID:                in.ResolvedHubID,
				HubIDPinned:          in.HubIDPinned,
				EnableTestIdentities: res.EnableTestIdentities,
				TestAdminGate:        res.TestAdminGate,
				TestMode:             res.TestMode,
			}))
		})
	}
}

// The daemon parent runs the same flag checks before re-exec:
// resolveDaemonServerMode refuses removed flags, unrecognised values,
// super-admin and --production with a level, so none reaches the child.
func TestTestMode_DaemonParentRefuses(t *testing.T) {
	savedHosted, savedEndpoints := hostedMode, enableDebugEndpoints
	t.Cleanup(func() {
		hostedMode, enableDebugEndpoints = savedHosted, savedEndpoints
		resetTestModeFlags()
	})
	t.Setenv("SCION_SERVER_MODE", "")
	writeServerSettings(t, "server:\n  mode: workstation\n")
	cases := []struct {
		args []string
		want error
	}{
		{[]string{"--enable-test-identities"}, hub.ErrTestModeRemovedFlag},
		{[]string{"--enable-test-admin-identities=" + tiaCmdHubID}, hub.ErrTestModeRemovedFlag},
		{[]string{"--test-mode=memebr:" + tiaCmdHubID}, hub.ErrTestModeUnrecognized},
		// A complete test-admin policy admits super-admin (A2, G3); the
		// incomplete-policy refusal is TestTestMode_SuperAdminNeedsCompletePolicy.
		{[]string{"--test-mode=super-admin:" + tiaCmdHubID}, nil},
		{[]string{"--production", "--test-mode=member:" + tiaCmdHubID}, hub.ErrTestAdminProductionFlag},
		{[]string{"--hosted", "--test-mode=member:" + tiaCmdHubID}, nil},
		{nil, nil},
	}
	for _, tc := range cases {
		hostedMode, enableDebugEndpoints = false, false
		err := resolveDaemonServerMode(tiaStartCmd(t, tc.args...))
		if tc.want == nil {
			assert.NoError(t, err, tc.args)
			continue
		}
		assert.ErrorIs(t, err, tc.want, tc.args)
	}
}

// TM-h: every level but off refuses a derived (unpinned) hub id at each cmd
// layer, the post-config check (validateTestModeHubIDPinned) and
// resolveTestMode, with the named error that names server.hub.hub_id; a
// pinned id, from settings or from SCION_SERVER_HUB_HUBID, starts as
// before, and off starts with a derived id. hub.New's re-check is
// pkg/hub TestTestMode_NewRefusesUnpinnedHubIDAtEveryNonOffLevel.
func TestTestMode_UnpinnedHubIDRefusedAtEveryNonOffLevel(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	t.Setenv(hubIDEnvVar, "")
	// buildHubServerConfig resolves a derived id, which may persist
	// ~/.scion/hub-id: keep that out of the real home.
	t.Setenv("HOME", t.TempDir())
	derived := &config.GlobalConfig{}
	pinned := &config.GlobalConfig{}
	pinned.Hub.HubID = tiaCmdHubID

	assert.False(t, hubIDPinned(derived), "no settings hub_id and no env: derived")
	assert.False(t, hubIDPinned(nil))
	assert.True(t, hubIDPinned(pinned), "settings hub_id")
	assert.False(t, buildHubServerConfig(derived, "", "", nil, false, "", nil).HubIDPinned)
	assert.True(t, buildHubServerConfig(pinned, "", "", nil, false, "", nil).HubIDPinned)

	for _, level := range []hub.TestMode{hub.TestModeMember, hub.TestModeAdmin, hub.TestModeSuperAdmin} {
		t.Run(string(level), func(t *testing.T) {
			t.Cleanup(resetTestModeFlags)
			require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode="+string(level)+":"+tiaCmdHubID)))
			// The post-config check, before the store is opened.
			err := validateTestModeHubIDPinned(derived)
			require.ErrorIs(t, err, hub.ErrTestModeHubIDNotPinned)
			assert.Contains(t, err.Error(), "--test-mode="+string(level))
			assert.Contains(t, err.Error(), "server.hub.hub_id")
			assert.Contains(t, err.Error(), "hub_id_not_pinned")
			assert.NoError(t, validateTestModeHubIDPinned(pinned))

			// resolveTestMode.
			in := tiaValidInputs()
			in.Level = level
			in.HubIDPinned = false
			res, err := resolveTestMode(in)
			require.ErrorIs(t, err, hub.ErrTestModeHubIDNotPinned)
			assert.Contains(t, err.Error(), "server.hub.hub_id")
			var named *hub.TestAdminGateError
			require.True(t, errors.As(err, &named), "the error is named")
			assert.Equal(t, testModeResolution{}, res, "a refused level is never lowered")
			in.HubIDPinned = true
			res, err = resolveTestMode(in)
			require.NoError(t, err, "pinned: the level starts as before")
			assert.Equal(t, level, res.TestMode.Level)
			assert.Contains(t, res.TestMode.Preconditions, hub.TestModePreconditionHubIDPinned)
		})
	}

	t.Run("env pins the id", func(t *testing.T) {
		t.Cleanup(resetTestModeFlags)
		t.Setenv(hubIDEnvVar, tiaCmdHubID)
		assert.True(t, hubIDPinned(derived))
		require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=member:"+tiaCmdHubID)))
		assert.NoError(t, validateTestModeHubIDPinned(derived))
	})

	t.Run("off starts with a derived id", func(t *testing.T) {
		for _, args := range [][]string{nil, {"--test-mode=off"}, {"--test-mode="}} {
			resetTestModeFlags()
			require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, args...)), args)
			assert.NoError(t, validateTestModeHubIDPinned(derived), args)
			res, err := resolveTestMode(testModeInputs{Level: testModeLevel, ResolvedHubID: "derived-id"})
			require.NoError(t, err, args)
			assert.Equal(t, testModeResolution{}, res, args)
		}
	})
}
