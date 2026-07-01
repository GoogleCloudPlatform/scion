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

package hub

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// ResourceSource provides the metadata and files for a resource to bootstrap
// into the Hub's storage backend and database.
type ResourceSource interface {
	Kind() storage.ResourceKind
	Name() string
	Scope() (scope string, scopeID string)
	SourceURL() string
	Files(ctx context.Context) ([]transfer.FileInfo, error)
	Metadata(ctx context.Context) (ResourceMetadata, error)
}

// ResourceMetadata holds the identity fields of a resource source.
type ResourceMetadata struct {
	Kind      storage.ResourceKind
	Name      string
	Scope     string
	ScopeID   string
	SourceURL string
	Harness   string
}

// BootstrapOptions controls how BootstrapSource creates or updates resources.
type BootstrapOptions struct {
	Force           bool
	RepairStorage   bool
	AdoptExisting   bool
	OverwritePolicy OverwritePolicy
}

// OverwritePolicy determines which existing resources BootstrapSource may overwrite.
type OverwritePolicy int

const (
	OverwriteBuiltinManaged OverwritePolicy = iota // only overwrite built-in-managed resources
	OverwriteAlways                                 // admin force
	OverwriteNever                                  // read-only
)

// BootstrapResult reports what BootstrapSource did for a single resource.
type BootstrapResult struct {
	Created  int
	Updated  int
	Repaired int
	Skipped  int
	Failed   int
}

// IsBuiltinManaged returns true if sourceURL identifies a resource managed by
// the built-in bundled catalog.
func IsBuiltinManaged(sourceURL string) bool {
	return strings.HasPrefix(sourceURL, "builtin://scion/")
}

// FSResourceSource implements ResourceSource for a bundled BundledResource.
// Files are read from the embedded fs.FS and staged to a temporary directory
// when BootstrapSource needs to upload them.
type FSResourceSource struct {
	resource resources.BundledResource
}

// NewFSResourceSource returns a ResourceSource backed by a BundledResource.
func NewFSResourceSource(r resources.BundledResource) *FSResourceSource {
	return &FSResourceSource{resource: r}
}

func (s *FSResourceSource) Kind() storage.ResourceKind { return s.resource.Kind }
func (s *FSResourceSource) Name() string               { return s.resource.Name }

func (s *FSResourceSource) Scope() (string, string) {
	return s.resource.Scope, s.resource.ScopeID
}

func (s *FSResourceSource) SourceURL() string { return s.resource.SourceURL }

// Files walks the embedded fs.FS rooted at Root and returns file metadata with
// computed content hashes. The returned FileInfo entries have empty FullPath
// since they are not backed by the local filesystem.
func (s *FSResourceSource) Files(ctx context.Context) ([]transfer.FileInfo, error) {
	var files []transfer.FileInfo
	fsys := s.resource.FS
	root := s.resource.Root

	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		relPath := path
		if root != "." && root != "" {
			relPath = strings.TrimPrefix(path, root+"/")
		}

		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}

		hash := sha256.Sum256(data)
		files = append(files, transfer.FileInfo{
			Path: relPath,
			Size: int64(len(data)),
			Hash: fmt.Sprintf("sha256:%x", hash),
			Mode: "0644",
		})
		return nil
	})

	return files, err
}

// Metadata returns the identity fields of the bundled resource.
func (s *FSResourceSource) Metadata(ctx context.Context) (ResourceMetadata, error) {
	r := s.resource
	return ResourceMetadata{
		Kind:      r.Kind,
		Name:      r.Name,
		Scope:     r.Scope,
		ScopeID:   r.ScopeID,
		SourceURL: r.SourceURL,
	}, nil
}

// stageResourceSource writes a ResourceSource's files to a temporary directory
// so they can be uploaded by the existing uploadResourceFiles helper. The caller
// must invoke the returned cleanup function when done.
func stageResourceSource(src ResourceSource) (dir string, cleanup func(), err error) {
	fsSrc, ok := src.(*FSResourceSource)
	if !ok {
		return "", nil, fmt.Errorf("unsupported resource source type %T", src)
	}

	dir, err = os.MkdirTemp("", "scion-bootstrap-*")
	if err != nil {
		return "", nil, err
	}

	fsys := fsSrc.resource.FS
	root := fsSrc.resource.Root

	walkErr := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath := path
		if root != "." && root != "" {
			relPath = strings.TrimPrefix(path, root+"/")
		}
		if relPath == "" || relPath == root {
			return nil
		}

		target := filepath.Join(dir, relPath)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}

		if mkErr := os.MkdirAll(filepath.Dir(target), 0755); mkErr != nil {
			return mkErr
		}
		return os.WriteFile(target, data, 0644)
	})

	if walkErr != nil {
		os.RemoveAll(dir)
		return "", nil, walkErr
	}

	cleanup = func() { os.RemoveAll(dir) }
	return dir, cleanup, nil
}
