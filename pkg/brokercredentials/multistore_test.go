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

package brokercredentials

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMultiStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	creds := &BrokerCredentials{
		Name:         "test-hub",
		BrokerID:     "broker-123",
		SecretKey:    base64.StdEncoding.EncodeToString([]byte("secret")),
		HubEndpoint:  "https://hub.example.com",
		AuthMode:     AuthModeHMAC,
		RegisteredAt: time.Now().Truncate(time.Second),
	}

	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load("test-hub")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Name != creds.Name {
		t.Errorf("Name mismatch: expected %q, got %q", creds.Name, loaded.Name)
	}
	if loaded.BrokerID != creds.BrokerID {
		t.Errorf("BrokerID mismatch: expected %q, got %q", creds.BrokerID, loaded.BrokerID)
	}
	if loaded.SecretKey != creds.SecretKey {
		t.Errorf("SecretKey mismatch: expected %q, got %q", creds.SecretKey, loaded.SecretKey)
	}
	if loaded.HubEndpoint != creds.HubEndpoint {
		t.Errorf("HubEndpoint mismatch: expected %q, got %q", creds.HubEndpoint, loaded.HubEndpoint)
	}
	if loaded.AuthMode != creds.AuthMode {
		t.Errorf("AuthMode mismatch: expected %q, got %q", creds.AuthMode, loaded.AuthMode)
	}
}

func TestMultiStore_List(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Empty list
	list, err := store.List()
	if err != nil {
		t.Fatalf("List on empty dir failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("Expected empty list, got %d items", len(list))
	}

	// Save two credentials
	creds1 := &BrokerCredentials{
		Name:      "hub-one",
		BrokerID:  "broker-1",
		SecretKey: "c2VjcmV0MQ==",
		AuthMode:  AuthModeHMAC,
	}
	creds2 := &BrokerCredentials{
		Name:      "hub-two",
		BrokerID:  "broker-2",
		SecretKey: "c2VjcmV0Mg==",
		AuthMode:  AuthModeHMAC,
	}

	if err := store.Save(creds1); err != nil {
		t.Fatalf("Save creds1 failed: %v", err)
	}
	if err := store.Save(creds2); err != nil {
		t.Fatalf("Save creds2 failed: %v", err)
	}

	list, err = store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Expected 2 items, got %d", len(list))
	}

	// Verify both are present
	found := map[string]bool{}
	for _, c := range list {
		found[c.Name] = true
	}
	if !found["hub-one"] {
		t.Error("Missing hub-one in list")
	}
	if !found["hub-two"] {
		t.Error("Missing hub-two in list")
	}
}

func TestMultiStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	creds := &BrokerCredentials{
		Name:      "to-delete",
		BrokerID:  "broker-del",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeHMAC,
	}

	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if !store.Exists("to-delete") {
		t.Fatal("Expected credential to exist after save")
	}

	if err := store.Delete("to-delete"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if store.Exists("to-delete") {
		t.Error("Expected credential to not exist after delete")
	}

	// Delete again should not error
	if err := store.Delete("to-delete"); err != nil {
		t.Errorf("Delete of non-existent file should not error: %v", err)
	}
}

func TestMultiStore_Exists(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	if store.Exists("nonexistent") {
		t.Error("Expected false for nonexistent credential")
	}

	creds := &BrokerCredentials{
		Name:      "exists-test",
		BrokerID:  "broker-ex",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeHMAC,
	}
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if !store.Exists("exists-test") {
		t.Error("Expected true for existing credential")
	}
}

