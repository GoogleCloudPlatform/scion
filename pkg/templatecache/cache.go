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

// Package templatecache provides a content-addressable local cache for templates.
// Runtime Brokers use this cache to store templates fetched from the Hub's cloud storage,
// enabling efficient agent creation without repeated downloads.
package templatecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultMaxSize is the default maximum cache size in bytes (100MB).
	DefaultMaxSize = 100 * 1024 * 1024

	// indexFileName is the name of the cache index file.
	indexFileName = "index.json"

	// manifestFileName is the name of the template manifest file within each cached template.
	manifestFileName = "manifest.json"
)

// Cache provides content-addressable storage for templates.
// It stores templates by their content hash, enabling cache hits
// even when the same template is referenced by different IDs.
type Cache struct {
	basePath string
	maxSize  int64
	index    *CacheIndex
	mu       sync.RWMutex
}

// CacheIndex tracks all cached templates and their metadata.
type CacheIndex struct {
	// Entries maps template ID to cache entry metadata.
	// This allows lookup by template ID while storing by content hash.
	Entries map[string]*CacheEntry `json:"entries"`

	// TotalSize is the current total size of all cached templates.
	TotalSize int64 `json:"totalSize"`

	// MaxSize is the maximum allowed cache size in bytes.
	MaxSize int64 `json:"maxSize"`
}

// CacheEntry contains metadata for a cached template.
type CacheEntry struct {
	// ContentHash is the content hash of the template (used for storage path).
	ContentHash string `json:"contentHash"`

	// LastUsed is the last time this template was accessed.
	LastUsed time.Time `json:"lastUsed"`

	// Size is the total size of this template's files in bytes.
	Size int64 `json:"size"`
}

// New creates a new template cache at the specified base path.
// If maxSize is 0, DefaultMaxSize is used.
func New(basePath string, maxSize int64) (*Cache, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}

	// Ensure base directory exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	c := &Cache{
		basePath: basePath,
		maxSize:  maxSize,
		index: &CacheIndex{
			Entries:   make(map[string]*CacheEntry),
			TotalSize: 0,
			MaxSize:   maxSize,
		},
	}

	// Load existing index if present
	if err := c.loadIndex(); err != nil {
		// If index doesn't exist or is corrupt, start fresh
		c.index = &CacheIndex{
			Entries:   make(map[string]*CacheEntry),
			TotalSize: 0,
			MaxSize:   maxSize,
		}
	}

	return c, nil
}

// Get retrieves a cached template by ID and content hash.
// Returns the path to the cached template directory and true if found and hash matches.
// Returns empty string and false if not found or hash doesn't match.
func (c *Cache) Get(templateID string, contentHash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.index.Entries[templateID]
	if !ok {
		return "", false
	}

	// Verify content hash matches
	if entry.ContentHash != contentHash {
		return "", false
	}

	// Build path and verify it exists
	templatePath := filepath.Join(c.basePath, contentHash)
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		// Cache entry exists but files are missing, clean up
		delete(c.index.Entries, templateID)
		c.index.TotalSize -= entry.Size
		_ = c.saveIndex()
		return "", false
	}

	// Update last used time
	entry.LastUsed = time.Now()
	_ = c.saveIndex()

	return templatePath, true
}

// GetByHash retrieves a cached template by content hash alone.
// This is useful when we only have the hash (e.g., from template metadata).
func (c *Cache) GetByHash(contentHash string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	templatePath := filepath.Join(c.basePath, contentHash)
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return "", false
	}

	return templatePath, true
}

// GetAnyVersion retrieves any cached version of a template by ID.
// Unlike Get, this returns the cached version even if the content hash differs.
// Returns the path, the cached content hash, and true if any version is cached.
func (c *Cache) GetAnyVersion(templateID string) (string, string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.index.Entries[templateID]
	if !ok {
		return "", "", false
	}

	// Build path and verify it exists
	templatePath := filepath.Join(c.basePath, entry.ContentHash)
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return "", "", false
	}

	return templatePath, entry.ContentHash, true
}

// GetFileHashes returns a map of file paths to their hashes for a cached template.
// This reads the actual files from the cache directory and computes their hashes.
func (c *Cache) GetFileHashes(templatePath string) (map[string]string, error) {
	hashes := make(map[string]string)

	err := filepath.Walk(templatePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(templatePath, path)
		if err != nil {
			return err
		}

		// Read file and compute hash
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		hash := sha256.Sum256(content)
		hashes[relPath] = "sha256:" + hex.EncodeToString(hash[:])
		return nil
	})

	if err != nil {
		return nil, err
	}

	return hashes, nil
}

