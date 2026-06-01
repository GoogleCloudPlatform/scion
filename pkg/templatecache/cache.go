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
// Runtime Brokers use this cache to store templates fetched from a remote Hub's
// cloud storage, avoiding repeated downloads on re-provision.
//
// The cache is a thin content-addressed store: entries are keyed solely by
// content hash, so identical content referenced by different template IDs (or
// the same template across versions that share files) is stored once. It is a
// hosted/remote-broker concern only — when the Hub's storage backend is the
// local filesystem the broker reads resources directly from disk and never
// touches this cache.
package templatecache

import (
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
)

// Cache provides content-addressable storage for templates. Templates are
// stored under a directory named for their content hash, so a cache hit only
// requires the hash.
type Cache struct {
	basePath string
	maxSize  int64
	index    *CacheIndex
	mu       sync.RWMutex
}

// CacheIndex tracks all cached templates and their metadata.
type CacheIndex struct {
	// Entries maps content hash to cache entry metadata.
	Entries map[string]*CacheEntry `json:"entries"`

	// TotalSize is the current total size of all cached templates.
	TotalSize int64 `json:"totalSize"`

	// MaxSize is the maximum allowed cache size in bytes.
	MaxSize int64 `json:"maxSize"`
}

// CacheEntry contains metadata for a cached template, keyed by content hash.
type CacheEntry struct {
	// LastUsed is the last time this entry was accessed (for LRU eviction).
	LastUsed time.Time `json:"lastUsed"`

	// Size is the total size of the entry's files in bytes.
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
		index:    newIndex(maxSize),
	}

	// Load existing index if present; start fresh on error.
	if err := c.loadIndex(); err != nil {
		c.index = newIndex(maxSize)
	}

	return c, nil
}

func newIndex(maxSize int64) *CacheIndex {
	return &CacheIndex{
		Entries:   make(map[string]*CacheEntry),
		TotalSize: 0,
		MaxSize:   maxSize,
	}
}

// Get retrieves a cached template by content hash. It returns the path to the
// cached directory and true if the content is present on disk.
func (c *Cache) Get(contentHash string) (string, bool) {
	if contentHash == "" {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.index.Entries[contentHash]
	if !ok {
		return "", false
	}

	templatePath := filepath.Join(c.basePath, contentHash)
	if _, err := os.Stat(templatePath); err != nil {
		// Files missing: drop stale index entry so accounting stays honest.
		delete(c.index.Entries, contentHash)
		c.index.TotalSize -= entry.Size
		_ = c.saveIndex()
		return "", false
	}

	entry.LastUsed = time.Now()
	_ = c.saveIndex()

	return templatePath, true
}

// Put stores template files in the cache under their content hash.
// files maps relative file paths to their content. It returns the path to the
// stored directory. If the content is already present, the existing directory
// is reused and its last-used time refreshed.
func (c *Cache) Put(contentHash string, files map[string][]byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	templatePath := filepath.Join(c.basePath, contentHash)

	var totalSize int64
	for _, content := range files {
		totalSize += int64(len(content))
	}

	// Already present: just refresh the entry.
	if _, err := os.Stat(templatePath); err == nil {
		if _, ok := c.index.Entries[contentHash]; !ok {
			c.index.Entries[contentHash] = &CacheEntry{}
			c.index.TotalSize += totalSize
		}
		c.index.Entries[contentHash].LastUsed = time.Now()
		c.index.Entries[contentHash].Size = totalSize
		_ = c.saveIndex()
		return templatePath, nil
	}

	// Evict old entries if needed to make room.
	if err := c.evictIfNeeded(totalSize); err != nil {
		return "", fmt.Errorf("failed to make room in cache: %w", err)
	}

	tmpPath := templatePath + ".tmp"
	if err := os.MkdirAll(tmpPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create template directory: %w", err)
	}

	for relativePath, content := range files {
		filePath := filepath.Join(tmpPath, relativePath)
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			_ = os.RemoveAll(tmpPath)
			return "", fmt.Errorf("failed to create directory for %s: %w", relativePath, err)
		}
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			_ = os.RemoveAll(tmpPath)
			return "", fmt.Errorf("failed to write file %s: %w", relativePath, err)
		}
	}

	if err := os.Rename(tmpPath, templatePath); err != nil {
		_ = os.RemoveAll(tmpPath)
		return "", fmt.Errorf("failed to commit cached template: %w", err)
	}

	c.index.Entries[contentHash] = &CacheEntry{
		LastUsed: time.Now(),
		Size:     totalSize,
	}
	c.index.TotalSize += totalSize

	if err := c.saveIndex(); err != nil {
		return "", fmt.Errorf("failed to save cache index: %w", err)
	}

	return templatePath, nil
}

// evictIfNeeded evicts least-recently-used entries to make room for newSize
// bytes. Must be called with the lock held. Because entries are keyed by content
// hash, each directory is owned by exactly one entry — no shared-hash refcounting.
func (c *Cache) evictIfNeeded(newSize int64) error {
	if c.index.TotalSize+newSize <= c.maxSize {
		return nil
	}

	type entryWithHash struct {
		Hash  string
		Entry *CacheEntry
	}
	entries := make([]entryWithHash, 0, len(c.index.Entries))
	for hash, entry := range c.index.Entries {
		entries = append(entries, entryWithHash{Hash: hash, Entry: entry})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Entry.LastUsed.Before(entries[j].Entry.LastUsed)
	})

	targetSize := c.maxSize - newSize
	for _, e := range entries {
		if c.index.TotalSize <= targetSize {
			break
		}
		templatePath := filepath.Join(c.basePath, e.Hash)
		if err := os.RemoveAll(templatePath); err != nil {
			fmt.Printf("Warning: failed to remove cached template %s: %v\n", templatePath, err)
		}
		delete(c.index.Entries, e.Hash)
		c.index.TotalSize -= e.Entry.Size
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

	if index.Entries == nil {
		index.Entries = make(map[string]*CacheEntry)
	}

	c.index = &index
	c.index.MaxSize = c.maxSize // Use configured max size
	return nil
}

// saveIndex persists the cache index to disk. Must be called with the lock held.
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

	c.index = newIndex(c.maxSize)
	return c.saveIndex()
}

// Stats returns cache statistics.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		TotalSize:    c.index.TotalSize,
		MaxSize:      c.index.MaxSize,
		EntryCount:   len(c.index.Entries),
		UsagePercent: float64(c.index.TotalSize) / float64(c.index.MaxSize) * 100,
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