func TestMultiStore_DevAuthLimit(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Save first dev-auth connection
	creds1 := &BrokerCredentials{
		Name:      "dev-one",
		BrokerID:  "broker-dev1",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeDevAuth,
	}
	if err := store.Save(creds1); err != nil {
		t.Fatalf("Save first dev-auth failed: %v", err)
	}

	// Save second dev-auth connection should fail
	creds2 := &BrokerCredentials{
		Name:      "dev-two",
		BrokerID:  "broker-dev2",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeDevAuth,
	}
	err := store.Save(creds2)
	if err == nil {
		t.Fatal("Expected error when saving second dev-auth connection")
	}
	if !errors.Is(err, ErrDevAuthLimit) {
		t.Errorf("Expected ErrDevAuthLimit, got: %v", err)
	}

	// Non-dev-auth should still work
	creds3 := &BrokerCredentials{
		Name:      "prod-one",
		BrokerID:  "broker-prod1",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeHMAC,
	}
	if err := store.Save(creds3); err != nil {
		t.Fatalf("Save HMAC connection should work: %v", err)
	}
}

func TestMultiStore_DevAuthLimit_SameNameAllowed(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Save dev-auth connection
	creds := &BrokerCredentials{
		Name:      "dev-hub",
		BrokerID:  "broker-dev",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeDevAuth,
	}
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Updating the same name should succeed
	creds.SecretKey = "bmV3LXNlY3JldA=="
	if err := store.Save(creds); err != nil {
		t.Fatalf("Updating same dev-auth name should succeed: %v", err)
	}
}

func TestMultiStore_MigrateFromLegacy(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(filepath.Join(dir, "hub-credentials"))

	// Create a legacy credentials file
	legacyCreds := &BrokerCredentials{
		BrokerID:     "legacy-broker",
		SecretKey:    base64.StdEncoding.EncodeToString([]byte("legacy-secret")),
		HubEndpoint:  "https://hub.scion.dev",
		RegisteredAt: time.Now().Truncate(time.Second),
	}
	legacyData, _ := json.MarshalIndent(legacyCreds, "", "  ")
	legacyPath := filepath.Join(dir, "broker-credentials.json")
	if err := os.WriteFile(legacyPath, legacyData, 0600); err != nil {
		t.Fatalf("Failed to write legacy file: %v", err)
	}

	if err := store.MigrateFromLegacy(legacyPath); err != nil {
		t.Fatalf("MigrateFromLegacy failed: %v", err)
	}

	// Legacy file should be renamed to .bak
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Error("Expected legacy file to be renamed")
	}
	if _, err := os.Stat(legacyPath + ".bak"); err != nil {
		t.Error("Expected .bak file to exist")
	}

	// Should be able to load the migrated credentials
	derivedName := DeriveHubName("https://hub.scion.dev")
	loaded, err := store.Load(derivedName)
	if err != nil {
		t.Fatalf("Failed to load migrated credentials: %v", err)
	}

	if loaded.BrokerID != "legacy-broker" {
		t.Errorf("BrokerID mismatch: got %q", loaded.BrokerID)
	}
	if loaded.Name != derivedName {
		t.Errorf("Name should be derived: expected %q, got %q", derivedName, loaded.Name)
	}
	if loaded.AuthMode != AuthModeHMAC {
		t.Errorf("AuthMode should default to hmac: got %q", loaded.AuthMode)
	}
}

func TestMultiStore_MigrateFromLegacy_NoFile(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Migration with no legacy file should be a no-op
	err := store.MigrateFromLegacy(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Fatalf("MigrateFromLegacy with no file should not error: %v", err)
	}
}

