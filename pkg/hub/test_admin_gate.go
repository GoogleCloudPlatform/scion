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

// Test admin tier gate (ptone/scion#4240, admin tier phase A0).
//
// The gate decides, once at process start, whether this hub may later issue
// test identities that hold hub-admin (and, from phase A2, super-admin)
// authority. Its inputs are deploy-time only:
//
//	G1  --enable-test-identities is on and the hub has a user store
//	G2  --enable-test-admin-identities=<id> with id == the resolved hub id
//	G3  --enable-test-super-admin-identities=<id> (REFUSED in this binary:
//	    the super-admin kind needs the test-admin policy registry, phase A2)
//	G4  server.hub.class is exactly "test" (never writable through the API)
//	G5  explicit --production is refused (--hosted is allowed)
//	G6  the database carries a test_infra_marker row for this hub id
//	G7  every signing-key class resolves from a hub-local source
//	G8  no federation is configured
//	G9  Helm refuses both flags (deploy/helm/scion-hub $unsafeToPass)
//	G10 not a workstation hub
//
// cmd's validateTestAdminIdentities checks G1-G8 and G10 and fails startup
// with a named error; New re-checks the configuration and verifies G7
// against the actual key sources, then captures the result into the
// immutable testAdminGate value. Nothing changes the gate afterwards: it has
// no setter, no settings key, no experiment and no route.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAdminGateError is a named refusal of the test admin tier. Condition is
// the gate condition (G1..G10, or "broker" for the broker check) and Name a
// stable snake_case identifier.
type TestAdminGateError struct {
	Condition string
	Name      string
	Detail    string
}

func (e *TestAdminGateError) Error() string {
	return fmt.Sprintf("test admin tier refused (%s %s): %s", e.Condition, e.Name, e.Detail)
}

func testAdminGateErr(condition, name, detail string) *TestAdminGateError {
	return &TestAdminGateError{Condition: condition, Name: name, Detail: detail}
}

// Named test admin tier refusals. Callers wrap them with %w and add detail;
// match with errors.Is.
var (
	ErrTestAdminRequiresHub = testAdminGateErr("G1", "requires_hub",
		"--enable-test-admin-identities and --enable-test-super-admin-identities need a hub (--enable-hub)")
	ErrTestAdminRequiresTestIdentities = testAdminGateErr("G1", "requires_test_identities",
		"--enable-test-admin-identities requires --enable-test-identities")
	ErrTestAdminRequiresUserStore = testAdminGateErr("G1", "requires_user_store",
		"--enable-test-admin-identities requires a hub with a user store")
	ErrTestAdminHubIDMismatch = testAdminGateErr("G2", "hub_id_mismatch",
		"the --enable-test-admin-identities value must equal this hub's resolved hub id")
	ErrTestSuperAdminRequiresPolicyRegistry = testAdminGateErr("G3", "super_admin_requires_policy_registry",
		"--enable-test-super-admin-identities is refused: the test super-admin kind needs the test-admin policy registry, which this binary does not include (admin tier phase A2)")
	ErrTestAdminHubClassNotTest = testAdminGateErr("G4", "hub_class_not_test",
		`server.hub.class must be exactly "test"`)
	ErrTestAdminProductionFlag = testAdminGateErr("G5", "production_flag",
		"--production is refused with the test admin tier; use --hosted")
	ErrTestAdminMarkerAbsent = testAdminGateErr("G6", "marker_absent",
		"the database has no test infrastructure marker; run `scion server test-infra mark` on the hub host")
	ErrTestAdminMarkerHubMismatch = testAdminGateErr("G6", "marker_hub_mismatch",
		"the test infrastructure marker names a different hub id")
	ErrTestAdminMarkerClassNotTest = testAdminGateErr("G6", "marker_class_not_test",
		`the test infrastructure marker class is not "test"`)
	ErrTestAdminMarkerUnreadable = testAdminGateErr("G6", "marker_unreadable",
		"the test infrastructure marker could not be read")
	ErrTestAdminSharedSigningSecret = testAdminGateErr("G7", "shared_signing_secret",
		"a shared signing secret (--session-secret / SCION_SERVER_SESSION_SECRET) is configured; the test admin tier requires hub-local signing keys")
	ErrTestAdminPreconfiguredSigningKey = testAdminGateErr("G7", "preconfigured_signing_key",
		"a pre-configured signing key is set; the test admin tier requires hub-local signing keys")
	ErrTestAdminSigningKeyNotHubLocal = testAdminGateErr("G7", "signing_key_not_hub_local",
		"a signing key did not resolve from a hub-local source")
	ErrTestAdminFederationConfigured = testAdminGateErr("G8", "federation_configured",
		"federation is configured; the test admin tier requires federation to be off")
	ErrTestAdminWorkstationHub = testAdminGateErr("G10", "workstation_hub",
		"the test admin tier is not available on a workstation hub; run with --hosted")
	ErrTestAdminRemoteBrokerActive = testAdminGateErr("broker", "remote_broker_active",
		"a runtime broker other than this hub's co-located broker is registered and active")
	ErrTestAdminInconsistent = testAdminGateErr("gate", "inconsistent",
		"the test admin gate configuration is inconsistent")
)

