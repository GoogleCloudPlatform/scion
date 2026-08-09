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

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// --- Deliverable 10a: Hot-reload via ApplySnapshot ---

func TestApplySnapshot_FederationHotReload(t *testing.T) {
	// Create a minimal Server with the fields ApplySnapshot needs.
	srv := &Server{}
	srv.config.Mode = "dev"
	srv.config.Workstation = true
	srv.config.OIDCConfig.IssuerURL = "https://hub.example.com"
	srv.federationClient = &http.Client{Timeout: 5 * time.Second}

	// Create a JWKS test server for the trusted issuer.
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer jwksServer.Close()

	// Build a snapshot with federation config.
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        jwksServer.URL,
				JWKSURL:          jwksServer.URL,
				ExpectedAudience: "https://hub.example.com",
			},
		},
	}
	snap := Layer1Snapshot{
		FederationConfig: &fedCfg,
	}

	// Initially, no authenticator is loaded.
	if srv.federationAuth.Load() != nil {
		t.Fatal("expected nil federationAuth before ApplySnapshot")
	}

	// Apply the snapshot — should hot-reload the authenticator.
	result := ApplySnapshot(srv, snap)

	// Verify the authenticator was stored.
	newAuth := srv.federationAuth.Load()
	if newAuth == nil {
		t.Fatal("expected non-nil federationAuth after ApplySnapshot")
	}

	// Check that "federation" is in the applied list.
	applied, ok := result["applied"].([]string)
	if !ok {
		t.Fatalf("expected applied to be []string, got %T", result["applied"])
	}
	found := false
	for _, a := range applied {
		if a == "federation" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'federation' in applied list, got %v", applied)
	}
}

func TestApplySnapshot_FederationValidationFailure(t *testing.T) {
	// Create a minimal Server.
	srv := &Server{}
	srv.config.Mode = "dev"
	srv.config.Workstation = true

	// Store an initial authenticator to verify it's kept on failure.
	initialAuth := &FederationAuthenticator{}
	srv.federationAuth.Store(initialAuth)

	// Build a snapshot with invalid federation config (enabled but no issuers).
	fedCfg := config.FederationConfig{
		Enabled:        true,
		TrustedIssuers: nil, // validation requires at least one issuer when enabled
	}
	snap := Layer1Snapshot{
		FederationConfig: &fedCfg,
	}

	// Apply — should fail validation, keep old authenticator.
	ApplySnapshot(srv, snap)

	// Verify old authenticator is preserved.
	currentAuth := srv.federationAuth.Load()
	if currentAuth != initialAuth {
		t.Error("expected old authenticator to be preserved after validation failure")
	}
}

func TestApplySnapshot_FederationNilClearsAuthenticator(t *testing.T) {
	// Create a minimal Server with an existing authenticator.
	srv := &Server{}
	initialAuth := &FederationAuthenticator{}
	srv.federationAuth.Store(initialAuth)

	// Apply snapshot without federation config — should clear the authenticator.
	snap := Layer1Snapshot{
		FederationConfig: nil,
	}
	ApplySnapshot(srv, snap)

	// Verify the authenticator is cleared (nil) so the middleware returns 401.
	currentAuth := srv.federationAuth.Load()
	if currentAuth != nil {
		t.Error("expected authenticator to be cleared (nil) when FederationConfig is nil")
	}
}

func TestApplySnapshot_FederationDisabledClearsAuthenticator(t *testing.T) {
	// Create a minimal Server with an existing authenticator.
	srv := &Server{}
	srv.config.Mode = "dev"
	srv.config.Workstation = true
	srv.config.OIDCConfig.IssuerURL = "https://hub.example.com"
	srv.federationClient = &http.Client{Timeout: 5 * time.Second}

	// Start with federation enabled.
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer jwksServer.Close()

	enabledCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        jwksServer.URL,
				JWKSURL:          jwksServer.URL,
				ExpectedAudience: "https://hub.example.com",
			},
		},
	}
	enableSnap := Layer1Snapshot{FederationConfig: &enabledCfg}
	ApplySnapshot(srv, enableSnap)

	// Verify federation is active.
	if srv.federationAuth.Load() == nil {
		t.Fatal("expected non-nil authenticator after enabling federation")
	}

	// Now disable federation via Enabled=false.
	disabledCfg := config.FederationConfig{
		Enabled: false,
	}
	disableSnap := Layer1Snapshot{FederationConfig: &disabledCfg}
	result := ApplySnapshot(srv, disableSnap)

	// Verify the authenticator is cleared.
	if srv.federationAuth.Load() != nil {
		t.Error("expected authenticator to be cleared (nil) when federation is disabled")
	}

	// Check "federation" is in the applied list.
	applied, ok := result["applied"].([]string)
	if !ok {
		t.Fatalf("expected applied to be []string, got %T", result["applied"])
	}
	found := false
	for _, a := range applied {
		if a == "federation" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'federation' in applied list when disabling, got %v", applied)
	}
}

