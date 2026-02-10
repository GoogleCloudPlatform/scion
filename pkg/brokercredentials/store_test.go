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
)

func TestStore_SaveLoad(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	creds := &BrokerCredentials{
		BrokerID:       "test-host-id",
		SecretKey:    base64.StdEncoding.EncodeToString([]byte("test-secret-key")),
		HubEndpoint:  "http://localhost:8080",
		RegisteredAt: time.Now().Truncate(time.Second),
	}

	// Save
	err := store.Save(creds)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

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

func TestStore_FilePermissions(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	creds := &BrokerCredentials{
		BrokerID:    "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("secret")),
	}

	err := store.Save(creds)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Check file permissions
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != FileMode {
		t.Errorf("Expected file permissions %o, got %o", FileMode, perm)
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
		BrokerID:    "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	_ = store.Save(creds)

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
		BrokerID:    "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("secret")),
	}
	_ = store.Save(creds)

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
		BrokerID:    "test-host-id",
		SecretKey: base64.StdEncoding.EncodeToString(originalKey),
	}
	_ = store.Save(creds)

	// Get secret key
	key, err := store.GetSecretKey()
	if err != nil {
		t.Fatalf("GetSecretKey failed: %v", err)
	}

	if string(key) != string(originalKey) {
		t.Errorf("Secret key mismatch: expected %q, got %q", originalKey, key)
	}
}

func TestStore_SaveValidation(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

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
			name: "missing broker ID",
			creds: &BrokerCredentials{
				BrokerID:    "",
				SecretKey: "abc",
			},
			expectErr: true,
		},
		{
			name: "missing secret key",
			creds: &BrokerCredentials{
				BrokerID:    "host-id",
				SecretKey: "",
			},
			expectErr: true,
		},
		{
			name: "valid credentials",
			creds: &BrokerCredentials{
				BrokerID:    "host-id",
				SecretKey: "secret",
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
			// Write invalid content directly
			err := os.WriteFile(credPath, []byte(tc.content), 0600)
			if err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}

			store := NewStore(credPath)
			_, err = store.Load()
			if tc.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestStore_SaveFromJoinResponse(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	err := store.SaveFromJoinResponse("host-123", "c2VjcmV0", "http://hub.example.com")
	if err != nil {
		t.Fatalf("SaveFromJoinResponse failed: %v", err)
	}

	// Verify saved data
	creds, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if creds.BrokerID != "host-123" {
		t.Errorf("BrokerID mismatch: got %q", creds.BrokerID)
	}
	if creds.SecretKey != "c2VjcmV0" {
		t.Errorf("SecretKey mismatch: got %q", creds.SecretKey)
	}
	if creds.HubEndpoint != "http://hub.example.com" {
		t.Errorf("HubEndpoint mismatch: got %q", creds.HubEndpoint)
	}
	if creds.RegisteredAt.IsZero() {
		t.Error("RegisteredAt should be set")
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

func TestStore_JSONFormat(t *testing.T) {
	tempDir := t.TempDir()
	credPath := filepath.Join(tempDir, "broker-credentials.json")
	store := NewStore(credPath)

	creds := &BrokerCredentials{
		BrokerID:       "test-host-id",
		SecretKey:    "dGVzdC1zZWNyZXQ=",
		HubEndpoint:  "http://localhost:8080",
		RegisteredAt: time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC),
	}
	_ = store.Save(creds)

	// Read raw file and verify JSON structure
	data, _ := os.ReadFile(credPath)
	var parsed map[string]interface{}
	err := json.Unmarshal(data, &parsed)
	if err != nil {
		t.Fatalf("Failed to parse saved JSON: %v", err)
	}

	if parsed["brokerId"] != "test-host-id" {
		t.Errorf("brokerId mismatch in JSON: %v", parsed["brokerId"])
	}
	if parsed["secretKey"] != "dGVzdC1zZWNyZXQ=" {
		t.Errorf("secretKey mismatch in JSON: %v", parsed["secretKey"])
	}
	if parsed["hubEndpoint"] != "http://localhost:8080" {
		t.Errorf("hubEndpoint mismatch in JSON: %v", parsed["hubEndpoint"])
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
		BrokerID:    "test-host",
		SecretKey: "dGVzdC1zZWNyZXQ=",
	}
	if err := store.Save(creds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

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
	creds, modTime, err := store.LoadIfChanged(time.Time{})
	if err != nil {
		t.Fatalf("LoadIfChanged failed for non-existent file: %v", err)
	}
	if creds != nil {
		t.Error("Expected nil credentials for non-existent file")
	}

	// Create initial credentials
	initialCreds := &BrokerCredentials{
		BrokerID:    "host-v1",
		SecretKey: "c2VjcmV0LXYx",
	}
	if err := store.Save(initialCreds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// First load with zero time should return credentials
	creds, modTime, err = store.LoadIfChanged(time.Time{})
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
		BrokerID:    "host-v2",
		SecretKey: "c2VjcmV0LXYy",
	}
	if err := store.Save(updatedCreds); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

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
