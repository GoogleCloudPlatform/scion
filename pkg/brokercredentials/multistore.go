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
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMultiDirName is the directory name for multi-hub credentials.
	DefaultMultiDirName = "hub-credentials"
	// MaxNameLength is the maximum length of a hub connection name.
	MaxNameLength = 63
)

var (
	// ErrDevAuthLimit is returned when attempting to save a second dev-auth connection.
	ErrDevAuthLimit = fmt.Errorf("only one dev-auth hub connection is allowed")
	// ErrInvalidName is returned when a hub connection name is invalid.
	ErrInvalidName = fmt.Errorf("invalid hub connection name")

	// validNamePattern matches filesystem-safe names: lowercase alphanumeric and hyphens,
	// must start and end with alphanumeric.
	validNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	// nonAlphanumPattern matches non-alphanumeric characters for slugification.
	nonAlphanumPattern = regexp.MustCompile(`[^a-z0-9]+`)
)

// MultiStore manages multiple hub credentials in a directory-based store.
// Each hub connection is stored as a separate JSON file named <name>.json.
type MultiStore struct {
	dir string
	mu  sync.RWMutex
}

// NewMultiStore creates a new multi-credential store at the given directory.
// If dir is empty, DefaultMultiDir() is used.
func NewMultiStore(dir string) *MultiStore {
	if dir == "" {
		dir = DefaultMultiDir()
	}
	return &MultiStore{dir: dir}
}

// DefaultMultiDir returns the default directory for multi-hub credentials.
// This is ~/.scion/hub-credentials/
func DefaultMultiDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return DefaultMultiDirName
	}
	return filepath.Join(home, ".scion", DefaultMultiDirName)
}

// Dir returns the directory path of the store.
func (s *MultiStore) Dir() string {
	return s.dir
}

// Save writes credentials to a named file in the store directory.
// It enforces the dev-auth limit: only one dev-auth connection is allowed at a time.
func (s *MultiStore) Save(creds *BrokerCredentials) error {
	if creds == nil {
		return fmt.Errorf("credentials cannot be nil")
	}
	if creds.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidName)
	}
	if err := ValidateName(creds.Name); err != nil {
		return err
	}
	if creds.BrokerID == "" {
		return fmt.Errorf("brokerId is required")
	}
	if err := ValidateBrokerIDPathElement(creds.BrokerID); err != nil {
		return err
	}
	if creds.SecretKey == "" {
		return fmt.Errorf("secretKey is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Enforce dev-auth limit: only one dev-auth connection allowed
	if creds.AuthMode == AuthModeDevAuth {
		if err := s.checkDevAuthLimit(creds.Name); err != nil {
			return err
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(s.dir, DirMode); err != nil {
		return fmt.Errorf("failed to create credentials directory: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	path := s.filePath(creds.Name)
	if err := os.WriteFile(path, data, FileMode); err != nil {
		return fmt.Errorf("failed to write credentials file: %w", err)
	}

	return nil
}

// Load reads credentials for a named hub connection.
func (s *MultiStore) Load(name string) (*BrokerCredentials, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.loadUnlocked(name)
}

// loadUnlocked reads credentials without acquiring the lock.
func (s *MultiStore) loadUnlocked(name string) (*BrokerCredentials, error) {
	path := s.filePath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds BrokerCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}

	// Ensure the Name field is populated (for backward compat with files that lack it)
	if creds.Name == "" {
		creds.Name = name
	}

	return &creds, nil
}

// Delete removes a named hub connection's credentials.
func (s *MultiStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.filePath(name)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete credentials file: %w", err)
	}
	return nil
}

// Exists checks if credentials exist for a named hub connection.
func (s *MultiStore) Exists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, err := os.Stat(s.filePath(name))
	return err == nil
}

// List returns all hub credentials stored in the directory.
func (s *MultiStore) List() ([]BrokerCredentials, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.listUnlocked()
}

// listUnlocked lists all credentials without acquiring the lock.
func (s *MultiStore) listUnlocked() ([]BrokerCredentials, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read credentials directory: %w", err)
	}

	var results []BrokerCredentials
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		creds, err := s.loadUnlocked(name)
		if err != nil {
			continue // Skip invalid files
		}
		results = append(results, *creds)
	}

	return results, nil
}