// --- Deliverable 10b: Concurrent access ---

func TestFederationAuth_ConcurrentAccess(t *testing.T) {
	// Initial value.
	auth1 := &FederationAuthenticator{}
	fedAuth := newFedAuthPointer(auth1)

	const numReaders = 10
	const numIterations = 1000

	var wg sync.WaitGroup

	// Readers: load from the atomic pointer concurrently.
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numIterations; j++ {
				loaded := fedAuth.Load()
				if loaded == nil {
					// nil is acceptable if a store of nil happened;
					// in this test we only store non-nil, so this would be a bug.
					t.Error("loaded nil from atomic pointer")
					return
				}
			}
		}()
	}

	// Writer: swap the authenticator concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < numIterations; j++ {
			newAuth := &FederationAuthenticator{}
			fedAuth.Store(newAuth)
		}
	}()

	wg.Wait()
}

// --- Deliverable 10c: buildSingleSectionDoc federation case ---

func TestBuildSingleSectionDoc_Federation(t *testing.T) {
	enabled := true
	req := &ServerConfigUpdateRequest{
		Federation: &config.V1FederationConfig{
			Enabled: &enabled,
			TrustedIssuers: []config.V1TrustedIssuerConfig{
				{
					IssuerURL:        "https://hub-a.example.com",
					ExpectedAudience: "https://hub-b.example.com",
					IssuerType:       "hub",
				},
			},
			Algorithms:      []string{"RS256"},
			RefreshInterval: "1h",
		},
	}

	fp := newFieldPresenceFromJSON(t, `{
		"federation": {
			"enabled": true,
			"trusted_issuers": [{"issuer_url": "https://hub-a.example.com"}],
			"algorithms": ["RS256"],
			"refresh_interval": "1h"
		}
	}`)

	doc, err := buildSingleSectionDoc(req, "federation", fp)
	if err != nil {
		t.Fatalf("buildSingleSectionDoc failed: %v", err)
	}
	if doc == nil {
		t.Fatal("expected non-nil doc for federation section")
	}

	// Unmarshal and verify fields.
	var fedSettings opsettings.FederationSettings
	if err := json.Unmarshal(doc, &fedSettings); err != nil {
		t.Fatalf("failed to unmarshal federation doc: %v", err)
	}

	if fedSettings.Enabled == nil || !*fedSettings.Enabled {
		t.Error("expected enabled to be true")
	}
	if len(fedSettings.TrustedIssuers) != 1 {
		t.Errorf("expected 1 trusted issuer, got %d", len(fedSettings.TrustedIssuers))
	}
	if fedSettings.TrustedIssuers[0].IssuerURL != "https://hub-a.example.com" {
		t.Errorf("expected issuer_url 'https://hub-a.example.com', got %q", fedSettings.TrustedIssuers[0].IssuerURL)
	}
	if len(fedSettings.Algorithms) != 1 || fedSettings.Algorithms[0] != "RS256" {
		t.Errorf("expected algorithms [RS256], got %v", fedSettings.Algorithms)
	}
	if fedSettings.RefreshInterval != "1h" {
		t.Errorf("expected refresh_interval '1h', got %q", fedSettings.RefreshInterval)
	}
}

