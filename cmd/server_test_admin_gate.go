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
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
)

// Test mode startup gate (ptone/scion#4240, phase TM; the admin levels are
// the test admin tier of phase A0). See pkg/hub/test_mode.go for the levels
// and pkg/hub/test_admin_gate.go for the gate conditions (G1-G10).

const (
	// testModeFlag is --test-mode=<level>:<hub id>.
	testModeFlag = "test-mode"

	// The flags --test-mode replaced. They stay registered, hidden, only
	// so that using one refuses startup with hub.ErrTestModeRemovedFlag
	// naming --test-mode instead of a bare "unknown flag".
	removedTestIdentitiesFlag           = "enable-test-identities"
	removedTestAdminIdentitiesFlag      = "enable-test-admin-identities"
	removedTestSuperAdminIdentitiesFlag = "enable-test-super-admin-identities"
)

// removedTestFlags lists the replaced flags in a fixed order.
var removedTestFlags = []string{removedTestIdentitiesFlag, removedTestAdminIdentitiesFlag, removedTestSuperAdminIdentitiesFlag}

var (
	// testModeValue is the raw --test-mode value.
	testModeValue string
	// removedTestFlagValue receives the value of any removed flag; it is
	// never read.
	removedTestFlagValue string

	// Recorded from the parsed flags by recordTestModeFlags, before any
	// config reconciliation, so the gate sees what the operator typed.
	testModeLevel            hub.TestMode
	testModeHubID            string
	productionFlagIsExplicit bool
)

func registerTestModeFlags(c *cobra.Command) {
	c.Flags().StringVar(&testModeValue, testModeFlag, "",
		"Test identity level of a designated test hub: off, member, admin or super-admin, written <level>:<hub id> (the hub id must be this hub's resolved hub id). "+
			"Every level but off requires server.hub.class: test, a test infrastructure marker for this hub id and no --production; admin also requires a non-workstation hub, no federation and hub-local signing keys. "+
			"A level whose requirements are not all met, or an unrecognised value, refuses to start; empty means off (do not use in production)")
	for _, name := range removedTestFlags {
		c.Flags().StringVar(&removedTestFlagValue, name, "", "Replaced by --"+testModeFlag)
		// A bare --enable-test-identities still parses, so it reaches the
		// named refusal.
		c.Flags().Lookup(name).NoOptDefVal = "true"
		_ = c.Flags().MarkHidden(name)
	}
}

// recordTestModeFlags captures the parsed --test-mode level and binding and
// whether --production was given explicitly. --production is a deprecated
// alias that shares --hosted's variable, so only Changed can tell the two
// spellings apart (G5).
func recordTestModeFlags(cmd *cobra.Command) (recognized bool) {
	testModeLevel, testModeHubID, recognized = hub.ParseTestMode(testModeValue)
	if !cmd.Flags().Changed(testModeFlag) {
		testModeLevel, testModeHubID, recognized = hub.TestModeOff, "", true
	}
	productionFlagIsExplicit = cmd.Flags().Changed("production")
	return recognized
}

// testModeRequested reports whether a level other than off was requested.
func testModeRequested() bool {
	return testModeLevel != "" && testModeLevel != hub.TestModeOff
}

// validateTestModeFlagsEarly runs the checks that need only the parsed
// flags: a removed flag is refused (naming --test-mode), super-admin is
// refused (G3) and an explicit --production with any level but off is
// refused (G5). It runs before config is loaded, in the daemon parent as
// well as the foreground process, so a refused flag never reaches a
// re-exec. A non-empty --test-mode value that is not recognised is refused
// too (the parser resolves it to off, so nothing would be granted, but the
// operator asked for something); an empty value is off.
func validateTestModeFlagsEarly(cmd *cobra.Command) error {
	for _, name := range removedTestFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return fmt.Errorf("--%s: %w", name, hub.ErrTestModeRemovedFlag)
		}
	}
	if !recordTestModeFlags(cmd) && testModeValue != "" {
		return hub.ErrTestModeUnrecognized
	}
	if !testModeRequested() {
		return nil
	}
	if testModeLevel == hub.TestModeSuperAdmin {
		// G3: the super-admin kind needs a complete test-admin policy.
		if err := testAdminPolicyComplete(); err != nil {
			return fmt.Errorf("%w: %v", hub.ErrTestSuperAdminRequiresPolicyRegistry, err)
		}
	}
	if productionFlagIsExplicit {
		return fmt.Errorf("--test-mode=%s: %w", testModeLevel, hub.ErrTestAdminProductionFlag)
	}
	return nil
}