func TestMultiStore_LoadAllIfChanged(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// No directory: should return no change
	_, _, _, err := store.LoadAllIfChanged(time.Time{})
	if err != nil {
		t.Fatalf("LoadAllIfChanged failed: %v", err)
	}
	// Note: the dir will be created by Save below

	// Save a credential
	creds := &BrokerCredentials{
		Name:      "test-hub",
		BrokerID:  "broker-1",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeHMAC,
	}
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Should detect changes with zero lastScan
	result, scanTime, changed, err := store.LoadAllIfChanged(time.Time{})
	if err != nil {
		t.Fatalf("LoadAllIfChanged failed: %v", err)
	}
	if !changed {
		t.Error("Expected change to be detected with zero lastScan")
	}
	if len(result) != 1 {
		t.Errorf("Expected 1 credential, got %d", len(result))
	}

	// Should not detect changes with current scan time
	_, _, changed, err = store.LoadAllIfChanged(scanTime)
	if err != nil {
		t.Fatalf("LoadAllIfChanged failed: %v", err)
	}
	if changed {
		t.Error("Expected no change with current scan time")
	}

	// Modify a file
	time.Sleep(10 * time.Millisecond)
	creds.SecretKey = "bmV3LXNlY3JldA=="
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save update failed: %v", err)
	}

	// Should detect the change
	result, _, changed, err = store.LoadAllIfChanged(scanTime)
	if err != nil {
		t.Fatalf("LoadAllIfChanged failed: %v", err)
	}
	if !changed {
		t.Error("Expected change to be detected after file modification")
	}
	if len(result) != 1 {
		t.Errorf("Expected 1 credential, got %d", len(result))
	}
}

func TestMultiStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	creds := &BrokerCredentials{
		Name:      "perm-test",
		BrokerID:  "broker-perm",
		SecretKey: "c2VjcmV0",
		AuthMode:  AuthModeHMAC,
	}
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Check file permissions
	info, err := os.Stat(filepath.Join(dir, "perm-test.json"))
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != FileMode {
		t.Errorf("Expected file permissions %o, got %o", FileMode, perm)
	}
}

func TestMultiStore_ValidateName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expectErr bool
	}{
		{"valid simple", "prod", false},
		{"valid with hyphens", "my-hub", false},
		{"valid with numbers", "hub-123", false},
		{"valid single char", "a", false},
		{"empty", "", true},
		{"starts with hyphen", "-hub", true},
		{"ends with hyphen", "hub-", true},
		{"contains uppercase", "MyHub", true},
		{"contains spaces", "my hub", true},
		{"contains dots", "hub.scion.dev", true},
		{"contains underscores", "my_hub", true},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true}, // 66 chars
		{"max length", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false}, // 63 chars
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.input)
			if tc.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestDeriveHubName(t *testing.T) {
	tests := []struct {
		endpoint string
		expected string
	}{
		{"https://hub.scion.dev", "hub-scion-dev"},
		{"http://localhost:8080", "localhost"},
		{"http://localhost:9090", "localhost-9090"},
		{"http://localhost", "localhost"},
		{"https://hub.example.com:443", "hub-example-com"},
		{"http://hub.example.com:80", "hub-example-com"},
		{"https://staging.hub.scion.dev", "staging-hub-scion-dev"},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			result := DeriveHubName(tc.endpoint)
			if result != tc.expected {
				t.Errorf("DeriveHubName(%q) = %q, want %q", tc.endpoint, result, tc.expected)
			}
		})
	}
}

func TestMultiStore_LoadNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	_, err := store.Load("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Expected ErrNotFound, got %v", err)
	}
}

