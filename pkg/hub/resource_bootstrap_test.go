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
	"context"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

func TestBootstrapBundledResources_EmptyDB(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	err := srv.BootstrapBundledResources(ctx, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapBundledResources failed: %v", err)
	}

	// Verify templates were created
	templates, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	expectedTemplates := len(resources.BuiltinTemplates())
	if templates.TotalCount != expectedTemplates {
		t.Errorf("expected %d templates, got %d", expectedTemplates, templates.TotalCount)
	}
	for _, tmpl := range templates.Items {
		if tmpl.Status != store.TemplateStatusActive {
			t.Errorf("template %q: expected active status, got %q", tmpl.Name, tmpl.Status)
		}
		if !IsBuiltinManaged(tmpl.SourceURL) {
			t.Errorf("template %q: expected built-in source URL, got %q", tmpl.Name, tmpl.SourceURL)
		}
		if tmpl.ContentHash == "" {
			t.Errorf("template %q: expected non-empty content hash", tmpl.Name)
		}
		if len(tmpl.Files) == 0 {
			t.Errorf("template %q: expected files, got none", tmpl.Name)
		}
	}

	// Verify harness-configs were created
	configs, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	expectedConfigs := len(resources.BuiltinHarnessConfigs())
	if configs.TotalCount != expectedConfigs {
		t.Errorf("expected %d harness configs, got %d", expectedConfigs, configs.TotalCount)
	}
	for _, hc := range configs.Items {
		if hc.Status != store.HarnessConfigStatusActive {
			t.Errorf("harness-config %q: expected active status, got %q", hc.Name, hc.Status)
		}
		if !IsBuiltinManaged(hc.SourceURL) {
			t.Errorf("harness-config %q: expected built-in source URL, got %q", hc.Name, hc.SourceURL)
		}
		if hc.ContentHash == "" {
			t.Errorf("harness-config %q: expected non-empty content hash", hc.Name)
		}
		if hc.Harness == "" {
			t.Errorf("harness-config %q: expected non-empty harness type", hc.Name)
		}
	}
}

func TestBootstrapBundledResources_Idempotent(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	opts := BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	}

	// First run: creates everything
	if err := srv.BootstrapBundledResources(ctx, opts); err != nil {
		t.Fatalf("first BootstrapBundledResources failed: %v", err)
	}

	// Capture state after first run
	templates1, _ := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	configs1, _ := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})

	templateHashes := make(map[string]string)
	for _, tmpl := range templates1.Items {
		templateHashes[tmpl.Slug] = tmpl.ContentHash
	}
	configHashes := make(map[string]string)
	for _, hc := range configs1.Items {
		configHashes[hc.Slug] = hc.ContentHash
	}

	// Second run: should be no-op
	if err := srv.BootstrapBundledResources(ctx, opts); err != nil {
		t.Fatalf("second BootstrapBundledResources failed: %v", err)
	}

	// Verify counts are unchanged
	templates2, _ := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	configs2, _ := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})

	if templates2.TotalCount != templates1.TotalCount {
		t.Errorf("template count changed: %d -> %d", templates1.TotalCount, templates2.TotalCount)
	}
	if configs2.TotalCount != configs1.TotalCount {
		t.Errorf("harness-config count changed: %d -> %d", configs1.TotalCount, configs2.TotalCount)
	}

	// Verify content hashes are unchanged
	for _, tmpl := range templates2.Items {
		if tmpl.ContentHash != templateHashes[tmpl.Slug] {
			t.Errorf("template %q content hash changed on idempotent re-run", tmpl.Name)
		}
	}
	for _, hc := range configs2.Items {
		if hc.ContentHash != configHashes[hc.Slug] {
			t.Errorf("harness-config %q content hash changed on idempotent re-run", hc.Name)
		}
	}
}