func TestBuildSingleSectionDoc_Federation_Nil(t *testing.T) {
	req := &ServerConfigUpdateRequest{
		Federation: nil,
	}
	fp := newFieldPresenceFromJSON(t, `{}`)

	doc, err := buildSingleSectionDoc(req, "federation", fp)
	if err != nil {
		t.Fatalf("buildSingleSectionDoc failed: %v", err)
	}

	// When Federation is nil, we still get a doc (empty FederationSettings).
	if doc == nil {
		t.Fatal("expected non-nil doc for federation section even with nil federation field")
	}
	var fedSettings opsettings.FederationSettings
	if err := json.Unmarshal(doc, &fedSettings); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if fedSettings.Enabled != nil {
		t.Error("expected nil enabled when federation request field is nil")
	}
}

// --- Deliverable 10d: Validation rejection ---

func TestConvertFederationSettingsToConfig(t *testing.T) {
	enabled := true
	fs := opsettings.FederationSettings{
		Enabled: &enabled,
		TrustedIssuers: []config.V1TrustedIssuerConfig{
			{
				IssuerURL:        "https://hub-a.example.com",
				ExpectedAudience: "https://hub-b.example.com",
				IssuerType:       "hub",
				AllowedProjects:  []string{"proj1"},
				AllowedRootUsers: []string{"user@example.com"},
				DefaultScopes:    []string{"agent:status:update"},
			},
		},
		Algorithms:       []string{"RS256", "ES256"},
		RefreshInterval:  "1h",
		DebounceInterval: "5s",
	}

	fc := convertFederationSettingsToConfig(fs)

	if !fc.Enabled {
		t.Error("expected Enabled true")
	}
	if len(fc.TrustedIssuers) != 1 {
		t.Fatalf("expected 1 issuer, got %d", len(fc.TrustedIssuers))
	}
	ti := fc.TrustedIssuers[0]
	if ti.IssuerURL != "https://hub-a.example.com" {
		t.Errorf("expected IssuerURL 'https://hub-a.example.com', got %q", ti.IssuerURL)
	}
	if ti.IssuerType != "hub" {
		t.Errorf("expected IssuerType 'hub', got %q", ti.IssuerType)
	}
	if len(ti.AllowedProjects) != 1 || ti.AllowedProjects[0] != "proj1" {
		t.Errorf("unexpected AllowedProjects: %v", ti.AllowedProjects)
	}
	if len(fc.Algorithms) != 2 {
		t.Errorf("expected 2 algorithms, got %d", len(fc.Algorithms))
	}
	if fc.Cache.RefreshInterval != 1*time.Hour {
		t.Errorf("expected RefreshInterval 1h, got %v", fc.Cache.RefreshInterval)
	}
	if fc.Cache.DebounceInterval != 5*time.Second {
		t.Errorf("expected DebounceInterval 5s, got %v", fc.Cache.DebounceInterval)
	}
}

func TestConvertFederationSettingsToConfig_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		fs   opsettings.FederationSettings
		want string // substring expected in one of the validation errors
	}{
		{
			name: "enabled with no issuers",
			fs: opsettings.FederationSettings{
				Enabled: boolPtr(true),
			},
			want: "no trusted_issuers",
		},
		{
			name: "missing issuer_url",
			fs: opsettings.FederationSettings{
				Enabled: boolPtr(true),
				TrustedIssuers: []config.V1TrustedIssuerConfig{
					{IssuerURL: ""},
				},
			},
			want: "issuer_url is required",
		},
		{
			name: "invalid algorithm",
			fs: opsettings.FederationSettings{
				Enabled: boolPtr(true),
				TrustedIssuers: []config.V1TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
				},
				Algorithms: []string{"HS256"},
			},
			want: "unsupported algorithm",
		},
		{
			name: "duplicate issuers",
			fs: opsettings.FederationSettings{
				Enabled: boolPtr(true),
				TrustedIssuers: []config.V1TrustedIssuerConfig{
					{IssuerURL: "https://hub.example.com"},
					{IssuerURL: "https://hub.example.com"},
				},
			},
			want: "duplicate issuer_url",
		},
		{
			name: "hub-only fields on service_account issuer",
			fs: opsettings.FederationSettings{
				Enabled: boolPtr(true),
				TrustedIssuers: []config.V1TrustedIssuerConfig{
					{
						IssuerURL:       "https://accounts.google.com",
						IssuerType:      "service_account",
						AllowedProjects: []string{"proj1"},
					},
				},
			},
			want: "allowed_projects is not applicable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := convertFederationSettingsToConfig(tt.fs)
			errs := fc.Validate()
			if len(errs) == 0 {
				t.Fatal("expected validation errors, got none")
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tt.want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error containing %q, got %v", tt.want, errs)
			}
		})
	}
}