// ErrTestAdminRemoteBrokerRefused is returned (and answered with 403) when
// a broker other than the co-located broker tries to register, join or
// authenticate while the test admin tier is on.
var ErrTestAdminRemoteBrokerRefused = testAdminGateErr("broker", "remote_broker_refused",
	"while the test admin tier is on, only this hub's co-located runtime broker may register or connect")

// ErrCodeTestAdminRemoteBrokerRefused is the API error code of
// ErrTestAdminRemoteBrokerRefused.
const ErrCodeTestAdminRemoteBrokerRefused = "test_admin_tier_remote_broker_refused"

// TestAdminGateConfig is the validated gate input (ServerConfig.TestAdminGate).
type TestAdminGateConfig struct {
	// HubAdmin is true when --enable-test-admin-identities passed validation.
	HubAdmin bool
	// SuperAdmin is true when --enable-test-super-admin-identities was
	// accepted. This binary always refuses it (G3); New refuses a config
	// that sets it.
	SuperAdmin bool
	// HubID is the validated flag value; it equals ServerConfig.HubID.
	HubID string
	// HubClass is server.hub.class as loaded at startup.
	HubClass string
	// MarkerHubID and MarkerCreatedAt come from the test_infra_marker row.
	MarkerHubID     string
	MarkerCreatedAt time.Time
}

func (c TestAdminGateConfig) requested() bool { return c.HubAdmin || c.SuperAdmin }

// federationConfigured reports whether any federation trust is configured:
// enabled, or carrying trusted issuers.
func federationConfigured(f *config.FederationConfig) bool {
	return f != nil && (f.Enabled || len(f.TrustedIssuers) > 0)
}

// validateTestAdminGateConfig re-checks, at construction, the gate inputs
// cmd validated. A Server is never built with an inconsistent gate.
func validateTestAdminGateConfig(cfg ServerConfig, hasStore bool) error {
	g := cfg.TestAdminGate
	if !g.requested() {
		return nil
	}
	switch {
	case g.SuperAdmin:
		return ErrTestSuperAdminRequiresPolicyRegistry
	case !cfg.EnableTestIdentities:
		return ErrTestAdminRequiresTestIdentities
	case !hasStore:
		return ErrTestAdminRequiresUserStore
	case g.HubID == "" || g.HubID != cfg.HubID:
		return ErrTestAdminHubIDMismatch
	case cfg.Workstation:
		return ErrTestAdminWorkstationHub
	case !config.IsTestHubClass(g.HubClass):
		return ErrTestAdminHubClassNotTest
	case g.MarkerHubID == "":
		return ErrTestAdminMarkerAbsent
	case g.MarkerHubID != g.HubID:
		return ErrTestAdminMarkerHubMismatch
	case federationConfigured(&cfg.Federation):
		return ErrTestAdminFederationConfigured
	case cfg.SharedSigningSecret != "":
		return ErrTestAdminSharedSigningSecret
	case len(cfg.UserTokenConfig.SigningKey) > 0 || len(cfg.AgentTokenConfig.SigningKey) > 0:
		return ErrTestAdminPreconfiguredSigningKey
	}
	return nil
}