// Store stores template files in the cache.
// files is a map of relative file paths to their content.
// Returns the path to the stored template directory.
func (c *Cache) Store(templateID string, contentHash string, files map[string][]byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	templatePath := filepath.Join(c.basePath, contentHash)

	// Check if already stored by hash
	if _, err := os.Stat(templatePath); err == nil {
		// Already exists, just update the entry
		var totalSize int64
		for _, content := range files {
			totalSize += int64(len(content))
		}

		// Check if this templateID already has an entry
		if existing, ok := c.index.Entries[templateID]; ok {
			// Update existing entry
			existing.ContentHash = contentHash
			existing.LastUsed = time.Now()
			existing.Size = totalSize
		} else {
			// Add new entry pointing to existing hash
			c.index.Entries[templateID] = &CacheEntry{
				ContentHash: contentHash,
				LastUsed:    time.Now(),
				Size:        totalSize,
			}
			// Only add to total if this is a new hash
			// Check if any other entry uses this hash
			hashUsed := false
			for id, entry := range c.index.Entries {
				if id != templateID && entry.ContentHash == contentHash {
					hashUsed = true
					break
				}
			}
			if !hashUsed {
				c.index.TotalSize += totalSize
			}
		}

		_ = c.saveIndex()
		return templatePath, nil
	}

	// Calculate total size
	var totalSize int64
	for _, content := range files {
		totalSize += int64(len(content))
	}

	// Evict old entries if needed to make room
	if err := c.evictIfNeeded(totalSize); err != nil {
		return "", fmt.Errorf("failed to make room in cache: %w", err)
	}

	// Create template directory
	if err := os.MkdirAll(templatePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create template directory: %w", err)
	}

	// Write files
	for relativePath, content := range files {
		filePath := filepath.Join(templatePath, relativePath)

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			// Cleanup on failure
			os.RemoveAll(templatePath)
			return "", fmt.Errorf("failed to create directory for %s: %w", relativePath, err)
		}

		if err := os.WriteFile(filePath, content, 0644); err != nil {
			// Cleanup on failure
			os.RemoveAll(templatePath)
			return "", fmt.Errorf("failed to write file %s: %w", relativePath, err)
		}
	}

	// Update index
	c.index.Entries[templateID] = &CacheEntry{
		ContentHash: contentHash,
		LastUsed:    time.Now(),
		Size:        totalSize,
	}
	c.index.TotalSize += totalSize

	if err := c.saveIndex(); err != nil {
		return "", fmt.Errorf("failed to save cache index: %w", err)
	}

	return templatePath, nil
}

// evictIfNeeded evicts old entries to make room for newSize bytes.
// Must be called with lock held.
func (c *Cache) evictIfNeeded(newSize int64) error {
	// Check if eviction is needed
	if c.index.TotalSize+newSize <= c.maxSize {
		return nil
	}

	// Build list of entries sorted by last used time (oldest first)
	type entryWithID struct {
		ID    string
		Entry *CacheEntry
	}
	var entries []entryWithID
	for id, entry := range c.index.Entries {
		entries = append(entries, entryWithID{ID: id, Entry: entry})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Entry.LastUsed.Before(entries[j].Entry.LastUsed)
	})

	// Evict until we have enough space
	targetSize := c.maxSize - newSize
	for _, e := range entries {
		if c.index.TotalSize <= targetSize {
			break
		}

		// Check if other entries share this hash
		hashShared := false
		for id, entry := range c.index.Entries {
			if id != e.ID && entry.ContentHash == e.Entry.ContentHash {
				hashShared = true
				break
			}
		}

		// Remove entry
		delete(c.index.Entries, e.ID)

		// Only remove files and decrement size if hash is not shared
		if !hashShared {
			templatePath := filepath.Join(c.basePath, e.Entry.ContentHash)
			if err := os.RemoveAll(templatePath); err != nil {
				// Log but continue
				fmt.Printf("Warning: failed to remove cached template %s: %v\n", templatePath, err)
			}
			c.index.TotalSize -= e.Entry.Size
		}
	}

	return nil
}

// loadIndex loads the cache index from disk.
func (c *Cache) loadIndex() error {
	indexPath := filepath.Join(c.basePath, indexFileName)

	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No index yet, use empty
		}
		return err
	}

	var index CacheIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return err
	}

	// Ensure Entries map is initialized
	if index.Entries == nil {
		index.Entries = make(map[string]*CacheEntry)
	}

	c.index = &index
	c.index.MaxSize = c.maxSize // Use configured max size
	return nil
}

// saveIndex persists the cache index to disk.
// Must be called with lock held.
func (c *Cache) saveIndex() error {
	indexPath := filepath.Join(c.basePath, indexFileName)

	data, err := json.MarshalIndent(c.index, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(indexPath, data, 0644)
}

// Clear removes all cached templates.
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove all template directories
	entries, err := os.ReadDir(c.basePath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == indexFileName {
			path := filepath.Join(c.basePath, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to remove %s: %w", path, err)
			}
		}
	}

	// Reset index
	c.index = &CacheIndex{
		Entries:   make(map[string]*CacheEntry),
		TotalSize: 0,
		MaxSize:   c.maxSize,
	}

	return c.saveIndex()
}

// Stats returns cache statistics.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		TotalSize:     c.index.TotalSize,
		MaxSize:       c.index.MaxSize,
		EntryCount:    len(c.index.Entries),
		UsagePercent:  float64(c.index.TotalSize) / float64(c.index.MaxSize) * 100,
	}
}

// CacheStats contains cache usage statistics.
type CacheStats struct {
	TotalSize    int64
	MaxSize      int64
	EntryCount   int
	UsagePercent float64
}

// CopyToDir copies a cached template to the specified destination directory.
func (c *Cache) CopyToDir(templatePath string, destDir string) error {
	return copyDir(templatePath, destDir)
}

// copyDir recursively copies a directory.
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