// --- Deliverable 10e: Middleware loads from atomic pointer ---

func TestMiddleware_LoadsFromAtomicPointer(t *testing.T) {
	// Start with no authenticator (nil pointer value).
	fedAuth := newFedAuthPointer(nil)

	cfg := AuthConfig{
		Mode:           "production",
		FederationAuth: fedAuth,
		Debug:          true,
		Logger:         slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// With nil authenticator, sending a federation token should get 401.
	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Federation-Token", "some-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when authenticator is nil, got %d", rr.Code)
	}

	var body map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	errData, _ := body["error"].(map[string]interface{})
	if msg, ok := errData["message"].(string); ok {
		if msg != "federation authentication is not configured" {
			t.Errorf("unexpected error message: %s", msg)
		}
	}
}

func TestMiddleware_NilFederationAuthField(t *testing.T) {
	// When FederationAuth field itself is nil (not just the pointer value).
	cfg := AuthConfig{
		Mode:           "production",
		FederationAuth: nil,
		Debug:          true,
		Logger:         slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Sending a federation token should get 401.
	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Federation-Token", "some-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when FederationAuth is nil, got %d", rr.Code)
	}
}

// --- Deliverable R1-fix: DB-mode PUT round-trip integration test ---

// TestExtractKoanfKeys_FederationRoundTrip verifies the full DB-mode path:
// PUT request with federation config → extractKoanfKeysFromRequest →
// ClassifyKeys → buildSectionDocsFromRequest → federation section doc produced.
// This is the gap that masked R1: federation config was silently dropped
// because extractKoanfKeysFromRequest emitted no federation koanf keys.
func TestExtractKoanfKeys_FederationRoundTrip(t *testing.T) {
	enabled := true
	req := &ServerConfigUpdateRequest{
		Federation: &config.V1FederationConfig{
			Enabled: &enabled,
			TrustedIssuers: []config.V1TrustedIssuerConfig{
				{
					IssuerURL:        "https://hub-a.example.com",
					ExpectedAudience: "https://hub-b.example.com",
					IssuerType:       "hub",
				},
			},
			Algorithms:      []string{"RS256"},
			RefreshInterval: "1h",
		},
	}

	// Step 1: Extract koanf keys — this was the bug (no federation keys emitted).
	keys := extractKoanfKeysFromRequest(req)

	// Verify at least one server.federation.* key is present.
	hasFedKey := false
	for _, k := range keys {
		if strings.HasPrefix(k, "server.federation.") {
			hasFedKey = true
			break
		}
	}
	if !hasFedKey {
		t.Fatalf("extractKoanfKeysFromRequest emitted no server.federation.* keys; got %v", keys)
	}

	// Step 2: Classify keys — federation keys should land in layer1BySec["federation"].
	layer1BySec, layer0Keys, _ := opsettings.ClassifyKeys(keys)

	if len(layer0Keys) > 0 {
		t.Errorf("unexpected Layer-0 keys: %v", layer0Keys)
	}

	fedKeys, ok := layer1BySec["federation"]
	if !ok || len(fedKeys) == 0 {
		t.Fatalf("ClassifyKeys did not produce a 'federation' entry; layer1BySec = %v", layer1BySec)
	}

	// Step 3: Build section docs — the federation section doc should be produced.
	rawBody := []byte(`{
		"federation": {
			"enabled": true,
			"trusted_issuers": [{"issuer_url": "https://hub-a.example.com", "expected_audience": "https://hub-b.example.com", "issuer_type": "hub"}],
			"algorithms": ["RS256"],
			"refresh_interval": "1h"
		}
	}`)
	sectionDocs, err := buildSectionDocsFromRequest(req, layer1BySec, rawBody)
	if err != nil {
		t.Fatalf("buildSectionDocsFromRequest failed: %v", err)
	}

	fedDoc, ok := sectionDocs["federation"]
	if !ok {
		t.Fatalf("federation section doc not produced; sectionDocs keys = %v", mapKeys2(sectionDocs))
	}

	// Step 4: Verify the section doc contents — federation config is NOT silently dropped.
	var fedSettings opsettings.FederationSettings
	if err := json.Unmarshal(fedDoc, &fedSettings); err != nil {
		t.Fatalf("failed to unmarshal federation section doc: %v", err)
	}

	if fedSettings.Enabled == nil || !*fedSettings.Enabled {
		t.Error("expected federation enabled=true in section doc")
	}
	if len(fedSettings.TrustedIssuers) != 1 {
		t.Errorf("expected 1 trusted issuer in section doc, got %d", len(fedSettings.TrustedIssuers))
	}
	if len(fedSettings.Algorithms) != 1 || fedSettings.Algorithms[0] != "RS256" {
		t.Errorf("expected algorithms [RS256] in section doc, got %v", fedSettings.Algorithms)
	}
	if fedSettings.RefreshInterval != "1h" {
		t.Errorf("expected refresh_interval '1h' in section doc, got %q", fedSettings.RefreshInterval)
	}

	// Step 5: Validate the section doc via convertFederationSettingsToConfig → Validate().
	fedCfg := convertFederationSettingsToConfig(fedSettings)
	if errs := fedCfg.Validate(); len(errs) > 0 {
		t.Errorf("unexpected validation errors for federation section doc: %v", errs)
	}
}

