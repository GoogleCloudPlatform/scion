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

package hubclient

import (
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// ManifestBuilder builds a template manifest from local files.
// This type wraps transfer.ManifestBuilder for backward compatibility.
type ManifestBuilder struct {
	// BasePath is the root directory of the template.
	BasePath string
	// IgnorePatterns are glob patterns to ignore.
	IgnorePatterns []string
}

// NewManifestBuilder creates a new manifest builder.
func NewManifestBuilder(basePath string) *ManifestBuilder {
	return &ManifestBuilder{
		BasePath: basePath,
		// Copy the shared defaults so callers appending to IgnorePatterns
		// cannot modify the shared slice.
		IgnorePatterns: append([]string{}, transfer.DefaultExcludePatterns...),
	}
}

// Build walks the template directory and builds a manifest using the current options.
func (b *ManifestBuilder) Build() (*TemplateManifest, error) {
	builder := transfer.NewManifestBuilder(b.BasePath)
	builder.ExcludePatterns = b.IgnorePatterns
	manifest, err := builder.Build()
	if err != nil {
		return nil, err
	}

	// Convert transfer.FileInfo to TemplateFile
	files := make([]TemplateFile, len(manifest.Files))
	for i, f := range manifest.Files {
		files[i] = TemplateFile{
			Path: f.Path,
			Size: f.Size,
			Hash: f.Hash,
			Mode: f.Mode,
		}
	}

	return &TemplateManifest{
		Version: manifest.Version,
		Files:   files,
	}, nil
}

// ComputeContentHash computes the v2 content hash of a template or
// harness-config with these files: transfer.ComputeContentHashV2 over
// paths, normalised modes and file hashes (ptone/scion#5658), the hash a Hub
// records with the hub.content_hash_v2 experiment on. While it is off, and
// on a Hub before ptone/scion#5658, the Hub records
// transfer.ComputeContentHashV1, and a row keeps the version of its last
// commit, so do not compare this with a Hub ContentHash to decide whether
// local files are in sync; compare manifests instead.
func ComputeContentHash(files []TemplateFile) string {
	// Convert TemplateFile to transfer.FileInfo
	transferFiles := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		transferFiles[i] = transfer.FileInfo{
			Path: f.Path,
			Size: f.Size,
			Hash: f.Hash,
			Mode: f.Mode,
		}
	}
	return transfer.ComputeContentHashV2(transferFiles)
}

// FileInfo contains information about a local file for upload.
// This is an alias for transfer.FileInfo for backward compatibility.
type FileInfo = transfer.FileInfo

// CollectFiles collects file information from a directory for upload.
// Delegates to transfer.CollectFiles.
func CollectFiles(basePath string, ignorePatterns []string) ([]FileInfo, error) {
	return transfer.CollectFiles(basePath, ignorePatterns)
}
