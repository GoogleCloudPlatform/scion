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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

func TestStore_Load(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	creds := &BrokerCredentials{
		BrokerID:     "test-host-id",
		SecretKey:    base64.StdEncoding.EncodeToString([]byte("test-secret-key")),
		HubEndpoint:  "http://localhost:8080",
		RegisteredAt: time.Now().Truncate(time.Second),
	}

	writeStoreFixture(t, store, creds)

	// Load
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
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
}

func TestStore_Exists(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	// Should not exist initially
	if store.Exists() {
		t.Error("Expected Exists to return false before creating file")
	}

	// Create file
	creds := &BrokerCredentials{
		BrokerID:  "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	writeStoreFixture(t, store, creds)

	// Should exist now
	if !store.Exists() {
		t.Error("Expected Exists to return true after creating file")
	}
}

func TestStore_Delete(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	// Create file
	creds := &BrokerCredentials{
		BrokerID:  "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	writeStoreFixture(t, store, creds)

	// Delete
	err := store.Delete()
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should not exist
	if store.Exists() {
		t.Error("Expected file to be deleted")
	}

	// Delete again should not error
	err = store.Delete()
	if err != nil {
		t.Fatalf("Delete of non-existent file failed: %v", err)
	}
}

func TestStore_LoadNotFound(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "nonexistent.json")
	store := NewStore(credPath)

	_, err := store.Load()
	if err != ErrNotFound {
		t.Errorf("Expected ErrNotFound, got %v", err)
	}
}

func TestStore_GetSecretKey(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	originalKey := []byte("test-secret-key-32bytes!12345678")
	creds := &BrokerCredentials{
		BrokerID:  "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString(originalKey),
	}
	writeStoreFixture(t, store, creds)

	// Get secret key
	key, err := store.GetSecretKey()
	if err != nil {
		t.Fatalf("GetSecretKey failed: %v", err)
	}

	if string(key) != string(originalKey) {
		t.Errorf("Secret key mismatch: expected %q, got %q", originalKey, key)
	}
}

func TestStore_LoadValidation(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")

	tests := []struct {
		name      string
		content   string
		expectErr bool
	}{
		{
			name:      "invalid JSON",
			content:   "not json",
			expectErr: true,
		},
		{
			name:      "missing brokerId",
			content:   `{"secretKey": "abc"}`,
			expectErr: true,
		},
		{
			name:      "missing secretKey",
			content:   `{"brokerId": "test"}`,
			expectErr: true,
		},
		{
			name:      "valid",
			content:   `{"brokerId": "test", "secretKey": "abc"}`,
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Write the content directly, as a private file (mode 0600; on
			// Windows an owner-only ACL) so only the content is under test.
			writePrivateFixture(t, credPath, []byte(tc.content))

			store := NewStore(credPath)
			_, err := store.Load()
			if tc.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestStore_Path(t *testing.T) {
	store := NewStore("/custom/path/creds.json")
	if store.Path() != "/custom/path/creds.json" {
		t.Errorf("Expected path '/custom/path/creds.json', got %q", store.Path())
	}
}

func TestDefaultPath(t *testing.T) {
	path := DefaultPath()
	if path == "" {
		t.Error("DefaultPath should not be empty")
	}
	if !filepath.IsAbs(path) && path != DefaultFileName {
		t.Errorf("DefaultPath should be absolute or fallback, got %q", path)
	}
}

func TestStore_TransportFieldsRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	creds := &BrokerCredentials{
		BrokerID:          "broker-iap",
		SecretKey:         base64.StdEncoding.EncodeToString([]byte("secret")),
		HubEndpoint:       "https://hub.example.com",
		TransportMode:     "iap",
		TransportAudience: "12345.apps.googleusercontent.com",
	}

	writeStoreFixture(t, store, creds)

	loaded, err := store.Load()
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

func TestStore_TransportFieldsBackwardCompatibility(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")

	// Write old-format JSON without transport fields
	oldJSON := `{"brokerId": "old-broker", "secretKey": "c2VjcmV0", "hubEndpoint": "https://hub.example.com"}`
	if err := os.WriteFile(credPath, []byte(oldJSON), 0600); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	store := NewStore(credPath)
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.TransportMode != "" {
		t.Errorf("Expected empty TransportMode for old creds, got %q", loaded.TransportMode)
	}
	if loaded.TransportAudience != "" {
		t.Errorf("Expected empty TransportAudience for old creds, got %q", loaded.TransportAudience)
	}
	if loaded.BrokerID != "old-broker" {
		t.Errorf("BrokerID: expected %q, got %q", "old-broker", loaded.BrokerID)
	}
}

func TestStore_ModTime(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	// ModTime should return zero for non-existent file
	modTime := store.ModTime()
	if !modTime.IsZero() {
		t.Errorf("Expected zero time for non-existent file, got %v", modTime)
	}

	// Create the file
	creds := &BrokerCredentials{
		BrokerID:  "test-host",
		SecretKey: "dGVzdC1zZWNyZXQ=",
	}
	writeStoreFixture(t, store, creds)

	// ModTime should now return a non-zero time
	modTime = store.ModTime()
	if modTime.IsZero() {
		t.Error("Expected non-zero time for existing file")
	}

	// ModTime should be recent (within last minute)
	if time.Since(modTime) > time.Minute {
		t.Errorf("ModTime %v is not recent", modTime)
	}
}

func TestStore_LoadIfChanged(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	// LoadIfChanged on non-existent file should return nil
	creds, _, err := store.LoadIfChanged(time.Time{})
	if err != nil {
		t.Fatalf("LoadIfChanged failed for non-existent file: %v", err)
	}
	if creds != nil {
		t.Error("Expected nil credentials for non-existent file")
	}

	// Create initial credentials
	initialCreds := &BrokerCredentials{
		BrokerID:  "host-v1",
		SecretKey: "c2VjcmV0LXYx",
	}
	writeStoreFixture(t, store, initialCreds)

	// First load with zero time should return credentials
	creds, modTime, err := store.LoadIfChanged(time.Time{})
	if err != nil {
		t.Fatalf("LoadIfChanged failed: %v", err)
	}
	if creds == nil {
		t.Fatal("Expected credentials on first load")
	}
	if creds.BrokerID != "host-v1" {
		t.Errorf("Expected host-v1, got %s", creds.BrokerID)
	}
	if modTime.IsZero() {
		t.Error("Expected non-zero mod time")
	}

	// LoadIfChanged with same mod time should return nil (no change)
	creds, _, err = store.LoadIfChanged(modTime)
	if err != nil {
		t.Fatalf("LoadIfChanged failed: %v", err)
	}
	if creds != nil {
		t.Error("Expected nil credentials when unchanged")
	}

	// Update the file
	time.Sleep(10 * time.Millisecond) // Ensure different mod time
	updatedCreds := &BrokerCredentials{
		BrokerID:  "host-v2",
		SecretKey: "c2VjcmV0LXYy",
	}
	writeStoreFixture(t, store, updatedCreds)

	// LoadIfChanged with old mod time should return new credentials
	creds, newModTime, err := store.LoadIfChanged(modTime)
	if err != nil {
		t.Fatalf("LoadIfChanged failed: %v", err)
	}
	if creds == nil {
		t.Fatal("Expected credentials after file update")
	}
	if creds.BrokerID != "host-v2" {
		t.Errorf("Expected host-v2, got %s", creds.BrokerID)
	}
	if !newModTime.After(modTime) {
		t.Error("Expected new mod time to be after old mod time")
	}
}

// writeStoreFixture writes creds as the legacy credentials file of store, the
// way older releases wrote it (the store itself only reads it now).
func writeStoreFixture(t *testing.T, store *Store, creds *BrokerCredentials) {
	t.Helper()
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writePrivateFixture(t, store.Path(), data)
}

// writePrivateFixture writes data to path as the broker writes its
// credentials: a private file (mode 0600; on Windows an owner-only ACL) in
// a directory created private.
func writePrivateFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	if err := tree.WriteFile(filepath.Base(path), data, privatefs.ForMode(FileMode)); err != nil {
		t.Fatal(err)
	}
}