func TestMultiStore_SaveValidation(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	tests := []struct {
		name      string
		creds     *BrokerCredentials
		expectErr bool
	}{
		{
			name:      "nil credentials",
			creds:     nil,
			expectErr: true,
		},
		{
			name: "missing name",
			creds: &BrokerCredentials{
				BrokerID:  "broker-1",
				SecretKey: "abc",
			},
			expectErr: true,
		},
		{
			name: "missing broker ID",
			creds: &BrokerCredentials{
				Name:      "test",
				BrokerID:  "",
				SecretKey: "abc",
			},
			expectErr: true,
		},
		{
			name: "missing secret key",
			creds: &BrokerCredentials{
				Name:      "test",
				BrokerID:  "broker-1",
				SecretKey: "",
			},
			expectErr: true,
		},
		{
			name: "invalid name",
			creds: &BrokerCredentials{
				Name:      "Invalid Name!",
				BrokerID:  "broker-1",
				SecretKey: "abc",
			},
			expectErr: true,
		},
		{
			name: "valid credentials",
			creds: &BrokerCredentials{
				Name:      "valid",
				BrokerID:  "broker-1",
				SecretKey: "abc",
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Save(tc.creds)
			if tc.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestMultiStore_TransportFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	creds := &BrokerCredentials{
		Name:              "iap-hub",
		BrokerID:          "broker-iap",
		SecretKey:         base64.StdEncoding.EncodeToString([]byte("secret")),
		HubEndpoint:       "https://iap-hub.example.com",
		AuthMode:          AuthModeHMAC,
		TransportMode:     "iap",
		TransportAudience: "12345.apps.googleusercontent.com",
	}

	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load("iap-hub")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.TransportMode != "iap" {
		t.Errorf("TransportMode: expected %q, got %q", "iap", loaded.TransportMode)
	}
	if loaded.TransportAudience != "12345.apps.googleusercontent.com" {
		t.Errorf("TransportAudience: expected %q, got %q", "12345.apps.googleusercontent.com", loaded.TransportAudience)
	}
}

func TestMultiStore_TransportFieldsBackwardCompatibility(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Write old-format JSON without transport fields
	oldJSON := `{"name": "old-hub", "brokerId": "old-broker", "secretKey": "c2VjcmV0"}`
	if err := os.MkdirAll(dir, DirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-hub.json"), []byte(oldJSON), FileMode); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load("old-hub")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.TransportMode != "" {
		t.Errorf("Expected empty TransportMode, got %q", loaded.TransportMode)
	}
	if loaded.TransportAudience != "" {
		t.Errorf("Expected empty TransportAudience, got %q", loaded.TransportAudience)
	}
}

func TestMultiStore_MixedTransportConfigs(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// IAP hub
	iapCreds := &BrokerCredentials{
		Name:              "iap-hub",
		BrokerID:          "broker-iap",
		SecretKey:         base64.StdEncoding.EncodeToString([]byte("secret1")),
		HubEndpoint:       "https://iap-hub.example.com",
		AuthMode:          AuthModeHMAC,
		TransportMode:     "iap",
		TransportAudience: "12345.apps.googleusercontent.com",
	}
	// Plain hub (no transport auth)
	plainCreds := &BrokerCredentials{
		Name:        "plain-hub",
		BrokerID:    "broker-plain",
		SecretKey:   base64.StdEncoding.EncodeToString([]byte("secret2")),
		HubEndpoint: "https://plain-hub.example.com",
		AuthMode:    AuthModeHMAC,
	}

	if err := store.Save(iapCreds); err != nil {
		t.Fatalf("Save IAP failed: %v", err)
	}
	if err := store.Save(plainCreds); err != nil {
		t.Fatalf("Save plain failed: %v", err)
	}

	// Verify IAP hub has transport config
	iap, err := store.Load("iap-hub")
	if err != nil {
		t.Fatalf("Load IAP failed: %v", err)
	}
	if iap.TransportMode != "iap" {
		t.Errorf("IAP TransportMode: expected %q, got %q", "iap", iap.TransportMode)
	}

	// Verify plain hub has no transport config
	plain, err := store.Load("plain-hub")
	if err != nil {
		t.Fatalf("Load plain failed: %v", err)
	}
	if plain.TransportMode != "" {
		t.Errorf("Plain TransportMode: expected empty, got %q", plain.TransportMode)
	}
	if plain.TransportAudience != "" {
		t.Errorf("Plain TransportAudience: expected empty, got %q", plain.TransportAudience)
	}
}

func TestMultiStore_LoadPopulatesName(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)

	// Write a file without the Name field (simulating legacy data)
	creds := map[string]interface{}{
		"brokerId":  "broker-123",
		"secretKey": "c2VjcmV0",
	}
	data, _ := json.Marshal(creds)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "my-hub.json"), data, FileMode); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load("my-hub")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Name != "my-hub" {
		t.Errorf("Expected Name to be populated from filename: got %q", loaded.Name)
	}
}
