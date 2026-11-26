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

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func templateFileInfosForHash(files []store.TemplateFile) []transfer.FileInfo {
	infos := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		infos[i] = transfer.FileInfo{
			Path: f.Path,
			Size: f.Size,
			Hash: f.Hash,
			Mode: f.Mode,
		}
	}
	return infos
}

func upsertContentHashV2Override(t *testing.T, ctx context.Context, st store.Store, enabled bool) {
	t.Helper()
	hs, ok := st.(store.HubSettingStore)
	if !ok {
		t.Fatalf("store %T does not implement HubSettingStore", st)
	}
	doc, err := json.Marshal(opsettings.ExperimentsSettings{
		Overrides: map[string]bool{experiments.ContentHashV2: enabled},
	})
	if err != nil {
		t.Fatalf("marshal experiments override: %v", err)
	}
	var expectedRev int64
	if existing, err := hs.GetHubSetting(ctx, "experiments"); err == nil && existing != nil {
		expectedRev = existing.Revision
	}
	if _, err := hs.UpsertHubSetting(ctx, "experiments", doc, "admin", expectedRev, opsettings.OriginManaged); err != nil {
		t.Fatalf("UpsertHubSetting(experiments): %v", err)
	}
}

func registryWithContentHashV2Default(t *testing.T, defaultOn bool) *experiments.Registry {
	t.Helper()
	var list []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.ContentHashV2 {
			e.Default = defaultOn
		}
		list = append(list, e)
	}
	reg, err := experiments.NewRegistry(list, experiments.Default().Retired())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// TestStartupBootstrap_ContentHashV2OverrideFalseRevertsBuiltinsToV1 verifies