// testAdminKind names an admin test-identity kind. The kinds themselves
// (user rows, issuance) arrive in phase A1; A0 only gates them.
type testAdminKind string

const (
	testAdminKindHubAdmin   testAdminKind = "test_hub_admin"
	testAdminKindSuperAdmin testAdminKind = "test_super_admin"
)

// testAdminGate is the immutable gate value captured by New. Its fields are
// unexported and it has no methods that change it; Server holds it by value
// and assigns it exactly once, in New.
type testAdminGate struct {
	hubAdmin        bool
	superAdmin      bool
	hubID           string
	hubClass        string
	markerCreatedAt time.Time
}

func newTestAdminGate(c TestAdminGateConfig) testAdminGate {
	if !c.requested() {
		return testAdminGate{}
	}
	return testAdminGate{
		hubAdmin:        c.HubAdmin,
		superAdmin:      c.SuperAdmin,
		hubID:           c.HubID,
		hubClass:        c.HubClass,
		markerCreatedAt: c.MarkerCreatedAt,
	}
}

func (g testAdminGate) on() bool { return g.hubAdmin || g.superAdmin }

func (g testAdminGate) enabled(kind testAdminKind) bool {
	switch kind {
	case testAdminKindHubAdmin:
		return g.hubAdmin
	case testAdminKindSuperAdmin:
		return g.superAdmin
	}
	return false
}

// testAdminTierEnabled reports whether the admin test-identity kind is
// enabled on this hub. It reads only the immutable gate captured by New.
func (s *Server) testAdminTierEnabled(kind testAdminKind) bool {
	return s.testAdminGate.enabled(kind)
}

// testAdminTierOn reports whether any admin test-identity kind is enabled.
func (s *Server) testAdminTierOn() bool {
	return s.testAdminGate.on()
}

// TestAdminTierOn reports whether the test admin tier is on. Read-only: it
// exists for startup checks in cmd.
func (s *Server) TestAdminTierOn() bool {
	return s.testAdminTierOn()
}

// liveTestAdminIdentityCount is the number of live admin-kind test
// identities. Phase A0 has no admin kinds, so it is always 0; phase A1
// replaces the body with a store count.
func (s *Server) liveTestAdminIdentityCount(_ context.Context) (int, error) {
	return 0, nil
}

// Signing-key source labels (signingKeySource). Labels only: no key
// material is ever recorded or returned.
const (
	signingKeySourceConfig        = "config"
	signingKeySourceSharedSecret  = "shared_secret"
	signingKeySourceSecretBackend = "secret_backend"
	signingKeySourceStore         = "store"
	signingKeySourceLegacyStore   = "legacy_store"
	signingKeySourceGenerated     = "generated"
	signingKeySourceEphemeral     = "ephemeral"
	signingKeySourceUnresolved    = "unresolved"
)

// signingKeySourceIsHubLocal reports whether a key from source is local to
// this hub: stored under this hub's id (secret backend or database),
// generated by this process, or held in memory only. A pre-configured key,
// a key derived from the deployment-wide shared secret, a key read from a
// legacy (not hub-id-scoped) record, or an unresolved key is not.
func signingKeySourceIsHubLocal(source string) bool {
	switch source {
	case signingKeySourceSecretBackend, signingKeySourceStore, signingKeySourceGenerated, signingKeySourceEphemeral:
		return true
	}
	return false
}

