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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tiaCmdHubID = "ti-a0-hub"

// tiaMarker is a valid marker for tiaCmdHubID.
func tiaMarker() (*testinfra.Marker, error) {
	return &testinfra.Marker{HubID: tiaCmdHubID, Class: config.HubClassTest, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}, nil
}

// tiaValidInputs passes every gate condition for --test-mode=admin (a
// --hosted hub: not a workstation, no explicit --production).
func tiaValidInputs() testModeInputs {
	return testModeInputs{
		Level:         hub.TestModeAdmin,
		BindingHubID:  tiaCmdHubID,
		HubEnabled:    true,
		HasUserStore:  true,
		ResolvedHubID: tiaCmdHubID,
		Workstation:   false,
		HubClass:      config.HubClassTest,
		ReadMarker:    tiaMarker,
	}
}

func TestValidateTestAdminIdentities_HostedWithEverythingValidStarts(t *testing.T) {
	res, err := resolveTestMode(tiaValidInputs())
	require.NoError(t, err)
	gate := res.TestAdminGate
	assert.True(t, gate.HubAdmin)
	assert.False(t, gate.SuperAdmin)
	assert.Equal(t, tiaCmdHubID, gate.HubID)
	assert.Equal(t, tiaCmdHubID, gate.MarkerHubID)
	assert.Equal(t, config.HubClassTest, gate.MarkerClass)
	assert.Equal(t, config.HubClassTest, gate.HubClass)
	assert.False(t, gate.MarkerCreatedAt.IsZero())
	assert.True(t, res.EnableTestIdentities, "admin implies member")
	assert.Equal(t, hub.TestModeAdmin, res.TestMode.Level)
}

func TestValidateTestAdminIdentities_OffWithoutFlags(t *testing.T) {
	in := tiaValidInputs()
	in.Level = hub.TestModeOff
	in.HubClass = ""
	in.ReadMarker = nil
	res, err := resolveTestMode(in)
	require.NoError(t, err)
	assert.Equal(t, testModeResolution{}, res)
	in.Level = ""
	res, err = resolveTestMode(in)
	require.NoError(t, err)
	assert.Equal(t, testModeResolution{}, res, "an empty level is off")
}

// Each condition missing, one at a time, refuses --test-mode=admin with its
// named error. (Before --test-mode, "G1 test identities off" was a case
// here: --test-mode=admin now always turns test identities on, so that
// combination can no longer be expressed. hub.New still refuses it, see
// pkg/hub TestNew_TestAdminGateRefusals.)
func TestValidateTestAdminIdentities_EachConditionRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testModeInputs)
		want   error
	}{
		{"G1 no user store", func(in *testModeInputs) { in.HasUserStore = false }, hub.ErrTestAdminRequiresUserStore},
		{"G1 no hub", func(in *testModeInputs) { in.HubEnabled = false }, hub.ErrTestAdminRequiresHub},
		{"G2 value differs from PersistentHubID", func(in *testModeInputs) { in.BindingHubID = "another-hub" }, hub.ErrTestAdminHubIDMismatch},
		{"G2 empty value", func(in *testModeInputs) { in.BindingHubID = "" }, hub.ErrTestAdminHubIDMismatch},
		{"G2 whitespace value", func(in *testModeInputs) { in.BindingHubID = " " + tiaCmdHubID }, hub.ErrTestAdminHubIDMismatch},
		{"G3 super-admin level with an incomplete policy", func(in *testModeInputs) {
			in.Level = hub.TestModeSuperAdmin
			in.PolicyComplete = func() error { return hub.ErrTestAdminPolicyIncomplete }
		}, hub.ErrTestSuperAdminRequiresPolicyRegistry},
		{"G4 class absent", func(in *testModeInputs) { in.HubClass = "" }, hub.ErrTestAdminHubClassNotTest},
		{"G4 class other", func(in *testModeInputs) { in.HubClass = "production" }, hub.ErrTestAdminHubClassNotTest},
		{"G4 class case", func(in *testModeInputs) { in.HubClass = "TEST" }, hub.ErrTestAdminHubClassNotTest},
		{"G5 explicit --production", func(in *testModeInputs) { in.ProductionFlagExplicit = true }, hub.ErrTestAdminProductionFlag},
		{"G6 marker absent", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, testinfra.ErrMarkerAbsent }
		}, hub.ErrTestAdminMarkerAbsent},
		{"G6 no marker reader", func(in *testModeInputs) { in.ReadMarker = nil }, hub.ErrTestAdminMarkerAbsent},
		{"G6 marker unreadable", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, errors.New("synthetic read failure") }
		}, hub.ErrTestAdminMarkerUnreadable},
		{"G6 marker hub id differs", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: "another-hub", Class: config.HubClassTest}, nil
			}
		}, hub.ErrTestAdminMarkerHubMismatch},
		{"G6 marker class", func(in *testModeInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: tiaCmdHubID, Class: "prod"}, nil
			}
		}, hub.ErrTestAdminMarkerClassNotTest},
		{"G7 shared signing secret", func(in *testModeInputs) { in.SharedSigningSecret = true }, hub.ErrTestAdminSharedSigningSecret},
		{"G7 pre-configured signing key", func(in *testModeInputs) { in.PreconfiguredSigningKey = true }, hub.ErrTestAdminPreconfiguredSigningKey},
		{"G8 federation enabled", func(in *testModeInputs) { in.Federation.Enabled = true }, hub.ErrTestAdminFederationConfigured},
		{"G8 federation issuers", func(in *testModeInputs) {
			in.Federation.TrustedIssuers = []config.TrustedIssuerConfig{{IssuerURL: "https://issuer.example.invalid"}}
		}, hub.ErrTestAdminFederationConfigured},
		{"G10 workstation hub", func(in *testModeInputs) { in.Workstation = true }, hub.ErrTestAdminWorkstationHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tiaValidInputs()
			tc.mutate(&in)
			res, err := resolveTestMode(in)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.want), "got %v, want %v", err, tc.want)
			assert.Equal(t, testModeResolution{}, res, "a refused level is off (never lowered)")
			var named *hub.TestAdminGateError
			require.True(t, errors.As(err, &named), "the error is named")
			assert.Contains(t, err.Error(), named.Name)
			assert.Contains(t, err.Error(), "--test-mode="+string(in.Level), "the error names the requested level")
		})
	}
}

