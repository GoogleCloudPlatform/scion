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

package hub

// Test mode (ptone/scion#4240, phase TM): one startup setting,
// --test-mode=<level>:<hub id>, selects which hub-issued test identities a
// test-designated hub may issue.
//
//	off          no test identities (the default)
//	member       member and viewer test identities (POST /api/v1/test-identities)
//	admin        member, plus the test hub-admin kind
//	super-admin  admin, plus the test super-admin kind (refused until phase A2)
//
// Every level other than off requires, all together: a hub with a user
// store, the hub id binding (the value after the colon equals the resolved
// hub id), server.hub.class: test, a test infrastructure marker for this
// hub id in the database, and no explicit --production. admin and
// super-admin add the rest of the test admin gate (test_admin_gate.go). A
// level whose preconditions are not all met refuses to start; it is never
// lowered to a smaller level.

import (
	"fmt"
	"strings"
)

// TestMode is a resolved test mode level.
type TestMode string

// The test mode levels. These four strings are the complete allowlist.
const (
	TestModeOff        TestMode = "off"
	TestModeMember     TestMode = "member"
	TestModeAdmin      TestMode = "admin"
	TestModeSuperAdmin TestMode = "super-admin"
)

// TestModeLevels returns the levels in order, off first.
func TestModeLevels() []TestMode {
	return []TestMode{TestModeOff, TestModeMember, TestModeAdmin, TestModeSuperAdmin}
}

// ParseTestMode resolves a --test-mode value into its level and hub id
// binding. The value is "<level>" or "<level>:<hub id>"; the split is at the
// first colon. The level must be exactly one of the four allowlisted
// strings: anything else (empty, unknown, a different case, surrounding
// whitespace) resolves to TestModeOff with recognized false, so the parser
// never grants a level by mistake. cmd refuses to start when a non-empty
// value is not recognised (ErrTestModeUnrecognized). "off" (with or
// without a binding) resolves to TestModeOff with recognized true. The hub
// id is returned as given and is not trimmed; the startup gate compares it
// with the resolved hub id.
func ParseTestMode(value string) (mode TestMode, hubID string, recognized bool) {
	level, id, _ := strings.Cut(value, ":")
	switch TestMode(level) {
	case TestModeOff:
		return TestModeOff, "", true
	case TestModeMember, TestModeAdmin, TestModeSuperAdmin:
		return TestMode(level), id, true
	}
	return TestModeOff, "", false
}

// TestModeCapabilities is what a level grants, expressed as the three
// switches the hub runs with.
type TestModeCapabilities struct {
	// TestIdentities is ServerConfig.EnableTestIdentities: member and
	// viewer test identities.
	TestIdentities bool
	// HubAdmin is TestAdminGateConfig.HubAdmin: the test hub-admin kind.
	HubAdmin bool
	// SuperAdmin is TestAdminGateConfig.SuperAdmin: the test super-admin
	// kind.
	SuperAdmin bool
}

// Capabilities returns what level m grants. An unknown level grants
// nothing.
func (m TestMode) Capabilities() TestModeCapabilities {
	switch m {
	case TestModeMember:
		return TestModeCapabilities{TestIdentities: true}
	case TestModeAdmin:
		return TestModeCapabilities{TestIdentities: true, HubAdmin: true}
	case TestModeSuperAdmin:
		return TestModeCapabilities{TestIdentities: true, HubAdmin: true, SuperAdmin: true}
	}
	return TestModeCapabilities{}
}

// IsAdminLevel reports whether m is admin or super-admin.
func (m TestMode) IsAdminLevel() bool {
	return m == TestModeAdmin || m == TestModeSuperAdmin
}

// Names of the test mode preconditions, as reported in the startup line and
// the health summary.
const (
	TestModePreconditionHubEnabled          = "hub_enabled"
	TestModePreconditionUserStore           = "user_store"
	TestModePreconditionNotProduction       = "not_production"
	TestModePreconditionHubIDBinding        = "hub_id_binding"
	TestModePreconditionHubClassTest        = "hub_class_test"
	TestModePreconditionMarker              = "test_infra_marker"
	TestModePreconditionNotWorkstation      = "not_workstation"
	TestModePreconditionFederationOff       = "federation_off"
	TestModePreconditionNoSharedSecret      = "no_shared_signing_secret"
	TestModePreconditionNoPreconfiguredKey  = "no_preconfigured_signing_key"
	TestModePreconditionSigningKeysHubLocal = "signing_keys_hub_local"
)

// TestModeMemberPreconditions returns the preconditions every level other
// than off requires.
func TestModeMemberPreconditions() []string {
	return []string{
		TestModePreconditionHubEnabled,
		TestModePreconditionUserStore,
		TestModePreconditionNotProduction,
		TestModePreconditionHubIDBinding,
		TestModePreconditionHubClassTest,
		TestModePreconditionMarker,
	}
}