// recordSigningKeySource records where a signing key came from.
func (s *Server) recordSigningKeySource(keyName, source string) {
	s.signingKeySourcesMu.Lock()
	defer s.signingKeySourcesMu.Unlock()
	if s.signingKeySources == nil {
		s.signingKeySources = make(map[string]string)
	}
	s.signingKeySources[keyName] = source
}

// signingKeySource returns the source label of the named signing key as
// resolved at startup, or "unresolved". It never returns key material.
func (s *Server) signingKeySource(keyName string) string {
	if keyName == SecretKeyOIDCSigningKey {
		if src := s.oidcKeyManager.Source(); src != "" {
			return src
		}
		return signingKeySourceUnresolved
	}
	s.signingKeySourcesMu.Lock()
	defer s.signingKeySourcesMu.Unlock()
	if src := s.signingKeySources[keyName]; src != "" {
		return src
	}
	return signingKeySourceUnresolved
}

// testAdminSigningKeyClass is one signing-key class the gate checks.
type testAdminSigningKeyClass struct {
	Class   string
	KeyName string
}

// testAdminSigningKeyClasses lists every hub signing-key class: user JWTs,
// agent tokens, signed download URLs, artifact view capability URLs and
// list cursors. The OIDC identity-token key is added when OIDC is enabled.
func testAdminSigningKeyClasses(oidcEnabled bool) []testAdminSigningKeyClass {
	classes := []testAdminSigningKeyClass{
		{Class: "user_jwt", KeyName: SecretKeyUserSigningKey},
		{Class: "agent_token", KeyName: SecretKeyAgentSigningKey},
		{Class: "download_url", KeyName: SecretKeyDownloadSigningKey},
		{Class: "artifact_view_url", KeyName: SecretKeyArtifactViewKey},
		{Class: "list_cursor", KeyName: SecretKeyListCursorKey},
	}
	if oidcEnabled {
		classes = append(classes, testAdminSigningKeyClass{Class: "oidc", KeyName: SecretKeyOIDCSigningKey})
	}
	return classes
}

// verifyTestAdminSigningKeys refuses the gate unless every signing-key
// class resolved from a hub-local source (G7).
func (s *Server) verifyTestAdminSigningKeys(oidcEnabled bool) error {
	for _, c := range testAdminSigningKeyClasses(oidcEnabled) {
		if src := s.signingKeySource(c.KeyName); !signingKeySourceIsHubLocal(src) {
			return fmt.Errorf("%w: %s key source is %q", ErrTestAdminSigningKeyNotHubLocal, c.Class, src)
		}
	}
	return nil
}

// emitTestAdminGateStart writes the startup banner and the gate-start event
// through the test admin audit sink. A write failure refuses startup.
func (s *Server) emitTestAdminGateStart() error {
	g := s.testAdminGate
	kinds := []string{}
	if g.hubAdmin {
		kinds = append(kinds, string(testAdminKindHubAdmin))
	}
	if g.superAdmin {
		kinds = append(kinds, string(testAdminKindSuperAdmin))
	}
	markerAt := ""
	if !g.markerCreatedAt.IsZero() {
		markerAt = g.markerCreatedAt.UTC().Format(time.RFC3339)
	}
	if err := s.testAdminAudit.emit(testAdminAuditEventGateStart, map[string]any{
		"level":             "WARN",
		"msg":               "TEST ADMIN TIER ENABLED: this hub can issue test identities with hub-admin authority. Test hubs only.",
		"kinds":             kinds,
		"hub_id":            g.hubID,
		"hub_class":         g.hubClass,
		"marker_created_at": markerAt,
	}); err != nil {
		return fmt.Errorf("test admin tier: startup banner could not be written: %w", err)
	}
	return nil
}

