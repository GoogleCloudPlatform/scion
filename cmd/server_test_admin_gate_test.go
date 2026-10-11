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

// tiaValidInputs passes every gate condition (a --hosted hub: not a
// workstation, no explicit --production).
func tiaValidInputs() testAdminGateInputs {
	return testAdminGateInputs{
		AdminFlagSet:          true,
		AdminHubID:            tiaCmdHubID,
		HubEnabled:            true,
		TestIdentitiesEnabled: true,
		HasUserStore:          true,
		ResolvedHubID:         tiaCmdHubID,
		Workstation:           false,
		HubClass:              config.HubClassTest,
		ReadMarker: func() (*testinfra.Marker, error) {
			return &testinfra.Marker{HubID: tiaCmdHubID, Class: config.HubClassTest, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}, nil
		},
	}
}

func TestValidateTestAdminIdentities_HostedWithEverythingValidStarts(t *testing.T) {
	gate, err := validateTestAdminIdentities(tiaValidInputs())
	require.NoError(t, err)
	assert.True(t, gate.HubAdmin)
	assert.False(t, gate.SuperAdmin)
	assert.Equal(t, tiaCmdHubID, gate.HubID)
	assert.Equal(t, tiaCmdHubID, gate.MarkerHubID)
	assert.Equal(t, config.HubClassTest, gate.MarkerClass)
	assert.Equal(t, config.HubClassTest, gate.HubClass)
	assert.False(t, gate.MarkerCreatedAt.IsZero())
}

func TestValidateTestAdminIdentities_OffWithoutFlags(t *testing.T) {
	in := tiaValidInputs()
	in.AdminFlagSet = false
	in.HubClass = ""
	in.ReadMarker = nil
	gate, err := validateTestAdminIdentities(in)
	require.NoError(t, err)
	assert.Equal(t, hub.TestAdminGateConfig{}, gate)
}

// Each condition missing, one at a time, refuses startup with its named
// error.
func TestValidateTestAdminIdentities_EachConditionRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testAdminGateInputs)
		want   error
	}{
		{"G1 test identities off", func(in *testAdminGateInputs) { in.TestIdentitiesEnabled = false }, hub.ErrTestAdminRequiresTestIdentities},
		{"G1 no user store", func(in *testAdminGateInputs) { in.HasUserStore = false }, hub.ErrTestAdminRequiresUserStore},
		{"G1 no hub", func(in *testAdminGateInputs) { in.HubEnabled = false }, hub.ErrTestAdminRequiresHub},
		{"G2 value differs from PersistentHubID", func(in *testAdminGateInputs) { in.AdminHubID = "another-hub" }, hub.ErrTestAdminHubIDMismatch},
		{"G2 empty value", func(in *testAdminGateInputs) { in.AdminHubID = "" }, hub.ErrTestAdminHubIDMismatch},
		{"G2 whitespace value", func(in *testAdminGateInputs) { in.AdminHubID = " " + tiaCmdHubID }, hub.ErrTestAdminHubIDMismatch},
		{"G3 super-admin flag", func(in *testAdminGateInputs) { in.SuperAdminFlagSet = true }, hub.ErrTestSuperAdminRequiresPolicyRegistry},
		{"G3 super-admin flag alone", func(in *testAdminGateInputs) { in.AdminFlagSet = false; in.SuperAdminFlagSet = true }, hub.ErrTestSuperAdminRequiresPolicyRegistry},
		{"G4 class absent", func(in *testAdminGateInputs) { in.HubClass = "" }, hub.ErrTestAdminHubClassNotTest},
		{"G4 class other", func(in *testAdminGateInputs) { in.HubClass = "production" }, hub.ErrTestAdminHubClassNotTest},
		{"G4 class case", func(in *testAdminGateInputs) { in.HubClass = "TEST" }, hub.ErrTestAdminHubClassNotTest},
		{"G5 explicit --production", func(in *testAdminGateInputs) { in.ProductionFlagExplicit = true }, hub.ErrTestAdminProductionFlag},
		{"G6 marker absent", func(in *testAdminGateInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, testinfra.ErrMarkerAbsent }
		}, hub.ErrTestAdminMarkerAbsent},
		{"G6 marker unreadable", func(in *testAdminGateInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) { return nil, errors.New("synthetic read failure") }
		}, hub.ErrTestAdminMarkerUnreadable},
		{"G6 marker hub id differs", func(in *testAdminGateInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: "another-hub", Class: config.HubClassTest}, nil
			}
		}, hub.ErrTestAdminMarkerHubMismatch},
		{"G6 marker class", func(in *testAdminGateInputs) {
			in.ReadMarker = func() (*testinfra.Marker, error) {
				return &testinfra.Marker{HubID: tiaCmdHubID, Class: "prod"}, nil
			}
		}, hub.ErrTestAdminMarkerClassNotTest},
		{"G7 shared signing secret", func(in *testAdminGateInputs) { in.SharedSigningSecret = true }, hub.ErrTestAdminSharedSigningSecret},
		{"G7 pre-configured signing key", func(in *testAdminGateInputs) { in.PreconfiguredSigningKey = true }, hub.ErrTestAdminPreconfiguredSigningKey},
		{"G8 federation enabled", func(in *testAdminGateInputs) { in.Federation.Enabled = true }, hub.ErrTestAdminFederationConfigured},
		{"G8 federation issuers", func(in *testAdminGateInputs) {
			in.Federation.TrustedIssuers = []config.TrustedIssuerConfig{{IssuerURL: "https://issuer.example.invalid"}}
		}, hub.ErrTestAdminFederationConfigured},
		{"G10 workstation hub", func(in *testAdminGateInputs) { in.Workstation = true }, hub.ErrTestAdminWorkstationHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tiaValidInputs()
			tc.mutate(&in)
			gate, err := validateTestAdminIdentities(in)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.want), "got %v, want %v", err, tc.want)
			assert.Equal(t, hub.TestAdminGateConfig{}, gate, "a refused gate is off")
			var named *hub.TestAdminGateError
			require.True(t, errors.As(err, &named), "the error is named")
			assert.Contains(t, err.Error(), named.Name)
		})
	}
}

