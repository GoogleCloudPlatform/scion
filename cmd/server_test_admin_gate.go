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

// Test admin tier startup gate (ptone/scion#4240, admin tier phase A0).
// See pkg/hub/test_admin_gate.go for the condition list (G1-G10).

const (
	testAdminIdentitiesFlag      = "enable-test-admin-identities"
	testSuperAdminIdentitiesFlag = "enable-test-super-admin-identities"
)

var (
	// enableTestAdminIdentities is --enable-test-admin-identities=<hub id>.
	enableTestAdminIdentities string
	// enableTestSuperAdminIdentities is
	// --enable-test-super-admin-identities=<hub id>. This binary always
	// refuses it (G3).
	enableTestSuperAdminIdentities string

	// Recorded from the parsed flags by recordTestAdminFlags, before any
	// config reconciliation, so the gate sees what the operator typed.
	testAdminFlagSet         bool
	testSuperAdminFlagSet    bool
	productionFlagIsExplicit bool
)

func registerTestAdminFlags(c *cobra.Command) {
	c.Flags().StringVar(&enableTestAdminIdentities, testAdminIdentitiesFlag, "",
		"Enable test identities with hub-admin authority on a designated test hub; the value must be this hub's hub id. Requires --enable-test-identities, server.hub.class: test, a test infrastructure marker and hub-local signing keys (do not use in production)")
	c.Flags().StringVar(&enableTestSuperAdminIdentities, testSuperAdminIdentitiesFlag, "",
		"Enable the test super-admin identity kind; the value must be this hub's hub id. Refused by this version (needs the test-admin policy registry)")
}

// recordTestAdminFlags captures which gate-relevant flags were given
// explicitly. --production is a deprecated alias that shares --hosted's
// variable, so only Changed can tell the two spellings apart (G5).
func recordTestAdminFlags(cmd *cobra.Command) {
	testAdminFlagSet = cmd.Flags().Changed(testAdminIdentitiesFlag)
	testSuperAdminFlagSet = cmd.Flags().Changed(testSuperAdminIdentitiesFlag)
	productionFlagIsExplicit = cmd.Flags().Changed("production")
}

// testAdminFlagsRequested reports whether either test admin flag was given.
func testAdminFlagsRequested() bool {
	return testAdminFlagSet || testSuperAdminFlagSet
}

// validateTestAdminFlagsEarly runs the checks that need only the parsed
// flags: G3 (super-admin refused) and G5 (explicit --production refused).
// It runs before config is loaded, in the daemon parent as well as the
// foreground process, so a refused flag never reaches a re-exec.
func validateTestAdminFlagsEarly(cmd *cobra.Command) error {
	recordTestAdminFlags(cmd)
	if !testAdminFlagsRequested() {
		return nil
	}
	if testSuperAdminFlagSet {
		return hub.ErrTestSuperAdminRequiresPolicyRegistry
	}
	if productionFlagIsExplicit {
		return hub.ErrTestAdminProductionFlag
	}
	return nil
}

// validateTestAdminRequiresHub refuses the test admin flags on a process
// that does not run the hub.
func validateTestAdminRequiresHub(hubEnabled bool) error {
	if testAdminFlagsRequested() && !hubEnabled {
		return hub.ErrTestAdminRequiresHub
	}
	return nil
}

// testAdminGateInputs are the inputs of validateTestAdminIdentities.
type testAdminGateInputs struct {
	AdminFlagSet           bool
	AdminHubID             string
	SuperAdminFlagSet      bool
	ProductionFlagExplicit bool

	HubEnabled            bool
	TestIdentitiesEnabled bool
	HasUserStore          bool
	ResolvedHubID         string
	Workstation           bool
	HubClass              string
	Federation            config.FederationConfig

	SharedSigningSecret     bool
	PreconfiguredSigningKey bool

	// ReadMarker reads the test_infra_marker row (G6).
	ReadMarker func() (*testinfra.Marker, error)
}

