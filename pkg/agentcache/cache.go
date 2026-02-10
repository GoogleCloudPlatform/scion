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

// Package agentcache provides a simple cache for agent names used by shell completion.
// The cache stores agent names per grove to ensure fast completions even when the
// Hub API is slow or unavailable.
package agentcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	// cacheSubdir is the subdirectory within ~/.scion for agent name caches.
	cacheSubdir = "cache/agent-names"
)

// CacheEntry represents a cached list of agent names for a grove.
type CacheEntry struct {
	// Agents is the list of agent names.
	Agents []string `json:"agents"`
	// UpdatedAt is when this cache entry was last updated.
	UpdatedAt time.Time `json:"updatedAt"`
}

// getCacheDir returns the directory where agent name caches are stored.
func getCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".scion", cacheSubdir), nil
}

// GenerateCacheKey creates a unique, filesystem-safe key for a grove path.
func GenerateCacheKey(grovePath string) string {
	hash := sha256.Sum256([]byte(grovePath))
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes for a shorter filename
}

// getCachePath returns the full path to a cache file for a given key.
func getCachePath(cacheKey string) (string, error) {
	cacheDir, err := getCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, cacheKey+".json"), nil
}

// ReadCache reads the cached agent names for a given cache key.
// Returns nil and no error if the cache doesn't exist.
func ReadCache(cacheKey string) ([]string, error) {
	cachePath, err := getCachePath(cacheKey)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Cache miss, not an error
		}
		return nil, err
	}

	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		// Corrupted cache, treat as miss
		return nil, nil
	}

	return entry.Agents, nil
}

// WriteCache writes agent names to the cache for a given cache key.
func WriteCache(cacheKey string, agents []string) error {
	cacheDir, err := getCacheDir()
	if err != nil {
		return err
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}

	cachePath := filepath.Join(cacheDir, cacheKey+".json")

	entry := CacheEntry{
		Agents:    agents,
		UpdatedAt: time.Now(),
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(cachePath, data, 0644)
}

// ClearCache removes all agent name caches.
func ClearCache() error {
	cacheDir, err := getCacheDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(cacheDir)
}
