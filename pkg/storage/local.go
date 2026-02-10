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

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalStorage implements Storage using the local filesystem.
// This is primarily used for development and testing.
type LocalStorage struct {
	config   Config
	basePath string
}

// NewLocal creates a new local filesystem storage client.
func NewLocal(cfg Config) (*LocalStorage, error) {
	basePath := cfg.LocalPath
	if basePath == "" {
		basePath = filepath.Join(os.TempDir(), "scion-storage")
	}

	// Ensure the base path exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	// Create a subdirectory for the "bucket"
	bucketPath := filepath.Join(basePath, cfg.Bucket)
	if err := os.MkdirAll(bucketPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create bucket directory: %w", err)
	}

	return &LocalStorage{
		config:   cfg,
		basePath: bucketPath,
	}, nil
}

// Bucket returns the bucket name.
func (s *LocalStorage) Bucket() string {
	return s.config.Bucket
}

// Provider returns the storage provider type.
func (s *LocalStorage) Provider() Provider {
	return ProviderLocal
}

// fullPath returns the full filesystem path for an object.
func (s *LocalStorage) fullPath(objectPath string) string {
	objectPath = strings.TrimPrefix(objectPath, "/")
	return filepath.Join(s.basePath, filepath.FromSlash(objectPath))
}

// GenerateSignedURL creates a "signed URL" for object access.
// For local storage, this returns a file:// URL that's valid immediately.
// This is primarily for testing the signed URL flow.
func (s *LocalStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts SignedURLOptions) (*SignedURL, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)

	// For local storage, we just return a file:// URL
	// In a real scenario, this would be used for testing only
	expires := opts.Expires
	if expires == 0 {
		expires = 15 * time.Minute
	}

	return &SignedURL{
		URL:     "file://" + fullPath,
		Method:  opts.Method,
		Expires: time.Now().Add(expires),
	}, nil
}

// Upload uploads data to the specified path.
func (s *LocalStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts UploadOptions) (*Object, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Create the file
	file, err := os.Create(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Copy data and compute hash
	hash := sha256.New()
	tee := io.TeeReader(reader, hash)

	size, err := io.Copy(file, tee)
	if err != nil {
		return nil, fmt.Errorf("failed to write data: %w", err)
	}

	// Get file info
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	return &Object{
		Name:        objectPath,
		Size:        size,
		ContentType: opts.ContentType,
		ETag:        etag,
		Created:     info.ModTime(),
		Updated:     info.ModTime(),
		Metadata:    opts.Metadata,
	}, nil
}

// Download downloads data from the specified path.
func (s *LocalStorage) Download(ctx context.Context, objectPath string) (io.ReadCloser, *Object, error) {
	if objectPath == "" {
		return nil, nil, ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)

	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}

	return file, &Object{
		Name:    objectPath,
		Size:    info.Size(),
		Created: info.ModTime(),
		Updated: info.ModTime(),
	}, nil
}

// Delete deletes the object at the specified path.
func (s *LocalStorage) Delete(ctx context.Context, objectPath string) error {
	if objectPath == "" {
		return ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)

	if err := os.Remove(fullPath); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// DeletePrefix deletes all objects with the given prefix.
func (s *LocalStorage) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return ErrInvalidPath
	}

	prefixPath := s.fullPath(prefix)

	// Remove the entire directory tree
	if err := os.RemoveAll(prefixPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete prefix: %w", err)
	}

	return nil
}

// List lists objects matching the given options.
func (s *LocalStorage) List(ctx context.Context, opts ListOptions) (*ListResult, error) {
	prefixPath := s.basePath
	if opts.Prefix != "" {
		prefixPath = s.fullPath(opts.Prefix)
	}

	result := &ListResult{}
	count := 0

	err := filepath.Walk(prefixPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		// Skip the root directory
		if path == prefixPath && info.IsDir() {
			return nil
		}

		// Convert to relative path
		relPath, _ := filepath.Rel(s.basePath, path)
		relPath = filepath.ToSlash(relPath)

		// Handle delimiter (directory listing)
		if opts.Delimiter != "" && info.IsDir() {
			result.Prefixes = append(result.Prefixes, relPath+"/")
			return filepath.SkipDir
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Apply max results
		if opts.MaxResults > 0 && count >= opts.MaxResults {
			result.NextOffset = relPath
			return filepath.SkipAll
		}

		result.Objects = append(result.Objects, Object{
			Name:    relPath,
			Size:    info.Size(),
			Created: info.ModTime(),
			Updated: info.ModTime(),
		})
		count++

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to list objects: %w", err)
	}

	return result, nil
}

// Exists checks if an object exists.
func (s *LocalStorage) Exists(ctx context.Context, objectPath string) (bool, error) {
	if objectPath == "" {
		return false, ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)
	_, err := os.Stat(fullPath)

	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat file: %w", err)
	}

	return true, nil
}

// GetObject returns object metadata without downloading content.
func (s *LocalStorage) GetObject(ctx context.Context, objectPath string) (*Object, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	fullPath := s.fullPath(objectPath)
	info, err := os.Stat(fullPath)

	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	return &Object{
		Name:    objectPath,
		Size:    info.Size(),
		Created: info.ModTime(),
		Updated: info.ModTime(),
	}, nil
}

// Copy copies an object from src to dst.
func (s *LocalStorage) Copy(ctx context.Context, srcPath, dstPath string) (*Object, error) {
	if srcPath == "" || dstPath == "" {
		return nil, ErrInvalidPath
	}

	srcFullPath := s.fullPath(srcPath)
	dstFullPath := s.fullPath(dstPath)

	// Open source file
	src, err := os.Open(srcFullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dstFullPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Create destination file
	dst, err := os.Create(dstFullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dst.Close()

	// Copy content
	size, err := io.Copy(dst, src)
	if err != nil {
		return nil, fmt.Errorf("failed to copy data: %w", err)
	}

	info, _ := dst.Stat()

	return &Object{
		Name:    dstPath,
		Size:    size,
		Created: info.ModTime(),
		Updated: info.ModTime(),
	}, nil
}

// Close releases any resources held by the storage client.
func (s *LocalStorage) Close() error {
	// Nothing to close for local storage
	return nil
}

// Ensure LocalStorage implements Storage interface.
var _ Storage = (*LocalStorage)(nil)