// tiaStartCmd parses args with the server-start flags the gate reads.
func tiaStartCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	testModeValue, removedTestFlagValue = "", ""
	var hosted bool
	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().BoolVar(&hosted, "hosted", false, "")
	c.Flags().BoolVar(&hosted, "production", false, "")
	registerTestModeFlags(c)
	c.SetArgs(args)
	require.NoError(t, c.Execute())
	return c
}

func resetTestModeFlags() {
	testModeValue, removedTestFlagValue = "", ""
	testModeLevel, testModeHubID, productionFlagIsExplicit = "", "", false
}

func TestValidateTestAdminFlagsEarly(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	err := validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=admin:"+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, "explicit --production is refused")
	assert.True(t, productionFlagIsExplicit)

	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=admin:"+tiaCmdHubID))
	assert.NoError(t, err, "--hosted is allowed")
	assert.Equal(t, hub.TestModeAdmin, testModeLevel)
	assert.False(t, productionFlagIsExplicit)
	assert.Equal(t, tiaCmdHubID, testModeHubID)

	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--hosted", "--test-mode=super-admin:"+tiaCmdHubID))
	assert.NoError(t, err, "super-admin is admitted with a complete test-admin policy (G3)")
	assert.Equal(t, hub.TestModeSuperAdmin, testModeLevel)
	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=super-admin:"+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, "explicit --production is refused for super-admin too")

	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--production"))
	assert.NoError(t, err, "--production alone is not this gate's concern")

	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--production", "--test-mode=off"))
	assert.NoError(t, err, "--production with off is allowed")

	err = validateTestModeFlagsEarly(tiaStartCmd(t, "--test-mode=admin:"))
	assert.NoError(t, err, "an empty binding is caught by G2 at hub construction")
	assert.Equal(t, hub.TestModeAdmin, testModeLevel)

	require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t, "--test-mode=member:"+tiaCmdHubID)))
	assert.ErrorIs(t, validateTestModeRequiresHub(false), hub.ErrTestAdminRequiresHub)
	assert.NoError(t, validateTestModeRequiresHub(true))
	require.NoError(t, validateTestModeFlagsEarly(tiaStartCmd(t)))
	assert.NoError(t, validateTestModeRequiresHub(false), "off needs no hub")
}

func TestTestAdminFlags_RegisteredOnServerStart(t *testing.T) {
	f := serverStartCmd.Flags().Lookup(testModeFlag)
	require.NotNil(t, f)
	assert.Equal(t, "string", f.Value.Type())
	assert.Equal(t, "", f.DefValue)
	assert.False(t, f.Hidden)
	for _, name := range removedTestFlags {
		f := serverStartCmd.Flags().Lookup(name)
		require.NotNil(t, f, name)
		assert.True(t, f.Hidden, "%s is kept only to refuse it", name)
	}
}

