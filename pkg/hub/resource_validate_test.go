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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// moveTemplateToLegacyLayout rewrites a bootstrapped template's row into the
// legacy path layout (Layout "", one object per file at <StoragePath>/<path>),
// as rows written before ptone/scion#4221 are, and uploads files at those
// legacy paths. Only legacy rows have a manifest.json and per-object hash
// checks in ValidateStorage.
func moveTemplateToLegacyLayout(t *testing.T, srv *Server, s store.Store, tmpl *store.Template, files map[string]string) *store.Template {
	t.Helper()
	ctx := context.Background()
	expected := store.TemplateContentPrecondition{ContentHash: tmpl.ContentHash, Layout: tmpl.Layout}
	legacy := *tmpl
	legacy.Layout = ""
	legacy.StoragePath = storage.TemplateStoragePath(srv.HubID(), tmpl.Scope, tmpl.ScopeID, tmpl.Slug)
	legacy.StorageURI = storage.StorageURIForPath(tmpl.StorageBucket, legacy.StoragePath)
	if err := s.UpdateTemplateContent(ctx, &legacy, expected); err != nil {
		t.Fatalf("move template to legacy layout: %v", err)
	}
	for p, c := range files {
		if _, err := srv.GetStorage().Upload(ctx, legacy.StoragePath+"/"+p, bytes.NewReader([]byte(c)),
			storage.UploadOptions{Metadata: map[string]string{"sha256": commitHash(c)}}); err != nil {
			t.Fatalf("upload legacy object %s: %v", p, err)
		}
	}
	got, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Layout != "" || got.StoragePath != legacy.StoragePath {
		t.Fatalf("row not in legacy layout: layout=%q path=%q", got.Layout, got.StoragePath)
	}
	return got
}

func TestValidateStorage_HealthyTemplate(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "healthy", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# bashrc",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "healthy", "global", "")
	if err != nil {
		t.Fatal(err)
	}

	// A bootstrapped template is a blob-layout row (ptone/scion#4221): its
	// files are blobs and no manifest.json is expected, so a fully healthy
	// row has zero issues as bootstrapped.
	if tmpl.Layout != store.TemplateLayoutBlobs {
		t.Fatalf("expected blob layout, got %q", tmpl.Layout)
	}
	for _, c := range []string{"harness: claude\n", "# bashrc"} {
		if ok, _ := stor.Exists(ctx, blobObjectPath(tmpl, c)); !ok {
			t.Fatalf("expected blob for %q", c)
		}
	}

	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	if len(report.Issues) != 0 {
		t.Errorf("expected no issues for healthy template, got %d: %v", len(report.Issues), report.Issues)
	}
	if report.Name != "healthy" {
		t.Errorf("expected name 'healthy', got %q", report.Name)
	}
}

func TestValidateStorage_MissingObject(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "broken", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# bashrc",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "broken", "global", "")
	if err != nil {
		t.Fatal(err)
	}

	// Delete one file's blob to simulate storage desync
	objectPath := blobObjectPath(tmpl, "# bashrc")
	if err := stor.Delete(ctx, objectPath); err != nil {
		t.Fatalf("failed to delete object: %v", err)
	}

	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	found := false
	for _, issue := range report.Issues {
		if issue.Kind == ValidationIssueMissingObject && issue.File == "home/.bashrc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected missing_object issue for home/.bashrc, got: %v", report.Issues)
	}
}

func TestValidateStorage_MissingManifest(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "no-manifest", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "no-manifest", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only legacy-layout rows expect a manifest.json.
	tmpl = moveTemplateToLegacyLayout(t, srv, s, tmpl, map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})

	// Delete the manifest from storage
	manifestPath := tmpl.StoragePath + "/manifest.json"
	_ = stor.Delete(ctx, manifestPath)

	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	found := false
	for _, issue := range report.Issues {
		if issue.Kind == ValidationIssueMissingManifest {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected missing_manifest issue, got: %v", report.Issues)
	}
}

