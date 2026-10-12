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
// test identities that hold hub-admin (and, with --test-mode=super-admin,
// super-admin) authority. Its inputs are deploy-time only:
//
//	G1  test identities are on (implied by the level) and the hub has a
//	    user store
//	G2  --test-mode=admin:<id> with id == the resolved hub id
//	G3  --test-mode=super-admin, admitted only while the test-admin policy
//	    classification of this binary is complete (TestAdminPolicyComplete,
//	    test_admin_policy.go); requires G2
//	G4  server.hub.class is exactly "test" (never writable through the API)
//	G5  explicit --production is refused (--hosted is allowed)
//	G6  the database carries a test_infra_marker row for this hub id
//	G7  every signing-key class resolves from a hub-local source
//	G8  no federation is configured
//	G9  Helm renders no admin level (deploy/helm/scion-hub $unsafeToPass
//	    refuses --test-mode in hub.args; the chart only renders member)
//	G10 not a workstation hub
//
// G1, G2, G4, G5 and G6 apply to the member level too (test_mode.go).
//
// cmd's resolveTestMode checks G1-G8 and G10 and fails startup
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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAdminGateError is a named refusal of a --test-mode level (the member
// preconditions and the test admin tier gate). Condition is the gate
// condition (G1..G10, "broker" for the broker check, "test_mode" for the
// setting itself) and Name a stable snake_case identifier.
type TestAdminGateError struct {
	Condition string
	Name      string
	Detail    string
}

func (e *TestAdminGateError) Error() string {
	return fmt.Sprintf("test mode refused (%s %s): %s", e.Condition, e.Name, e.Detail)
}

func testAdminGateErr(condition, name, detail string) *TestAdminGateError {
	return &TestAdminGateError{Condition: condition, Name: name, Detail: detail}
}

// Named test admin tier refusals. Callers wrap them with %w and add detail;
// match with errors.Is.
var (
	ErrTestAdminRequiresHub = testAdminGateErr("G1", "requires_hub",
		"--test-mode needs a hub (--enable-hub)")
	ErrTestAdminRequiresTestIdentities = testAdminGateErr("G1", "requires_test_identities",
		"the test admin tier requires test identities to be on (--test-mode=admin turns on both)")
	ErrTestAdminRequiresUserStore = testAdminGateErr("G1", "requires_user_store",
		"--test-mode requires a hub with a user store")
	ErrTestAdminHubIDMismatch = testAdminGateErr("G2", "hub_id_mismatch",
		"the hub id in --test-mode=<level>:<hub id> must equal this hub's resolved hub id")
	ErrTestSuperAdminRequiresPolicyRegistry = testAdminGateErr("G3", "super_admin_requires_policy_registry",
		"--test-mode=super-admin is refused: the test super-admin kind needs a complete test-admin policy classification in this binary")
	ErrTestAdminHubClassNotTest = testAdminGateErr("G4", "hub_class_not_test",
		`server.hub.class must be exactly "test"`)
	ErrTestAdminProductionFlag = testAdminGateErr("G5", "production_flag",
		"--production is refused with --test-mode; use --hosted")
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
	// HubAdmin is true when --test-mode=admin passed validation.
	HubAdmin bool
	// SuperAdmin is true when --test-mode=super-admin was accepted (G3:
	// only with a complete test-admin policy classification). It requires
	// HubAdmin.
	SuperAdmin bool
	// HubID is the validated flag value; it equals ServerConfig.HubID.
	HubID string
	// HubClass is server.hub.class as loaded at startup.
	HubClass string
	// MarkerHubID, MarkerClass and MarkerCreatedAt come from the
	// test_infra_marker row.
	MarkerHubID     string
	MarkerClass     string
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
	if g.SuperAdmin {
		if !g.HubAdmin {
			return ErrTestAdminInconsistent
		}
		if err := TestAdminPolicyComplete(); err != nil {
			return fmt.Errorf("%w: %v", ErrTestSuperAdminRequiresPolicyRegistry, err)
		}
	}
	switch {
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
	case !config.IsTestHubClass(g.MarkerClass):
		return ErrTestAdminMarkerClassNotTest
	case federationConfigured(&cfg.Federation):
		return ErrTestAdminFederationConfigured
	case cfg.SharedSigningSecret != "":
		return ErrTestAdminSharedSigningSecret
	case len(cfg.UserTokenConfig.SigningKey) > 0 || len(cfg.AgentTokenConfig.SigningKey) > 0:
		return ErrTestAdminPreconfiguredSigningKey
	}
	return nil
}

// testAdminKind names an admin test-identity kind; the values are the
// stored user kinds.
type testAdminKind string

const (
	testAdminKindHubAdmin   testAdminKind = store.UserKindTestHubAdmin
	testAdminKindSuperAdmin testAdminKind = store.UserKindTestSuperAdmin
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
// identities (test_hub_admin and test_super_admin whose expiry is after
// the test identity clock), as reported by the health summary.
func (s *Server) liveTestAdminIdentityCount(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	return s.store.CountLiveTestAdminUsers(ctx, "", "", s.testIdentities.clock())
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
	// signingKeySourceSharedScope is a key persisted under a fixed scope
	// every node and hub sharing the database reads (not hub-id scoped).
	signingKeySourceSharedScope = "shared_scope"
)

// webSessionKeyName names the web session cookie key class for
// signingKeySource (the key itself lives in the WebServer).
const webSessionKeyName = "web_session_key"

// conduitGrantKeySource is the source label of the conduit stream grant
// key ring. It uses the same predicate as conduitGrantKeySet: the ring is
// persisted (under the fixed, not hub-id scoped "conduit" scope) exactly
// when an encryption key exists, otherwise it is memory-only.
func conduitGrantKeySource(encryptionKey []byte) string {
	if _, err := newDBConduitGrantKeyStore(nil, encryptionKey); err == nil {
		return signingKeySourceSharedScope
	}
	return signingKeySourceEphemeral
}

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
// agent tokens, signed download URLs, artifact view capability URLs, list
// cursors, conduit stream grants and the web session cookie key. The OIDC
// identity-token key is added when OIDC is enabled.
func testAdminSigningKeyClasses(oidcEnabled bool) []testAdminSigningKeyClass {
	classes := []testAdminSigningKeyClass{
		{Class: "user_jwt", KeyName: SecretKeyUserSigningKey},
		{Class: "agent_token", KeyName: SecretKeyAgentSigningKey},
		{Class: "download_url", KeyName: SecretKeyDownloadSigningKey},
		{Class: "artifact_view_url", KeyName: SecretKeyArtifactViewKey},
		{Class: "list_cursor", KeyName: SecretKeyListCursorKey},
		{Class: "conduit_grant", KeyName: conduitGrantKeySecretName},
		{Class: "web_session", KeyName: webSessionKeyName},
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
	sources := map[string]string{}
	for _, c := range testAdminSigningKeyClasses(s.oidcKeyManager != nil) {
		sources[c.Class] = s.signingKeySource(c.KeyName)
	}
	if err := s.testAdminAudit.emit(testAdminAuditEventGateStart, map[string]any{
		"level":               "WARN",
		"msg":                 testAdminGateStartMessage(g.superAdmin),
		"kinds":               kinds,
		"hub_id":              g.hubID,
		"hub_class":           g.hubClass,
		"marker_created_at":   markerAt,
		"signing_key_sources": sources,
		"federation":          "off",
		"test_mode":           string(s.TestModeLevel()),
		"preconditions":       s.testModePreconditions(),
	}); err != nil {
		return fmt.Errorf("test admin tier: startup banner could not be written: %w", err)
	}
	return nil
}

// RegisterInProcessPluginBroker records the id of a message-broker plugin
// record this process provisioned itself (cmd, where it creates the record
// and its HMAC secret). While the test admin tier is on, those ids and the
// co-located broker are the only brokers admitted.
func (s *Server) RegisterInProcessPluginBroker(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inProcessPluginBrokers == nil {
		s.inProcessPluginBrokers = make(map[string]bool)
	}
	s.inProcessPluginBrokers[id] = true
}

// testAdminAdmittedBroker reports whether brokerID is the co-located broker
// or a plugin record this process provisioned. Both are in-memory facts set
// by this process; no stored attribute is consulted.
func (s *Server) testAdminAdmittedBroker(brokerID string) bool {
	if brokerID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return brokerID == s.embeddedBrokerID || s.inProcessPluginBrokers[brokerID]
}

// testAdminBrokerAdmission is BrokerAuthService.brokerAdmission. While the
// test admin tier is on it admits only the co-located (embedded) broker and
// the plugin records this process provisioned; every other broker is
// refused with ErrTestAdminRemoteBrokerRefused.
func (s *Server) testAdminBrokerAdmission(_ context.Context, brokerID string) error {
	if !s.testAdminTierOn() || s.testAdminAdmittedBroker(brokerID) {
		return nil
	}
	return ErrTestAdminRemoteBrokerRefused
}

// refuseTestAdminPluginLabel writes a 422 and returns true when the test
// admin tier is on and labels carry the plugin broker label. Only the hub
// sets that label, in-process; no API caller may.
func (s *Server) refuseTestAdminPluginLabel(w http.ResponseWriter, labels map[string]string) bool {
	if !s.testAdminTierOn() {
		return false
	}
	if _, ok := labels[pluginBrokerLabel]; !ok {
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError,
		"the "+pluginBrokerLabel+" label is reserved for the hub while the test admin tier is on", nil)
	return true
}

// refuseTestAdminBrokerRegistration writes a 403 and returns true when the
// test admin tier is on and brokerID is not the co-located broker. HTTP
// registration paths call it before any lookup or mutation; the co-located
// broker registers in-process and never reaches them.
func (s *Server) refuseTestAdminBrokerRegistration(w http.ResponseWriter, brokerID string) bool {
	if !s.testAdminTierOn() {
		return false
	}
	if s.isEmbeddedBroker(brokerID) {
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
// admin tier is on it fails when any broker other than the co-located one
// and the plugin records this process provisioned is registered and active
// (status not offline). Offline brokers are tolerated here because broker
// authentication refuses every other broker while the tier is on
// (testAdminBrokerAdmission). On success it writes a test_admin_broker_set
// line through the test admin audit sink.
func (s *Server) VerifyTestAdminBrokerSet(ctx context.Context) error {
	if !s.testAdminTierOn() {
		return nil
	}
	embedded := s.embeddedBrokerSnapshot()
	opts := store.ListOptions{Limit: 200}
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		if err != nil {
			return fmt.Errorf("%w: list runtime brokers: %v", ErrTestAdminRemoteBrokerActive, err)
		}
		for i := range page.Items {
			b := &page.Items[i]
			if s.testAdminAdmittedBroker(b.ID) {
				continue
			}
			if b.Status != store.BrokerStatusOffline {
				if embedded.regErr != "" {
					return fmt.Errorf("%w: broker %s (%s) has status %q, and this hub's co-located broker failed to register (%s); "+
						"if that record is this hub's own broker, fix the co-located broker registration first",
						ErrTestAdminRemoteBrokerActive, b.ID, b.Name, b.Status, embedded.regErr)
				}
				return fmt.Errorf("%w: broker %s (%s) has status %q; remove it or stop it before enabling the tier",
					ErrTestAdminRemoteBrokerActive, b.ID, b.Name, b.Status)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}
	s.mu.RLock()
	plugins := make([]string, 0, len(s.inProcessPluginBrokers))
	for id := range s.inProcessPluginBrokers {
		plugins = append(plugins, id)
	}
	s.mu.RUnlock()
	sort.Strings(plugins)
	if err := s.testAdminAudit.emit(testAdminAuditEventBrokerSet, map[string]any{
		"level":              "WARN",
		"embedded_broker_id": embedded.id,
		"plugin_broker_ids":  plugins,
	}); err != nil {
		return fmt.Errorf("test admin tier: broker check line could not be written: %w", err)
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

// testAdminGateStartMessage is the startup banner text.
func testAdminGateStartMessage(superAdmin bool) string {
	if superAdmin {
		return "TEST ADMIN TIER ENABLED: this hub can issue test identities with hub-admin and super-admin authority. Test hubs only."
	}
	return "TEST ADMIN TIER ENABLED: this hub can issue test identities with hub-admin authority. Test hubs only."
}