// testAdminBrokerAdmission is BrokerAuthService.brokerAdmission. While the
// test admin tier is on it admits only the co-located (embedded) broker and
// the hub's own message-broker plugin records; every other broker is
// refused with ErrTestAdminRemoteBrokerRefused.
func (s *Server) testAdminBrokerAdmission(ctx context.Context, brokerID string) error {
	if !s.testAdminTierOn() {
		return nil
	}
	if brokerID != "" && s.isEmbeddedBroker(brokerID) {
		return nil
	}
	if brokerID != "" && s.store != nil {
		if b, err := s.store.GetRuntimeBroker(ctx, brokerID); err == nil && isPluginBroker(b) {
			return nil
		}
	}
	return ErrTestAdminRemoteBrokerRefused
}

// refuseTestAdminBrokerRegistration writes a 403 and returns true when the
// test admin tier is on and brokerID is not the co-located broker. HTTP
// registration paths call it before any lookup or mutation; the co-located
// broker registers in-process and never reaches them.
func (s *Server) refuseTestAdminBrokerRegistration(w http.ResponseWriter, brokerID string) bool {
	if !s.testAdminTierOn() {
		return false
	}
	if brokerID != "" && s.isEmbeddedBroker(brokerID) {
		return false
	}
	writeTestAdminBrokerRefusal(w)
	return true
}

func writeTestAdminBrokerRefusal(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, ErrCodeTestAdminRemoteBrokerRefused,
		ErrTestAdminRemoteBrokerRefused.Detail, nil)
}

// VerifyTestAdminBrokerSet is the startup broker check. While the test
// admin tier is on it fails when any runtime broker other than the
// co-located one is registered and active (status not offline). Message
// broker plugin records are not runtime brokers and are skipped. Offline
// brokers are tolerated here because broker authentication refuses every
// non-co-located broker while the tier is on (testAdminBrokerAdmission).
func (s *Server) VerifyTestAdminBrokerSet(ctx context.Context) error {
	if !s.testAdminTierOn() {
		return nil
	}
	embedded := s.embeddedBrokerSnapshot().id
	opts := store.ListOptions{Limit: 200}
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		if err != nil {
			return fmt.Errorf("%w: list runtime brokers: %v", ErrTestAdminRemoteBrokerActive, err)
		}
		for i := range page.Items {
			b := &page.Items[i]
			if isPluginBroker(b) || (embedded != "" && b.ID == embedded) {
				continue
			}
			if b.Status != store.BrokerStatusOffline {
				return fmt.Errorf("%w: broker %s (%s) has status %q; remove it or stop it before enabling the tier",
					ErrTestAdminRemoteBrokerActive, b.ID, b.Name, b.Status)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}
	return nil
}

// errTestAdminFederationRefused is the 422 detail for a federation write
// while the test admin tier is on.
var errTestAdminFederationRefused = errors.New("federation settings cannot be changed while the test admin tier is on")

// logTestAdminFederationSkipped records that a federation snapshot was not
// applied because the test admin tier is on.
func logTestAdminFederationSkipped() {
	slog.Error("Federation configuration ignored: the test admin tier is on and requires federation to be off")
}

// VerifyTestAdminFederationOff is the post-settings startup half of G8:
// once operational settings are loaded, no federation may be configured in
// the stored settings either, and no federation authenticator may be
// installed. cmd calls it before the hub is reported started.
func (s *Server) VerifyTestAdminFederationOff() error {
	if !s.testAdminTierOn() {
		return nil
	}
	if federationConfigured(&s.config.Federation) {
		return fmt.Errorf("%w: startup configuration", ErrTestAdminFederationConfigured)
	}
	if ops := s.GetOperationalSettings(); ops != nil {
		if snap := ops.Snapshot(); federationConfigured(snap.FederationConfig) {
			return fmt.Errorf("%w: stored operational settings", ErrTestAdminFederationConfigured)
		}
	}
	if s.federationAuth.Load() != nil {
		return fmt.Errorf("%w: a federation authenticator is installed", ErrTestAdminFederationConfigured)
	}
	return nil
}