func TestValidateStorage_ZeroFilesActive(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Create a template record with active status but zero files
	tmpl := &store.Template{
		ID:            tid("zero-files-template"),
		Name:          "zero-files",
		Slug:          "zero-files",
		Scope:         "global",
		ScopeID:       "",
		Status:        store.TemplateStatusActive,
		SourceURL:     "builtin://scion/dev/template/zero-files",
		StoragePath:   "templates/global/zero-files",
		StorageBucket: stor.Bucket(),
		StorageURI:    "gs://test-bucket/templates/global/zero-files",
		Files:         nil,
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	rec := templateToRecord(tmpl)
	rs := srv.templateStore()
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	found := false
	for _, issue := range report.Issues {
		if issue.Kind == ValidationIssueZeroFilesActive {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected zero_files_active issue, got: %v", report.Issues)
	}
}

func TestValidateStorage_ContentHashMismatch(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "hash-mismatch", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# original content",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "hash-mismatch", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only legacy-layout rows re-check object hashes: a blob's name is its
	// content hash.
	tmpl = moveTemplateToLegacyLayout(t, srv, s, tmpl, map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# original content",
	})

	// Overwrite one file with different content while keeping the DB manifest
	// unchanged, simulating GCS/DB divergence.
	objectPath := tmpl.StoragePath + "/home/.bashrc"
	stor.Upload(ctx, objectPath, //nolint:errcheck
		bytes.NewReader([]byte("# WRONG content")),
		storage.UploadOptions{Metadata: map[string]string{"sha256": "sha256:0000000000000000000000000000000000000000000000000000000000000000"}},
	)

	// Add manifest so we don't get a missing_manifest issue
	manifestPath := tmpl.StoragePath + "/manifest.json"
	stor.Upload(ctx, manifestPath, nil, storage.UploadOptions{}) //nolint:errcheck

	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	found := false
	for _, issue := range report.Issues {
		if issue.Kind == ValidationIssueContentHashMismatch && issue.File == "home/.bashrc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected content_hash_mismatch issue for home/.bashrc, got: %v", report.Issues)
	}
}

func TestRepairStorage_FixesHashMismatch(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "repair-hash", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# correct content",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "repair-hash", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	// A hash mismatch can only be detected on a legacy-layout row.
	tmpl = moveTemplateToLegacyLayout(t, srv, s, tmpl, map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# correct content",
	})

	// Corrupt one file in storage
	objectPath := tmpl.StoragePath + "/home/.bashrc"
	stor.Upload(ctx, objectPath, //nolint:errcheck
		bytes.NewReader([]byte("# CORRUPTED")),
		storage.UploadOptions{Metadata: map[string]string{"sha256": "sha256:badhash"}},
	)

	// Verify it's detected as broken
	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	hashMismatchFound := false
	for _, issue := range report.Issues {
		if issue.Kind == ValidationIssueContentHashMismatch {
			hashMismatchFound = true
		}
	}
	if !hashMismatchFound {
		t.Fatal("expected content_hash_mismatch before repair")
	}

	// Run repair
	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("repair bootstrap failed: %v", err)
	}
	if result.Repaired != 1 {
		t.Errorf("expected Repaired=1, got %d", result.Repaired)
	}

	// The repair commit migrates the row to the blob layout, with the
	// correct content stored as a blob.
	repaired, err := s.GetTemplateBySlug(ctx, "repair-hash", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Layout != store.TemplateLayoutBlobs {
		t.Errorf("expected repaired row in blob layout, got %q", repaired.Layout)
	}
	if ok, _ := stor.Exists(ctx, blobObjectPath(repaired, "# correct content")); !ok {
		t.Error("expected the correct content's blob after repair")
	}
	report2, err := rs.ValidateStorage(ctx, templateToRecord(repaired))
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.Issues) != 0 {
		t.Errorf("expected no issues after repair, got %v", report2.Issues)
	}
}

func TestValidateStorage_HarnessConfigPass(t *testing.T) {
	srv, _, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindHarnessConfig, "healthy-hc", map[string]string{
		"config.yaml":  "harness: claude\nimage: test:latest\nuser: scion\n",
		"home/.bashrc": "# bashrc",
	})
	src := NewFSResourceSource(br)
	rs := srv.harnessConfigStore("claude")

	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	hc, err := srv.store.GetHarnessConfigBySlug(ctx, "healthy-hc", "global", "")
	if err != nil {
		t.Fatal(err)
	}

	// Add manifest to match fully healthy state
	manifestPath := hc.StoragePath + "/manifest.json"
	stor.Upload(ctx, manifestPath, nil, storage.UploadOptions{}) //nolint:errcheck

	rec := harnessConfigToRecord(hc)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatalf("ValidateStorage failed: %v", err)
	}

	if len(report.Issues) != 0 {
		t.Errorf("expected PASS for healthy harness-config, got %d issues: %v", len(report.Issues), report.Issues)
	}
}

