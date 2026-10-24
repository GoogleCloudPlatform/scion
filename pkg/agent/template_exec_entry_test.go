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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestProvisionAgent_ExecTemplateCacheEntryKeepsHashAndName provisions from
// a hub template with an executable file, hydrated into the cache under its
// entry key, which covers paths and modes (ptone/scion#5652,
// ptone/scion#5658). agent-info.json must record
// the slug as the template name and the entry key as the template hash, as
// for any hydrated template, and the key must find the cached directory
// again (the start and cleared-fields paths look it up by that hash).
func TestProvisionAgent_ExecTemplateCacheEntryKeepsHashAndName(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0o755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	cache, err := templatecache.New(filepath.Join(tmpDir, "cache"), templatecache.DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{
		"scion-agent.json":        transfer.ModeRegular,
		"plugins/demo/bin/server": transfer.ModeExecutable,
	}
	files := map[string][]byte{
		"scion-agent.json":        []byte(`{"default_harness_config":"claude"}`),
		"plugins/demo/bin/server": []byte("#!/bin/sh\n"),
	}
	var infos []transfer.FileInfo
	for p, data := range files {
		infos = append(infos, transfer.FileInfo{Path: p, Hash: transfer.HashBytes(data), Mode: transfer.FormatMode(modes[p])})
	}
	key := templatecache.EntryKey(infos)
	if key == testContentHash || !transfer.IsContentHash(key) {
		t.Fatalf("precondition: exec entry key %q must differ from the content hash and keep its form", key)
	}
	cacheDir, err := cache.PutWithModes(key, files, modes)
	if err != nil {
		t.Fatal(err)
	}

	scionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(scionDir, 0o755)
	ctx := api.ContextWithTemplateName(context.Background(), "plugin-template")
	agentHome, _, _, err := ProvisionAgent(ctx, "exec-agent", cacheDir, "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Template != "plugin-template" {
		t.Errorf("agent-info.json template = %q, want the slug %q", info.Template, "plugin-template")
	}
	if info.TemplateHash != key {
		t.Errorf("agent-info.json templateHash = %q, want the cache entry key %q", info.TemplateHash, key)
	}
	if got, ok := cache.Get(info.TemplateHash); !ok || got != cacheDir {
		t.Errorf("cache.Get(recorded hash) = (%q, %v), want (%q, true)", got, ok, cacheDir)
	}
	cfg := &api.ScionConfig{Info: &info}
	if !templateHydrated(nil, cfg) {
		t.Error("an agent from an exec template is not classified as hydrated")
	}
	if got := agentTemplateDisplayName("", cfg); got != "plugin-template" {
		t.Errorf("display name = %q, want %q", got, "plugin-template")
	}
}