// TestModeAdminPreconditions returns the preconditions admin and
// super-admin require in addition to TestModeMemberPreconditions, checked
// from the configuration before the hub is built. New adds
// TestModePreconditionSigningKeysHubLocal once it has verified the actual
// key sources, and the co-located broker set is verified once brokers are
// known (VerifyTestAdminBrokerSet).
func TestModeAdminPreconditions() []string {
	return []string{
		TestModePreconditionNotWorkstation,
		TestModePreconditionFederationOff,
		TestModePreconditionNoSharedSecret,
		TestModePreconditionNoPreconfiguredKey,
	}
}

// TestModeConfig is the resolved test mode (ServerConfig.TestMode). cmd
// builds it after every precondition of Level passed. The zero value is
// off.
type TestModeConfig struct {
	// Level is the resolved level; empty means off.
	Level TestMode
	// Preconditions names the preconditions that admitted Level.
	Preconditions []string
}

// level returns c.Level, with empty read as off.
func (c TestModeConfig) level() TestMode {
	if c.Level == "" {
		return TestModeOff
	}
	return c.Level
}

// ErrTestModeInconsistent is returned by New when the configured switches
// (EnableTestIdentities, TestAdminGate) do not equal what TestMode.Level
// grants, or the level is not one of the four allowlisted strings.
var ErrTestModeInconsistent = testAdminGateErr("test_mode", "inconsistent",
	"the test identity switches do not match the resolved --test-mode level")

// ErrTestModeUnrecognized refuses startup when --test-mode is given a
// non-empty value whose level is not one of the four allowlisted strings
// (for example a typo or a different case). The value is not echoed.
var ErrTestModeUnrecognized = testAdminGateErr("test_mode", "unrecognized_value",
	"the --test-mode value is not recognised; use off, member, admin or super-admin (exact, lower case), written <level>:<hub id> for every level but off")

// ErrTestModeRemovedFlag refuses startup when one of the removed test
// identity flags (--enable-test-identities, --enable-test-admin-identities,
// --enable-test-super-admin-identities) is given. --test-mode replaces all
// three.
var ErrTestModeRemovedFlag = testAdminGateErr("test_mode", "removed_flag",
	"this flag was replaced by --test-mode=off|member|admin|super-admin (with :<hub id> for every level but off)")

// validateTestModeConfig re-checks, at construction, that the switches the
// hub runs with are exactly what the resolved level grants.
func validateTestModeConfig(cfg ServerConfig) error {
	level := cfg.TestMode.level()
	known := false
	for _, l := range TestModeLevels() {
		if level == l {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("%w: unknown level %q", ErrTestModeInconsistent, level)
	}
	want := level.Capabilities()
	got := TestModeCapabilities{
		TestIdentities: cfg.EnableTestIdentities,
		HubAdmin:       cfg.TestAdminGate.HubAdmin,
		SuperAdmin:     cfg.TestAdminGate.SuperAdmin,
	}
	if got != want {
		return fmt.Errorf("%w: level %q grants %+v, configured %+v", ErrTestModeInconsistent, level, want, got)
	}
	return nil
}

// testModeState is the immutable resolved test mode captured by New.
type testModeState struct {
	level         TestMode
	preconditions []string
}

func newTestModeState(c TestModeConfig) testModeState {
	level := c.level()
	if level == TestModeOff {
		return testModeState{level: TestModeOff}
	}
	return testModeState{level: level, preconditions: append([]string(nil), c.Preconditions...)}
}

// TestModeLevel returns the resolved --test-mode level this hub runs with.
func (s *Server) TestModeLevel() TestMode {
	if s.testMode.level == "" {
		return TestModeOff
	}
	return s.testMode.level
}

// testModePreconditions returns a copy of the preconditions that admitted
// the level (empty when off).
func (s *Server) testModePreconditions() []string {
	return append([]string{}, s.testMode.preconditions...)
}

// testAdminAuditEventTestModeStart is the startup line of the member level.
// The admin levels write the test_admin_gate_start line instead, which
// carries the same test_mode and preconditions fields.
const testAdminAuditEventTestModeStart = "test_mode_start"

// emitTestModeStart writes the resolved level and the preconditions that
// admitted it through the test admin audit sink. A write failure refuses
// startup.
func (s *Server) emitTestModeStart() error {
	if err := s.testAdminAudit.emit(testAdminAuditEventTestModeStart, map[string]any{
		"level":         "WARN",
		"msg":           "TEST MODE ENABLED: this hub can issue test identities. Test hubs only.",
		"test_mode":     string(s.TestModeLevel()),
		"preconditions": s.testModePreconditions(),
		"hub_id":        s.config.HubID,
	}); err != nil {
		return fmt.Errorf("test mode: startup line could not be written: %w", err)
	}
	return nil
}
