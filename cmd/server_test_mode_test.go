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
		if level == hub.TestModeSuperAdmin {
			// G3 runs first: super-admin is refused whatever else is set.
			assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry, level)
		} else {
			assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, level)
		}
		in := tiaValidInputs()
		in.Level = level
		in.ProductionFlagExplicit = true
		res, err := resolveTestMode(in)
		require.Error(t, err, level)
		assert.Equal(t, testModeResolution{}, res, level)
		if level != hub.TestModeSuperAdmin {
			assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, level)
		}
	}
	require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=off")))
}

// super-admin still refuses to start with the same named error as the old
// --enable-test-super-admin-identities, until phase A2.
func TestTestMode_SuperAdminStillRefused(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	in := tiaValidInputs()
	in.Level = hub.TestModeSuperAdmin
	_, err := resolveTestMode(in)
	assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry)
	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=super-admin:"+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry)
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
// --enable-test-super-admin-identities, which is refused with the same
// named error as before. Only the preconditions differ: every level now
// requires the member preconditions (stricter than the old member flag).
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
		{hub.TestModeSuperAdmin, oldFlags{testIdentities: true, admin: true, superAdmin: true}, hub.ErrTestSuperAdminRequiresPolicyRegistry},
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
		{[]string{"--test-mode=super-admin:" + tiaCmdHubID}, hub.ErrTestSuperAdminRequiresPolicyRegistry},
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