func TestBootstrapBundledResources_ParallelConverges(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	opts := BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	}

	// Run two bootstraps in parallel to simulate HA replicas
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = srv.BootstrapBundledResources(ctx, opts)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("parallel bootstrap %d failed: %v", i, err)
		}
	}

	// Verify exactly one copy of each resource exists
	templates, _ := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	expectedTemplates := len(resources.BuiltinTemplates())
	if templates.TotalCount != expectedTemplates {
		t.Errorf("expected %d templates after parallel bootstrap, got %d", expectedTemplates, templates.TotalCount)
	}

	configs, _ := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})
	expectedConfigs := len(resources.BuiltinHarnessConfigs())
	if configs.TotalCount != expectedConfigs {
		t.Errorf("expected %d harness-configs after parallel bootstrap, got %d", expectedConfigs, configs.TotalCount)
	}

	// All resources should be active
	for _, tmpl := range templates.Items {
		if tmpl.Status != store.TemplateStatusActive {
			t.Errorf("template %q: expected active after parallel bootstrap, got %q", tmpl.Name, tmpl.Status)
		}
	}
	for _, hc := range configs.Items {
		if hc.Status != store.HarnessConfigStatusActive {
			t.Errorf("harness-config %q: expected active after parallel bootstrap, got %q", hc.Name, hc.Status)
		}
	}
}

func TestBootstrapBundledResources_NoLocalDirRequired(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// BootstrapBundledResources reads from the embedded FS, not from
	// ~/.scion/templates/default. This test verifies it succeeds even
	// when no local template directory exists (which is always the case
	// in the test environment).
	err := srv.BootstrapBundledResources(ctx, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapBundledResources failed (no local dir): %v", err)
	}

	templates, _ := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	if templates.TotalCount == 0 {
		t.Error("expected at least one template from bundled resources, got 0")
	}
}

func TestBootstrapBundledResources_NoStorage(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Remove the storage backend to verify graceful handling
	srv.SetStorage(nil)

	err := srv.BootstrapBundledResources(ctx, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("expected nil error without storage, got: %v", err)
	}
}

func TestBootstrapBundledResources_SkipIfAnyExist_UpdatesExisting(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// First bootstrap without SkipIfAnyExist to seed all configs.
	err := srv.BootstrapBundledResources(ctx, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("initial bootstrap failed: %v", err)
	}

	configs1, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if configs1.TotalCount == 0 {
		t.Fatal("expected at least one harness config after initial bootstrap")
	}

	// Record initial hashes.
	initialHashes := make(map[string]string)
	for _, hc := range configs1.Items {
		initialHashes[hc.Slug] = hc.ContentHash
	}

	// Second bootstrap with SkipIfAnyExist should NOT skip updates for
	// existing configs. Since the content hasn't changed, hashes should
	// remain the same but the call must not error (verifies the update
	// path is reached, not short-circuited).
	err = srv.BootstrapBundledResources(ctx, BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
		SkipIfAnyExist:  true,
	})
	if err != nil {
		t.Fatalf("second bootstrap with SkipIfAnyExist failed: %v", err)
	}

	configs2, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if configs2.TotalCount != configs1.TotalCount {
		t.Errorf("expected %d harness configs, got %d", configs1.TotalCount, configs2.TotalCount)
	}
	for _, hc := range configs2.Items {
		if hc.ContentHash != initialHashes[hc.Slug] {
			t.Errorf("harness-config %q: content hash changed unexpectedly", hc.Name)
		}
	}
}

func TestResolveHarnessType(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		want    string
		wantErr bool
	}{
		{
			name:  "valid config",
			files: map[string]string{"config.yaml": "harness: claude\nimage: test:latest\n"},
			want:  "claude",
		},
		{
			name:    "missing config.yaml",
			files:   map[string]string{"other.txt": "data"},
			wantErr: true,
		},
		{
			name:    "missing harness field",
			files:   map[string]string{"config.yaml": "image: test:latest\n"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := resources.BundledResource{
				Kind: storage.ResourceKindHarnessConfig,
				Name: "test",
				FS:   testFS(tt.files),
				Root: ".",
			}
			got, err := resolveHarnessType(r)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