// LoadAllIfChanged loads all credentials if the directory has been modified since lastScan.
// Returns the credentials, the new scan time, whether anything changed, and any error.
func (s *MultiStore) LoadAllIfChanged(lastScan time.Time) ([]BrokerCredentials, time.Time, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()

	// Check directory mod time
	dirInfo, err := os.Stat(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, now, false, nil
		}
		return nil, lastScan, false, fmt.Errorf("failed to stat credentials directory: %w", err)
	}

	// Also check individual file mod times since directory mod time
	// only changes when files are added/removed, not when contents change.
	changed := dirInfo.ModTime().After(lastScan)
	if !changed {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return nil, lastScan, false, fmt.Errorf("failed to read credentials directory: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(lastScan) {
				changed = true
				break
			}
		}
	}

	if !changed {
		return nil, lastScan, false, nil
	}

	creds, err := s.listUnlocked()
	if err != nil {
		return nil, lastScan, false, err
	}

	return creds, now, true, nil
}

// MigrateFromLegacy reads the legacy single-file broker credentials and migrates
// them to the multi-store directory. The legacy file is renamed to .bak after migration.
func (s *MultiStore) MigrateFromLegacy(legacyPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No legacy file, nothing to migrate
		}
		return fmt.Errorf("failed to read legacy credentials: %w", err)
	}

	var creds BrokerCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("failed to parse legacy credentials: %w", err)
	}

	// Derive a name from the hub endpoint
	name := DeriveHubName(creds.HubEndpoint)
	if name == "" {
		name = "default"
	}
	creds.Name = name

	// Default to HMAC auth mode for legacy credentials
	if creds.AuthMode == "" {
		creds.AuthMode = AuthModeHMAC
	}

	// Ensure directory exists
	if err := os.MkdirAll(s.dir, DirMode); err != nil {
		return fmt.Errorf("failed to create credentials directory: %w", err)
	}

	// Save to new location
	newData, err := json.MarshalIndent(&creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	path := s.filePath(name)
	if err := os.WriteFile(path, newData, FileMode); err != nil {
		return fmt.Errorf("failed to write migrated credentials: %w", err)
	}

	// Rename legacy file to .bak
	bakPath := legacyPath + ".bak"
	if err := os.Rename(legacyPath, bakPath); err != nil {
		// Non-fatal: the credentials are already migrated
		return nil
	}

	return nil
}

// filePath returns the full path for a named credential file.
func (s *MultiStore) filePath(name string) string {
	return filepath.Join(s.dir, name+".json")
}

// checkDevAuthLimit checks if saving a dev-auth connection would violate the limit.
// Must be called with the lock held.
func (s *MultiStore) checkDevAuthLimit(name string) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No directory yet, no limit to check
		}
		return fmt.Errorf("failed to read credentials directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		existingName := strings.TrimSuffix(entry.Name(), ".json")
		if existingName == name {
			continue // Updating the same file is OK
		}

		creds, err := s.loadUnlocked(existingName)
		if err != nil {
			continue
		}
		if creds.AuthMode == AuthModeDevAuth {
			return ErrDevAuthLimit
		}
	}

	return nil
}

// DeriveHubName derives a filesystem-safe name from a hub endpoint URL.
// Examples:
//   - https://hub.scion.dev → hub-scion-dev
//   - http://localhost:8080 → localhost
//   - http://localhost:9090 → localhost-9090
func DeriveHubName(endpoint string) string {
	if endpoint == "" {
		return ""
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		// Fallback: slugify the raw endpoint
		return slugifyString(endpoint)
	}

	host := u.Hostname()
	port := u.Port()

	// Strip default ports
	if port == "80" || port == "443" {
		port = ""
	}

	// For localhost, only include port if non-default and not 8080
	// (8080 is commonly used as the default dev port)
	result := host
	if port != "" && port != "8080" {
		result = host + "-" + port
	}

	return slugifyString(result)
}

// ValidateName checks if a name is valid for use as a hub connection name.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidName)
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("%w: name cannot exceed %d characters", ErrInvalidName, MaxNameLength)
	}
	if !validNamePattern.MatchString(name) {
		return fmt.Errorf("%w: name must contain only lowercase alphanumeric characters and hyphens, and must start and end with an alphanumeric character", ErrInvalidName)
	}
	return nil
}

// slugifyString converts a string to a filesystem-safe slug.
func slugifyString(s string) string {
	s = strings.ToLower(s)
	// Replace non-alphanumeric runs with hyphens
	s = nonAlphanumPattern.ReplaceAllString(s, "-")
	// Trim leading/trailing hyphens
	s = strings.Trim(s, "-")
	if s == "" {
		return "default"
	}
	return s
}