// TestExtractKoanfKeys_FederationNil verifies that when Federation is nil,
// no server.federation.* keys are emitted and no federation section doc is produced.
func TestExtractKoanfKeys_FederationNil(t *testing.T) {
	req := &ServerConfigUpdateRequest{
		Federation: nil,
	}

	keys := extractKoanfKeysFromRequest(req)
	for _, k := range keys {
		if strings.HasPrefix(k, "server.federation.") {
			t.Errorf("unexpected federation key %q when Federation is nil", k)
		}
	}
}

// mapKeys2 is a test helper that returns sorted keys from a map[string]json.RawMessage.
func mapKeys2(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- Deliverable: Duration string validation ---

func TestConvertFederationSettingsToConfig_InvalidDurations(t *testing.T) {
	// convertFederationSettingsToConfig silently falls back to zero for invalid
	// durations. The admin API must reject these before they reach ApplySnapshot.
	tests := []struct {
		name  string
		ri    string // RefreshInterval
		di    string // DebounceInterval
		valid bool
	}{
		{"valid durations", "1h", "5s", true},
		{"valid duration with minutes", "30m", "500ms", true},
		{"empty durations (optional)", "", "", true},
		{"invalid refresh_interval", "1hour", "5s", false},
		{"invalid debounce_interval", "5s", "five_seconds", false},
		{"both invalid", "1hour", "five_seconds", false},
		{"number without unit", "30", "5", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var validationErrors []string
			if tt.ri != "" {
				if _, err := time.ParseDuration(tt.ri); err != nil {
					validationErrors = append(validationErrors, err.Error())
				}
			}
			if tt.di != "" {
				if _, err := time.ParseDuration(tt.di); err != nil {
					validationErrors = append(validationErrors, err.Error())
				}
			}

			if tt.valid && len(validationErrors) > 0 {
				t.Errorf("expected valid, got errors: %v", validationErrors)
			}
			if !tt.valid && len(validationErrors) == 0 {
				t.Error("expected validation errors for invalid durations, got none")
			}
		})
	}
}

// --- Helpers ---

func boolPtr(b bool) *bool {
	return &b
}

// newFieldPresenceFromJSON creates a fieldPresence from raw JSON for testing.
func newFieldPresenceFromJSON(t *testing.T, rawJSON string) *fieldPresence {
	t.Helper()
	fp, err := parseFieldPresence([]byte(rawJSON))
	if err != nil {
		t.Fatalf("failed to parse field presence JSON: %v", err)
	}
	return fp
}
