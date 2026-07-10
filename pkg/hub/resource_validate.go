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
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// ValidationReport is the result of validating a single resource's storage.
type ValidationReport struct {
	ResourceKind storage.ResourceKind `json:"resourceKind"`
	Name         string               `json:"name"`
	Scope        string               `json:"scope"`
	Status       string               `json:"status"`
	Issues       []ValidationIssue    `json:"issues"`
}

// ValidationIssue describes a single storage consistency problem.
type ValidationIssue struct {
	Kind    string `json:"kind"`
	File    string `json:"file,omitempty"`
	Message string `json:"message"`
}

// Validation issue kinds.
const (
	ValidationIssueMissingObject       = "missing_object"
	ValidationIssueMissingManifest     = "missing_manifest"
	ValidationIssueZeroFilesActive     = "zero_files_active"
	ValidationIssueContentHashMismatch = "content_hash_mismatch"
	ValidationIssueLegacyPath          = "legacy_path"
)

// ValidateStorage checks a resource record's storage consistency. It verifies:
//   - An active record has at least one file
//   - Every file listed in the DB has a corresponding storage object
//   - The manifest object exists
func (rs *ResourceStore) ValidateStorage(ctx context.Context, rec *ResourceRecord) (*ValidationReport, error) {
	report := &ValidationReport{
		ResourceKind: rec.Kind,
		Name:         rec.Name,
		Scope:        rec.Scope,
		Status:       rec.Status,
	}

	if rec.Status == resourceStatusActive && len(rec.Files) == 0 {
		report.Issues = append(report.Issues, ValidationIssue{
			Kind:    ValidationIssueZeroFilesActive,
			Message: fmt.Sprintf("resource %q is active but has zero files", rec.Name),
		})
	}

	stor := rs.srv.GetStorage()
	if stor == nil {
		return report, fmt.Errorf("storage backend is not configured")
	}

	storagePath := rec.StoragePath
	if storagePath == "" {
		storagePath = storage.ResourceStoragePath(rs.hubID, rec.Kind, rec.Scope, rec.ScopeID, rec.Slug)
	}

	var legacyBase string
	if rs.srv.LegacyFallbackEnabled() {
		legacyBase = storage.ResourceStoragePath("", rec.Kind, rec.Scope, rec.ScopeID, rec.Slug)
	}

	for _, file := range rec.Files {
		objectPath := storagePath + "/" + file.Path
		obj, err := stor.GetObject(ctx, objectPath)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				// Try legacy fallback before reporting missing.
				if legacyBase != "" && legacyBase != storagePath {
					legacyObjectPath := legacyBase + "/" + file.Path
					if legacyObj, legacyErr := stor.GetObject(ctx, legacyObjectPath); legacyErr == nil {
						report.Issues = append(report.Issues, ValidationIssue{
							Kind:    ValidationIssueLegacyPath,
							File:    file.Path,
							Message: fmt.Sprintf("file %q found at legacy path %q, expected at %q", file.Path, legacyObjectPath, objectPath),
						})
						obj = legacyObj
						goto hashCheck
					}
				}
				report.Issues = append(report.Issues, ValidationIssue{
					Kind:    ValidationIssueMissingObject,
					File:    file.Path,
					Message: fmt.Sprintf("storage object missing for file %q", file.Path),
				})
				continue
			}
			return report, fmt.Errorf("checking object %q: %w", objectPath, err)
		}
	hashCheck:

		if file.Hash == "" {
			continue
		}

		storedHash := objectMetadataHash(obj)
		if storedHash == "" {
			var hashErr error
			storedHash, hashErr = computeStoredHash(ctx, stor, objectPath)
			if hashErr != nil {
				return report, fmt.Errorf("computing hash for %q: %w", objectPath, hashErr)
			}
		}
		if storedHash != "" && storedHash != file.Hash {
			report.Issues = append(report.Issues, ValidationIssue{
				Kind:    ValidationIssueContentHashMismatch,
				File:    file.Path,
				Message: fmt.Sprintf("expected %s, got %s", file.Hash, storedHash),
			})
		}
	}

	manifestPath := storagePath + "/manifest.json"
	exists, err := stor.Exists(ctx, manifestPath)
	if err != nil {
		return report, fmt.Errorf("checking manifest: %w", err)
	}
	if !exists {
		if legacyBase != "" && legacyBase != storagePath {
			legacyManifest := legacyBase + "/manifest.json"
			legacyExists, _ := stor.Exists(ctx, legacyManifest)
			if legacyExists {
				report.Issues = append(report.Issues, ValidationIssue{
					Kind:    ValidationIssueLegacyPath,
					File:    "manifest.json",
					Message: fmt.Sprintf("manifest found at legacy path %q, expected at %q", legacyManifest, manifestPath),
				})
				exists = true
			}
		}
		if !exists {
			report.Issues = append(report.Issues, ValidationIssue{
				Kind:    ValidationIssueMissingManifest,
				Message: "manifest.json missing from storage",
			})
		}
	}

	return report, nil
}

// objectMetadataHash extracts the SHA256 hash stored in object metadata during
// upload. Returns "" if the metadata doesn't contain a hash.
func objectMetadataHash(obj *storage.Object) string {
	if obj == nil || obj.Metadata == nil {
		return ""
	}
	return obj.Metadata["sha256"]
}

func computeStoredHash(ctx context.Context, stor storage.Storage, objectPath string) (string, error) {
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		return "", err
	}
	if reader == nil {
		return "", fmt.Errorf("storage returned nil reader for %s", objectPath)
	}
	defer func() { _ = reader.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
