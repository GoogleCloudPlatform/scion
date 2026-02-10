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
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultExcludePatterns are patterns commonly excluded from file transfers.
var DefaultExcludePatterns = []string{
	".git",
	".git/**",
	".DS_Store",
	"**/.DS_Store",
}

// ManifestBuilder builds a manifest from local files.
type ManifestBuilder struct {
	// BasePath is the root directory to scan.
	BasePath string

	// ExcludePatterns are glob patterns to exclude.
	// Supports simple globs and ** patterns.
	ExcludePatterns []string
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
		ContentHash: ComputeContentHash(files),
		Files:       files,
	}, nil
}

// CollectFiles walks the directory and collects file information.
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

		// Check exclude patterns
		if b.shouldExclude(relPath, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip directories (only collect files)
		if d.IsDir() {
			return nil
		}

		// Get file info
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info for %s: %w", relPath, err)
		}

		// Compute hash
		hash, err := HashFile(path)
		if err != nil {
			return fmt.Errorf("failed to hash file %s: %w", relPath, err)
		}

		// Get file mode
		mode := fmt.Sprintf("%04o", info.Mode().Perm())

		files = append(files, FileInfo{
			Path:     relPath,
			FullPath: path,
			Size:     info.Size(),
			Hash:     hash,
			Mode:     mode,
		})

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
		ContentHash: ComputeContentHash(files),
		Files:       files,
	}
}