// the rollback path when hub.content_hash_v2 defaults to on: after built-in
// templates and harness configs have been bootstrapped with v2 hashes, storing
// hub.content_hash_v2=false and restarting runs initOperationalSettings before
// resource bootstrap and re-syncs the built-in rows back to v1 hashes (in both
// hosted and workstation bootstrap modes).
func TestStartupBootstrap_ContentHashV2OverrideFalseRevertsBuiltinsToV1(t *testing.T) {
	t.Run("hosted", func(t *testing.T) {
		ctx := context.Background()
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}

		st := newTestStore(t)
		stor, err := storage.NewLocal(storage.Config{LocalPath: t.TempDir(), Bucket: "test"})
		if err != nil {
			t.Fatalf("storage.NewLocal: %v", err)
		}
		cfg := &config.GlobalConfig{}

		// Boot 1: no override -> compiled default (on) writes v2 hashes.
		srv1, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New (boot 1): %v", err)
		}
		srv1.SetStorage(stor)
		if err := initOperationalSettings(ctx, cfg, srv1, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings (boot 1): %v", err)
		}
		if err := srv1.BootstrapBundledResources(ctx, hub.BootstrapOptions{
			RepairStorage:   true,
			OverwritePolicy: hub.OverwriteBuiltinManaged,
		}); err != nil {
			t.Fatalf("BootstrapBundledResources (boot 1): %v", err)
		}

		tmplList, err := st.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 100})
		if err != nil || len(tmplList.Items) == 0 {
			t.Fatalf("ListTemplates (boot 1): len=%d err=%v", len(tmplList.Items), err)
		}
		hcList, err := st.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeGlobal}, store.ListOptions{Limit: 100})
		if err != nil || len(hcList.Items) == 0 {
			t.Fatalf("ListHarnessConfigs (boot 1): len=%d err=%v", len(hcList.Items), err)
		}

		for _, tmpl := range tmplList.Items {
			infos := templateFileInfosForHash(tmpl.Files)
			v1, v2 := transfer.ComputeContentHashV1(infos), transfer.ComputeContentHashV2(infos)
			if v1 == v2 {
				t.Fatalf("template %q: test requires v1 != v2", tmpl.Name)
			}
			if tmpl.ContentHash != v2 {
				t.Fatalf("boot 1 template %q ContentHash = %q, want v2 %q", tmpl.Name, tmpl.ContentHash, v2)
			}
		}
		for _, hc := range hcList.Items {
			infos := templateFileInfosForHash(hc.Files)
			v2 := transfer.ComputeContentHashV2(infos)
			if hc.ContentHash != v2 {
				t.Fatalf("boot 1 harness config %q ContentHash = %q, want v2 %q", hc.Name, hc.ContentHash, v2)
			}
		}

		// Operator disables hub.content_hash_v2 and restarts the Hub.
		upsertContentHashV2Override(t, ctx, st, false)

		// Boot 2: fresh server loads operational settings before bootstrap.
		srv2, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New (boot 2): %v", err)
		}
		srv2.SetStorage(stor)
		if err := initOperationalSettings(ctx, cfg, srv2, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings (boot 2): %v", err)
		}
		if err := srv2.BootstrapBundledResources(ctx, hub.BootstrapOptions{
			RepairStorage:   true,
			OverwritePolicy: hub.OverwriteBuiltinManaged,
		}); err != nil {
			t.Fatalf("BootstrapBundledResources (boot 2): %v", err)
		}

		tmplList2, err := st.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 100})
		if err != nil || len(tmplList2.Items) == 0 {
			t.Fatalf("ListTemplates (boot 2): len=%d err=%v", len(tmplList2.Items), err)
		}
		for _, tmpl := range tmplList2.Items {
			v1 := transfer.ComputeContentHashV1(templateFileInfosForHash(tmpl.Files))
			if tmpl.ContentHash != v1 {
				t.Errorf("boot 2 template %q ContentHash = %q, want v1 %q after rollback", tmpl.Name, tmpl.ContentHash, v1)
			}
		}
		hcList2, err := st.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeGlobal}, store.ListOptions{Limit: 100})
		if err != nil || len(hcList2.Items) == 0 {
			t.Fatalf("ListHarnessConfigs (boot 2): len=%d err=%v", len(hcList2.Items), err)
		}
		for _, hc := range hcList2.Items {
			v1 := transfer.ComputeContentHashV1(templateFileInfosForHash(hc.Files))
			if hc.ContentHash != v1 {
				t.Errorf("boot 2 harness config %q ContentHash = %q, want v1 %q after rollback", hc.Name, hc.ContentHash, v1)
			}
		}
	})

	t.Run("workstation", func(t *testing.T) {
		ctx := context.Background()
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		tmplDir := filepath.Join(globalDir, "templates", "default")
		hcDir := filepath.Join(globalDir, "harness-configs", "claude")
		if err := os.MkdirAll(tmplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmplDir, "scion-agent.yaml"), []byte("schema_version: \"1\"\nharness_config: claude\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: scion-claude:latest\nuser: scion\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		st := newTestStore(t)
		stor, err := storage.NewLocal(storage.Config{LocalPath: t.TempDir(), Bucket: "test"})
		if err != nil {
			t.Fatalf("storage.NewLocal: %v", err)
		}
		cfg := &config.GlobalConfig{}

		// Boot 1: compiled default (on) records v2 hashes.
		srv1, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New (boot 1): %v", err)
		}
		srv1.SetStorage(stor)
		if err := initOperationalSettings(ctx, cfg, srv1, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings (boot 1): %v", err)
		}
		bootstrapWorkstationResources(ctx, st, srv1, globalDir)

		tmpl1, err := st.GetTemplateBySlug(ctx, "default", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatalf("GetTemplateBySlug (boot 1): %v", err)
		}
		if want := transfer.ComputeContentHashV2(templateFileInfosForHash(tmpl1.Files)); tmpl1.ContentHash != want {
			t.Fatalf("boot 1 workstation template ContentHash = %q, want v2 %q", tmpl1.ContentHash, want)
		}
		hc1, err := st.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
		if err != nil {
			t.Fatalf("GetHarnessConfigBySlug (boot 1): %v", err)
		}
		if want := transfer.ComputeContentHashV2(templateFileInfosForHash(hc1.Files)); hc1.ContentHash != want {
			t.Fatalf("boot 1 workstation harness config ContentHash = %q, want v2 %q", hc1.ContentHash, want)
		}

		// Operator disables hub.content_hash_v2 and restarts.
		upsertContentHashV2Override(t, ctx, st, false)

		srv2, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New (boot 2): %v", err)
		}
		srv2.SetStorage(stor)
		if err := initOperationalSettings(ctx, cfg, srv2, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings (boot 2): %v", err)
		}
		bootstrapWorkstationResources(ctx, st, srv2, globalDir)

		tmpl2, err := st.GetTemplateBySlug(ctx, "default", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatalf("GetTemplateBySlug (boot 2): %v", err)
		}
		if want := transfer.ComputeContentHashV1(templateFileInfosForHash(tmpl2.Files)); tmpl2.ContentHash != want {
			t.Errorf("boot 2 workstation template ContentHash = %q, want v1 %q after rollback", tmpl2.ContentHash, want)
		}
		hc2, err := st.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
		if err != nil {
			t.Fatalf("GetHarnessConfigBySlug (boot 2): %v", err)
		}
		if want := transfer.ComputeContentHashV1(templateFileInfosForHash(hc2.Files)); hc2.ContentHash != want {
			t.Errorf("boot 2 workstation harness config ContentHash = %q, want v1 %q after rollback", hc2.ContentHash, want)
		}
	})
}