func TestBuildDaemonStartArgsForwardsTestAdminFlags(t *testing.T) {
	t.Cleanup(resetTestModeFlags)
	args := buildDaemonStartArgs(tiaStartCmd(t, "--test-mode=admin:"+tiaCmdHubID))
	assert.Contains(t, args, "--test-mode=admin:"+tiaCmdHubID)
	args = buildDaemonStartArgs(tiaStartCmd(t, "--test-mode=member:"+tiaCmdHubID))
	assert.Contains(t, args, "--test-mode=member:"+tiaCmdHubID)
	for _, a := range buildDaemonStartArgs(tiaStartCmd(t)) {
		assert.False(t, strings.HasPrefix(a, "--test-mode"), "unset flag must not be forwarded: %s", a)
		for _, name := range removedTestFlags {
			assert.False(t, strings.HasPrefix(a, "--"+name), "removed flag must not be forwarded: %s", a)
		}
	}
}

func TestTestInfraMark_HumanCLIOnly(t *testing.T) {
	assert.False(t, agentAllowed["server.test-infra"], "server test-infra must not be allowed in agent mode")
	assert.False(t, agentAllowed["server.test-infra.mark"], "server test-infra mark must not be allowed in agent mode")

	build := func() *cobra.Command {
		root := buildTestTree()
		for _, child := range root.Commands() {
			if child.Name() == "server" {
				ti := &cobra.Command{Use: "test-infra"}
				ti.AddCommand(&cobra.Command{Use: "mark"})
				child.AddCommand(ti)
			}
		}
		return root
	}
	t.Setenv("SCION_CLI_MODE", "agent")
	root := build()
	applyModeRestrictions(root, resolveMode())
	assert.NotContains(t, collectCommandNames(root), "server.test-infra.mark")

	t.Setenv("SCION_CLI_MODE", "human")
	root = build()
	applyModeRestrictions(root, resolveMode())
	assert.Contains(t, collectCommandNames(root), "server.test-infra.mark")
}

func TestTestInfraMarkArgs(t *testing.T) {
	reset := func() {
		testInfraMarkHubID, testInfraMarkAcknowledge, testInfraMarkAttested = "", false, ""
	}
	t.Cleanup(reset)

	reset()
	assert.ErrorContains(t, testInfraMarkArgs(nil, nil), "--hub-id is required")

	reset()
	testInfraMarkHubID = tiaCmdHubID
	assert.ErrorContains(t, testInfraMarkArgs(nil, nil), testInfraMarkAcknowledgeFlag)

	reset()
	testInfraMarkHubID, testInfraMarkAcknowledge = " "+tiaCmdHubID, true
	assert.ErrorContains(t, testInfraMarkArgs(nil, nil), "whitespace")

	reset()
	testInfraMarkHubID, testInfraMarkAcknowledge = tiaCmdHubID, true
	for _, bad := range []string{`[]`, `"x"`, `null`, `{`} {
		testInfraMarkAttested = bad
		assert.ErrorContains(t, testInfraMarkArgs(nil, nil), "JSON object", bad)
	}
	testInfraMarkAttested = `{"bucket":"b","broker_ids":["x"]}`
	assert.NoError(t, testInfraMarkArgs(nil, nil))
	assert.ErrorContains(t, testInfraMarkArgs(nil, []string{"extra"}), "unexpected arguments")
}

// The web session cookie key class (G7) is fed from the same secret the web
// server gets. If a separate cookie-key input is ever added, this test must
// be revisited.
func TestWebSessionKeySource(t *testing.T) {
	assert.Equal(t, "generated", webSessionKeySource(""))
	assert.Equal(t, "shared_secret", webSessionKeySource("synthetic-test-secret"))

	src, err := os.ReadFile("server_foreground.go")
	require.NoError(t, err)
	s := string(src)
	assert.Equal(t, 1, strings.Count(s, "SessionSecret:        sessionSecret,"), "NewWebServer gets sessionSecret")
	assert.Equal(t, 1, strings.Count(s, "SessionSecret:"), "exactly one web session secret input")
	assert.Contains(t, s, "sessionSecret := resolveSessionSecret()")
	assert.Contains(t, s, "WebSessionKeySource: webSessionKeySource(resolveSessionSecret()),")
}