// validateTestModeRequiresHub refuses a test mode level on a process that
// does not run the hub.
func validateTestModeRequiresHub(hubEnabled bool) error {
	if testModeRequested() && !hubEnabled {
		return fmt.Errorf("--test-mode=%s: %w", testModeLevel, hub.ErrTestAdminRequiresHub)
	}
	return nil
}

// testModeInputs are the inputs of resolveTestMode.
type testModeInputs struct {
	// Level and BindingHubID are the parsed --test-mode value.
	Level                  hub.TestMode
	BindingHubID           string
	ProductionFlagExplicit bool

	HubEnabled    bool
	HasUserStore  bool
	ResolvedHubID string
	Workstation   bool
	HubClass      string
	Federation    config.FederationConfig

	SharedSigningSecret     bool
	PreconfiguredSigningKey bool

	// ReadMarker reads the test_infra_marker row (G6).
	ReadMarker func() (*testinfra.Marker, error)

	// PolicyComplete reports whether the test-admin policy classification
	// is complete (G3, super-admin only). Nil means
	// hub.TestAdminPolicyComplete.
	PolicyComplete func() error
}

// testAdminPolicyComplete is the G3 check. A variable only so tests can
// substitute an incomplete classification.
var testAdminPolicyComplete = hub.TestAdminPolicyComplete

// testModeResolution is what a resolved level sets on hub.ServerConfig.
type testModeResolution struct {
	EnableTestIdentities bool
	TestAdminGate        hub.TestAdminGateConfig
	TestMode             hub.TestModeConfig
}

