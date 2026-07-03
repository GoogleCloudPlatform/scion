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
	"fmt"

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
	ValidationIssueMissingObject   = "missing_object"
	ValidationIssueMissingManifest = "missing_manifest"
	ValidationIssueZeroFilesActive = "zero_files_active"
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
		storagePath = storage.ResourceStoragePath(rec.Kind, rec.Scope, rec.ScopeID, rec.Slug)
	}

	for _, file := range rec.Files {
		objectPath := storagePath + "/" + file.Path
		exists, err := stor.Exists(ctx, objectPath)
		if err != nil {
			return report, fmt.Errorf("checking object %q: %w", objectPath, err)
		}
		if !exists {
			report.Issues = append(report.Issues, ValidationIssue{
				Kind:    ValidationIssueMissingObject,
				File:    file.Path,
				Message: fmt.Sprintf("storage object missing for file %q", file.Path),
			})
		}
	}

	manifestPath := storagePath + "/manifest.json"
	exists, err := stor.Exists(ctx, manifestPath)
	if err != nil {
		return report, fmt.Errorf("checking manifest: %w", err)
	}
	if !exists {
		report.Issues = append(report.Issues, ValidationIssue{
			Kind:    ValidationIssueMissingManifest,
			Message: "manifest.json missing from storage",
		})
	}

	return report, nil
}