// tiaStartCmd parses args with the server-start flags the gate reads.
func tiaStartCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	enableTestAdminIdentities, enableTestSuperAdminIdentities = "", ""
	var hosted bool
	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().BoolVar(&hosted, "hosted", false, "")
	c.Flags().BoolVar(&hosted, "production", false, "")
	registerTestAdminFlags(c)
	c.SetArgs(args)
	require.NoError(t, c.Execute())
	return c
}

func TestValidateTestAdminFlagsEarly(t *testing.T) {
	t.Cleanup(func() {
		enableTestAdminIdentities, enableTestSuperAdminIdentities = "", ""
		testAdminFlagSet, testSuperAdminFlagSet, productionFlagIsExplicit = false, false, false
	})
	err := validateTestAdminFlagsEarly(tiaStartCmd(t, "--production", "--enable-test-admin-identities="+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestAdminProductionFlag, "explicit --production is refused")
	assert.True(t, productionFlagIsExplicit)

	err = validateTestAdminFlagsEarly(tiaStartCmd(t, "--hosted", "--enable-test-admin-identities="+tiaCmdHubID))
	assert.NoError(t, err, "--hosted is allowed")
	assert.True(t, testAdminFlagSet)
	assert.False(t, productionFlagIsExplicit)
	assert.Equal(t, tiaCmdHubID, enableTestAdminIdentities)

	err = validateTestAdminFlagsEarly(tiaStartCmd(t, "--hosted", "--enable-test-admin-identities="+tiaCmdHubID, "--enable-test-super-admin-identities="+tiaCmdHubID))
	assert.ErrorIs(t, err, hub.ErrTestSuperAdminRequiresPolicyRegistry)
	assert.Contains(t, err.Error(), "policy registry")

	err = validateTestAdminFlagsEarly(tiaStartCmd(t, "--production"))
	assert.NoError(t, err, "--production alone is not this gate's concern")

	err = validateTestAdminFlagsEarly(tiaStartCmd(t, "--enable-test-admin-identities="))
	assert.NoError(t, err, "an empty value is caught by G2 at hub construction")
	assert.True(t, testAdminFlagSet)

	assert.ErrorIs(t, validateTestAdminRequiresHub(false), hub.ErrTestAdminRequiresHub)
	assert.NoError(t, validateTestAdminRequiresHub(true))
}

func TestTestAdminFlags_RegisteredOnServerStart(t *testing.T) {
	for _, name := range []string{testAdminIdentitiesFlag, testSuperAdminIdentitiesFlag} {
		f := serverStartCmd.Flags().Lookup(name)
		require.NotNil(t, f, name)
		assert.Equal(t, "string", f.Value.Type(), name)
		assert.Equal(t, "", f.DefValue, name)
	}
}

func TestBuildDaemonStartArgsForwardsTestAdminFlags(t *testing.T) {
	t.Cleanup(func() { enableTestAdminIdentities, enableTestSuperAdminIdentities = "", "" })
	args := buildDaemonStartArgs(tiaStartCmd(t, "--enable-test-admin-identities="+tiaCmdHubID))
	assert.Contains(t, args, "--enable-test-admin-identities="+tiaCmdHubID)
	for _, a := range buildDaemonStartArgs(tiaStartCmd(t)) {
		assert.False(t, strings.HasPrefix(a, "--enable-test-admin-identities"), "unset flag must not be forwarded: %s", a)
		assert.False(t, strings.HasPrefix(a, "--enable-test-super-admin-identities"), "unset flag must not be forwarded: %s", a)
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