// resolveTestMode checks every precondition of the requested level and
// returns the hub configuration it grants. Every level but off ANDs the
// member preconditions (G1 hub and user store, G5 no explicit
// --production, G2 hub id binding, G4 hub class, G6 marker); admin adds G10
// (not a workstation hub), G8 (no federation) and G7 (no shared or
// pre-configured signing key); super-admin is refused (G3). Any unmet
// precondition refuses startup with a named error (hub.Err*): a level is
// never lowered. Off returns the zero resolution. hub.New re-checks the
// result and verifies the actual signing-key sources (G7) before it
// captures the gate.
func resolveTestMode(in testModeInputs) (testModeResolution, error) {
	var off testModeResolution
	level := in.Level
	if level == "" || level == hub.TestModeOff {
		return off, nil
	}
	refuse := func(err error) (testModeResolution, error) {
		return off, fmt.Errorf("--test-mode=%s: %w", level, err)
	}
	switch level {
	case hub.TestModeMember, hub.TestModeAdmin:
	case hub.TestModeSuperAdmin:
		// G3: the super-admin kind is admitted only by a binary whose
		// test-admin policy classification is complete.
		complete := in.PolicyComplete
		if complete == nil {
			complete = testAdminPolicyComplete
		}
		if err := complete(); err != nil {
			return refuse(fmt.Errorf("%w: %v", hub.ErrTestSuperAdminRequiresPolicyRegistry, err))
		}
	default:
		return refuse(hub.ErrTestModeInconsistent)
	}
	// G1: a hub with a user store.
	if !in.HubEnabled {
		return refuse(hub.ErrTestAdminRequiresHub)
	}
	if !in.HasUserStore {
		return refuse(hub.ErrTestAdminRequiresUserStore)
	}
	// G5: explicit --production (not --hosted).
	if in.ProductionFlagExplicit {
		return refuse(hub.ErrTestAdminProductionFlag)
	}
	// G2: the binding is this hub's resolved id.
	if in.BindingHubID == "" || in.ResolvedHubID == "" || in.BindingHubID != in.ResolvedHubID {
		return refuse(fmt.Errorf("%w: --test-mode hub id %q, resolved hub id %q", hub.ErrTestAdminHubIDMismatch, in.BindingHubID, in.ResolvedHubID))
	}
	// G4: a positive, exact hub class.
	if !config.IsTestHubClass(in.HubClass) {
		return refuse(fmt.Errorf("%w: %s is %q", hub.ErrTestAdminHubClassNotTest, config.HubClassKey, in.HubClass))
	}
	preconditions := append([]string(nil), hub.TestModeMemberPreconditions()...)
	if level.IsAdminLevel() {
		// G10: never on a workstation hub.
		if in.Workstation {
			return refuse(hub.ErrTestAdminWorkstationHub)
		}
		// G8: no federation trust at startup.
		if in.Federation.Enabled || len(in.Federation.TrustedIssuers) > 0 {
			return refuse(hub.ErrTestAdminFederationConfigured)
		}
		// G7: no shared or pre-configured signing key.
		if in.SharedSigningSecret {
			return refuse(hub.ErrTestAdminSharedSigningSecret)
		}
		if in.PreconfiguredSigningKey {
			return refuse(hub.ErrTestAdminPreconfiguredSigningKey)
		}
		preconditions = append(preconditions, hub.TestModeAdminPreconditions()...)
		if level == hub.TestModeSuperAdmin {
			preconditions = append(preconditions, hub.TestModePreconditionPolicyComplete)
		}
	}
	// G6: the datastore's own test marker for this hub. Read last: it is
	// the only check that touches the database.
	if in.ReadMarker == nil {
		return refuse(hub.ErrTestAdminMarkerAbsent)
	}
	marker, err := in.ReadMarker()
	if err != nil {
		if errors.Is(err, testinfra.ErrMarkerAbsent) {
			return refuse(hub.ErrTestAdminMarkerAbsent)
		}
		return refuse(fmt.Errorf("%w: %v", hub.ErrTestAdminMarkerUnreadable, err))
	}
	if !config.IsTestHubClass(marker.Class) {
		return refuse(fmt.Errorf("%w: marker class %q", hub.ErrTestAdminMarkerClassNotTest, marker.Class))
	}
	if marker.HubID != in.ResolvedHubID {
		return refuse(fmt.Errorf("%w: marker hub id %q, resolved hub id %q", hub.ErrTestAdminMarkerHubMismatch, marker.HubID, in.ResolvedHubID))
	}
	res := testModeResolution{
		EnableTestIdentities: true,
		TestMode: hub.TestModeConfig{
			Level:          level,
			Preconditions:  preconditions,
			HubID:          in.BindingHubID,
			HubClass:       in.HubClass,
			MarkerHubID:    marker.HubID,
			MarkerClass:    marker.Class,
			ProductionFlag: in.ProductionFlagExplicit,
		},
	}
	if level.IsAdminLevel() {
		res.TestAdminGate = hub.TestAdminGateConfig{
			HubAdmin:        true,
			SuperAdmin:      level == hub.TestModeSuperAdmin,
			HubID:           in.ResolvedHubID,
			HubClass:        in.HubClass,
			MarkerHubID:     marker.HubID,
			MarkerClass:     marker.Class,
			MarkerCreatedAt: marker.CreatedAt,
		}
	}
	return res, nil
}

// testModeInputsFor assembles the gate inputs at hub construction.
func testModeInputsFor(ctx context.Context, cfg *config.GlobalConfig, hubCfg hub.ServerConfig, hasStore bool, entClient *ent.Client) testModeInputs {
	return testModeInputs{
		Level:                   testModeLevel,
		BindingHubID:            testModeHubID,
		ProductionFlagExplicit:  productionFlagIsExplicit,
		HubEnabled:              enableHub,
		HasUserStore:            hasStore,
		ResolvedHubID:           hubCfg.HubID,
		Workstation:             hubCfg.Workstation,
		HubClass:                cfg.Hub.HubClass,
		Federation:              hubCfg.Federation,
		SharedSigningSecret:     hubCfg.SharedSigningSecret != "",
		PreconfiguredSigningKey: len(hubCfg.UserTokenConfig.SigningKey) > 0 || len(hubCfg.AgentTokenConfig.SigningKey) > 0,
		ReadMarker: func() (*testinfra.Marker, error) {
			if entClient == nil {
				return nil, testinfra.ErrMarkerAbsent
			}
			return testinfra.Read(ctx, entClient)
		},
	}
}

// webSessionKeySource is the signingKeySource label of the web session
// cookie key: initWebServer passes resolveSessionSecret() to NewWebServer,
// which derives the cookie keys from it, or generates a random per-process
// key when it is empty.
func webSessionKeySource(sessionSecret string) string {
	if sessionSecret == "" {
		return "generated"
	}
	return "shared_secret"
}
