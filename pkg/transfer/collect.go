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

package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultExcludePatterns are patterns commonly excluded from file transfers.
//
// ".scion/**" excludes the workspace-root .scion entry (the project marker
// file, or the directory holding project identity and settings). The matcher
// anchors "/**" patterns to the root and also matches the bare prefix, so the
// one pattern covers both layouts. A bare ".scion" pattern is deliberately
// absent: simple patterns also match by basename, which would drop nested
// .scion entries that are ordinary workspace content.
var DefaultExcludePatterns = []string{
	".git",
	".git/**",
	".DS_Store",
	"**/.DS_Store",
	".scion/**",
}

// ManifestBuilder builds a manifest from local files.
type ManifestBuilder struct {
	// BasePath is the root directory to scan.
	BasePath string

	// ExcludePatterns are glob patterns to exclude.
	// Supports simple globs and ** patterns.
	ExcludePatterns []string

	// OnSkip, when set, is called with the slash-separated relative path of
	// each entry that is not collected because it is not a regular file
	// (symlinks, sockets, FIFOs, devices). Excluded paths are not reported.
	OnSkip func(relPath string)
}

// NewManifestBuilder creates a new manifest builder with default exclude patterns.
func NewManifestBuilder(basePath string) *ManifestBuilder {
	return &ManifestBuilder{
		BasePath:        basePath,
		ExcludePatterns: append([]string{}, DefaultExcludePatterns...),
	}
}

// WithExcludePatterns adds additional exclude patterns.
func (b *ManifestBuilder) WithExcludePatterns(patterns []string) *ManifestBuilder {
	if patterns != nil {
		b.ExcludePatterns = append(b.ExcludePatterns, patterns...)
	}
	return b
}

// Build walks the directory and builds a manifest.
func (b *ManifestBuilder) Build() (*Manifest, error) {
	files, err := b.CollectFiles()
	if err != nil {
		return nil, err
	}

	return &Manifest{
		Version:     "1.0",
		ContentHash: ComputeContentHashV2(files),
		Files:       files,
	}, nil
}

// CollectFiles walks the directory and collects information for regular files.
func (b *ManifestBuilder) CollectFiles() ([]FileInfo, error) {
	var files []FileInfo

	err := filepath.WalkDir(b.BasePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(b.BasePath, path)
		if err != nil {
			return err
		}

		// Use forward slashes for consistency
		relPath = filepath.ToSlash(relPath)

		// Skip root
		if relPath == "." {
			return nil
		}

		fi, err := b.visit(relPath, d, path, func() (string, int64, error) {
			h, err := HashFile(path)
			return h, -1, err
		})
		if err != nil || fi == nil {
			return err
		}
		files = append(files, *fi)

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Sort files by path for deterministic ordering
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return files, nil
}

// visit applies the collection rules to one walked entry: relPath is its
// slash-separated path relative to the base (never "."), fullPath the path
// recorded in FileInfo.FullPath, and hash computes the content hash of a
// regular file and, when it is not negative, the size of the content it
// hashed (otherwise the size comes from the walk's FileInfo). An excluded
// directory returns fs.SkipDir and an excluded file nothing. Only regular
// files are collected: opening a FIFO can block, and symlinks, sockets and
// devices are not transferable file content, so they return nothing
// (reported to OnSkip). A regular file returns its FileInfo.
func (b *ManifestBuilder) visit(relPath string, d fs.DirEntry, fullPath string, hash func() (string, int64, error)) (*FileInfo, error) {
	if b.shouldExclude(relPath, d.IsDir()) {
		if d.IsDir() {
			return nil, fs.SkipDir
		}
		return nil, nil
	}
	if !d.Type().IsRegular() {
		if b.OnSkip != nil && !d.IsDir() {
			b.OnSkip(relPath)
		}
		return nil, nil
	}
	info, err := d.Info()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info for %s: %w", relPath, err)
	}
	h, size, err := hash()
	if err != nil {
		return nil, fmt.Errorf("failed to hash file %s: %w", relPath, err)
	}
	if size < 0 {
		size = info.Size()
	}
	return &FileInfo{
		Path:     relPath,
		FullPath: fullPath,
		Size:     size,
		Hash:     h,
		Mode:     fmt.Sprintf("%04o", info.Mode().Perm()),
	}, nil
}

// CollectFilesFromRoot is CollectFiles for a directory already opened as
// root: the walk (walkRoot), each entry's stat and the hashing all go through
// root, so the directory is not reopened by path (a directory or link swapped
// in at basePath after root was opened is never walked), and a subdirectory
// replaced during the walk by a FIFO, a link or another directory fails the
// collection without blocking. basePath is the
// directory's path; it is used only for FileInfo.FullPath. The exclude and
// regular-file rules are CollectFiles'. A file is hashed through
// OpenRegularFile, so one that is no longer a regular file when it is opened
// fails the collection instead of being followed, and its recorded size is
// the number of bytes hashed from that open file.
func CollectFilesFromRoot(root *os.Root, basePath string, excludePatterns []string) ([]FileInfo, error) {
	b := NewManifestBuilder(basePath)
	if excludePatterns != nil {
		b.ExcludePatterns = append(b.ExcludePatterns, excludePatterns...)
	}
	var files []FileInfo
	err := walkRoot(root, func(relPath string, d fs.DirEntry) error {
		rel := filepath.FromSlash(relPath)
		fi, err := b.visit(relPath, d, filepath.Join(basePath, rel), func() (string, int64, error) { return hashRootFile(root, rel) })
		if err != nil || fi == nil {
			return err
		}
		files = append(files, *fi)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// hashRootFile is HashFile for rel, read through root with OpenRegularFile.
// It returns the hash and the number of bytes hashed.
func hashRootFile(root *os.Root, rel string) (string, int64, error) {
	f, _, err := OpenRegularFile(root, rel)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	hasher := sha256.New()
	n, err := io.Copy(hasher, f)
	if err != nil {
		return "", 0, err
	}
	return HashPrefix + hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// shouldExclude checks if a path should be excluded.
func (b *ManifestBuilder) shouldExclude(relPath string, isDir bool) bool {
	for _, pattern := range b.ExcludePatterns {
		// Handle ** patterns
		if strings.Contains(pattern, "**") {
			// Convert ** pattern to check
			prefix := strings.TrimSuffix(pattern, "/**")
			if strings.HasPrefix(relPath, prefix+"/") || relPath == prefix {
				return true
			}
			// Check if pattern matches directory contents
			suffix := strings.TrimPrefix(pattern, "**/")
			if suffix != pattern && strings.HasSuffix(relPath, suffix) {
				return true
			}
			continue
		}

		// Simple match
		if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
			return true
		}
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
	}
	return false
}

// CollectFiles is a convenience function that collects file information from a directory.
// It creates a ManifestBuilder with the provided exclude patterns and collects files.
func CollectFiles(basePath string, excludePatterns []string) ([]FileInfo, error) {
	builder := NewManifestBuilder(basePath)
	if excludePatterns != nil {
		builder.ExcludePatterns = append(builder.ExcludePatterns, excludePatterns...)
	}
	return builder.CollectFiles()
}

// BuildManifest creates a manifest from a list of files.
func BuildManifest(files []FileInfo) *Manifest {
	return &Manifest{
		Version:     "1.0",
		ContentHash: ComputeContentHashV2(files),
		Files:       files,
	}
}