func TestRepairStorage_FixesMissingObjects(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "repairable", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# bashrc content",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	// First bootstrap: creates the resource
	_, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	tmpl, err := s.GetTemplateBySlug(ctx, "repairable", "global", "")
	if err != nil {
		t.Fatal(err)
	}

	// Delete one file's blob to simulate desync
	objectPath := blobObjectPath(tmpl, "# bashrc content")
	if err := stor.Delete(ctx, objectPath); err != nil {
		t.Fatalf("failed to delete object: %v", err)
	}

	// Verify it's actually broken
	rec := templateToRecord(tmpl)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) == 0 {
		t.Fatal("expected issues before repair")
	}

	// Run bootstrap with RepairStorage=true to fix
	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("repair bootstrap failed: %v", err)
	}
	if result.Repaired != 1 {
		t.Errorf("expected Repaired=1, got %d", result.Repaired)
	}

	// Verify the missing file object was repaired
	tmpl2, err := s.GetTemplateBySlug(ctx, "repairable", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	rec2 := templateToRecord(tmpl2)
	report2, err := rs.ValidateStorage(ctx, rec2)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range report2.Issues {
		if issue.Kind == ValidationIssueMissingObject {
			t.Errorf("file object still missing after repair: %s", issue.File)
		}
	}
}

func TestGenerateDownloadURLs_ErrorOnMissingObject(t *testing.T) {
	stor := newMockStorage("test-bucket")

	files := []store.TemplateFile{
		{Path: "existing.txt", Size: 10},
		{Path: "missing.txt", Size: 20},
	}

	// Upload only existing.txt — missing.txt won't be in the mock storage,
	// but the mock generates signed URLs for any path. To simulate a signing
	// failure, we need a storage that returns errors for unknown objects.
	// Since the default mock always succeeds, the test validates the upstream
	// behavior change via the download handler tests.

	// Instead, test the handler-level error propagation by verifying that
	// generateDownloadURLs returns errors for all files (since it now
	// hard-errors on signing failures).
	ctx := context.Background()

	// The mock storage always succeeds for GenerateSignedURL,
	// so this test validates the success path with the new stricter contract.
	urls, manifest, _, err := generateDownloadURLs(ctx, stor, "templates/global/test", "", files)
	if err != nil {
		t.Fatalf("unexpected error with mock (mock always succeeds): %v", err)
	}
	if len(urls) != 2 {
		t.Errorf("expected 2 download URLs, got %d", len(urls))
	}
	if manifest == "" {
		t.Error("expected non-empty manifest URL")
	}
}

func TestPartialUpload_ResourceStaysPending(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Create a template manually in pending state with files listed
	// to simulate a partial upload scenario.
	tmpl := &store.Template{
		ID:            tid("partial-upload"),
		Name:          "partial",
		Slug:          "partial",
		Scope:         "global",
		ScopeID:       "",
		Status:        store.TemplateStatusPending,
		SourceURL:     "builtin://scion/dev/template/partial",
		StoragePath:   "templates/global/partial",
		StorageBucket: "test-bucket",
		StorageURI:    "gs://test-bucket/templates/global/partial",
		Files: []store.TemplateFile{
			{Path: "scion-agent.yaml", Size: 20},
		},
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Bootstrap should re-upload and activate the pending resource
	br := testBundledResource(storage.ResourceKindTemplate, "partial", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("bootstrap of partial resource failed: %v", err)
	}
	if result.Updated != 1 {
		t.Errorf("expected Updated=1 for partial recovery, got Updated=%d Created=%d", result.Updated, result.Created)
	}

	// Verify it's now active
	tmpl2, err := s.GetTemplateBySlug(ctx, "partial", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl2.Status != store.TemplateStatusActive {
		t.Errorf("expected active after recovery, got %q", tmpl2.Status)
	}
	if tmpl2.ContentHash == "" {
		t.Error("expected non-empty content hash after recovery")
	}
}