// TestStartupBootstrap_ContentHashV2OverrideTrueOverridesCompiledDefaultOff
// verifies the reverse direction: when the compiled default is off, a stored
// hub.content_hash_v2=true override is loaded before bootstrap and upgrades
// v1-hashed built-ins to v2.
func TestStartupBootstrap_ContentHashV2OverrideTrueOverridesCompiledDefaultOff(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	stor, err := storage.NewLocal(storage.Config{LocalPath: t.TempDir(), Bucket: "test"})
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	cfg := &config.GlobalConfig{}
	regOff := registryWithContentHashV2Default(t, false)

	// Boot 1: compiled default off, no override -> built-ins get v1 hashes.
	srv1, err := hub.New(hub.ServerConfig{Experiments: regOff}, st)
	if err != nil {
		t.Fatalf("hub.New (boot 1): %v", err)
	}
	srv1.SetStorage(stor)
	if err := initOperationalSettings(ctx, cfg, srv1, st, globalDir); err != nil {
		t.Fatalf("initOperationalSettings (boot 1): %v", err)
	}
	if err := srv1.BootstrapBundledResources(ctx, hub.BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: hub.OverwriteBuiltinManaged,
	}); err != nil {
		t.Fatalf("BootstrapBundledResources (boot 1): %v", err)
	}

	tmplList1, err := st.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 100})
	if err != nil || len(tmplList1.Items) == 0 {
		t.Fatalf("ListTemplates (boot 1): len=%d err=%v", len(tmplList1.Items), err)
	}
	for _, tmpl := range tmplList1.Items {
		v1 := transfer.ComputeContentHashV1(templateFileInfosForHash(tmpl.Files))
		if tmpl.ContentHash != v1 {
			t.Fatalf("boot 1 template %q ContentHash = %q, want v1 %q", tmpl.Name, tmpl.ContentHash, v1)
		}
	}

	// Operator enables hub.content_hash_v2 and restarts.
	upsertContentHashV2Override(t, ctx, st, true)

	srv2, err := hub.New(hub.ServerConfig{Experiments: regOff}, st)
	if err != nil {
		t.Fatalf("hub.New (boot 2): %v", err)
	}
	srv2.SetStorage(stor)
	if err := initOperationalSettings(ctx, cfg, srv2, st, globalDir); err != nil {
		t.Fatalf("initOperationalSettings (boot 2): %v", err)
	}
	if err := srv2.BootstrapBundledResources(ctx, hub.BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: hub.OverwriteBuiltinManaged,
	}); err != nil {
		t.Fatalf("BootstrapBundledResources (boot 2): %v", err)
	}

	tmplList2, err := st.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 100})
	if err != nil || len(tmplList2.Items) == 0 {
		t.Fatalf("ListTemplates (boot 2): len=%d err=%v", len(tmplList2.Items), err)
	}
	for _, tmpl := range tmplList2.Items {
		v2 := transfer.ComputeContentHashV2(templateFileInfosForHash(tmpl.Files))
		if tmpl.ContentHash != v2 {
			t.Errorf("boot 2 template %q ContentHash = %q, want v2 %q with override=true", tmpl.Name, tmpl.ContentHash, v2)
		}
	}
}