// validateTestAdminIdentities checks the test admin tier gate (G1-G8 and
// G10; G9 is the Helm render refusal) and returns the gate configuration
// for hub.ServerConfig. Any failure refuses startup with a named error
// (hub.Err*); with neither flag given it returns the zero (off) gate.
// hub.New re-checks the configuration and verifies the actual signing-key
// sources (G7) before it captures the gate.
func validateTestAdminIdentities(in testAdminGateInputs) (hub.TestAdminGateConfig, error) {
	var off hub.TestAdminGateConfig
	if !in.AdminFlagSet && !in.SuperAdminFlagSet {
		return off, nil
	}
	// G3: the super-admin kind needs the A2 policy registry.
	if in.SuperAdminFlagSet {
		return off, hub.ErrTestSuperAdminRequiresPolicyRegistry
	}
	if !in.HubEnabled {
		return off, hub.ErrTestAdminRequiresHub
	}
	// G5: explicit --production (not --hosted).
	if in.ProductionFlagExplicit {
		return off, hub.ErrTestAdminProductionFlag
	}
	// G1: the member tier and a user store.
	if !in.TestIdentitiesEnabled {
		return off, hub.ErrTestAdminRequiresTestIdentities
	}
	if !in.HasUserStore {
		return off, hub.ErrTestAdminRequiresUserStore
	}
	// G2: the flag value is this hub's resolved id.
	if in.AdminHubID == "" || in.ResolvedHubID == "" || in.AdminHubID != in.ResolvedHubID {
		return off, fmt.Errorf("%w: flag value %q, resolved hub id %q", hub.ErrTestAdminHubIDMismatch, in.AdminHubID, in.ResolvedHubID)
	}
	// G10: never on a workstation hub.
	if in.Workstation {
		return off, hub.ErrTestAdminWorkstationHub
	}
	// G4: a positive, exact hub class.
	if !config.IsTestHubClass(in.HubClass) {
		return off, fmt.Errorf("%w: %s is %q", hub.ErrTestAdminHubClassNotTest, config.HubClassKey, in.HubClass)
	}
	// G8: no federation trust at startup.
	if in.Federation.Enabled || len(in.Federation.TrustedIssuers) > 0 {
		return off, hub.ErrTestAdminFederationConfigured
	}
	// G7: no shared or pre-configured signing key.
	if in.SharedSigningSecret {
		return off, hub.ErrTestAdminSharedSigningSecret
	}
	if in.PreconfiguredSigningKey {
		return off, hub.ErrTestAdminPreconfiguredSigningKey
	}
	// G6: the datastore's own test marker for this hub.
	if in.ReadMarker == nil {
		return off, hub.ErrTestAdminMarkerAbsent
	}
	marker, err := in.ReadMarker()
	if err != nil {
		if errors.Is(err, testinfra.ErrMarkerAbsent) {
			return off, hub.ErrTestAdminMarkerAbsent
		}
		return off, fmt.Errorf("%w: %v", hub.ErrTestAdminMarkerUnreadable, err)
	}
	if !config.IsTestHubClass(marker.Class) {
		return off, fmt.Errorf("%w: marker class %q", hub.ErrTestAdminMarkerClassNotTest, marker.Class)
	}
	if marker.HubID != in.ResolvedHubID {
		return off, fmt.Errorf("%w: marker hub id %q, resolved hub id %q", hub.ErrTestAdminMarkerHubMismatch, marker.HubID, in.ResolvedHubID)
	}
	return hub.TestAdminGateConfig{
		HubAdmin:        true,
		HubID:           in.ResolvedHubID,
		HubClass:        in.HubClass,
		MarkerHubID:     marker.HubID,
		MarkerCreatedAt: marker.CreatedAt,
	}, nil
}

// testAdminGateInputsFor assembles the gate inputs at hub construction.
func testAdminGateInputsFor(ctx context.Context, cfg *config.GlobalConfig, hubCfg hub.ServerConfig, hasStore bool, entClient *ent.Client) testAdminGateInputs {
	return testAdminGateInputs{
		AdminFlagSet:            testAdminFlagSet,
		AdminHubID:              enableTestAdminIdentities,
		SuperAdminFlagSet:       testSuperAdminFlagSet,
		ProductionFlagExplicit:  productionFlagIsExplicit,
		HubEnabled:              enableHub,
		TestIdentitiesEnabled:   hubCfg.EnableTestIdentities,
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
