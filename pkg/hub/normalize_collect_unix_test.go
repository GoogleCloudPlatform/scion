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

//go:build !no_sqlite && !windows

package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// replaceDirDuringNormalize installs a normalization hook that, when the
// file a.txt is about to be read, moves the directory being normalized away
// and creates a different directory at its path (still a valid
// harness-config). It returns the moved directory's path.
func replaceDirDuringNormalize(t *testing.T) *string {
	t.Helper()
	var moved string
	restore := transfer.SetNormalizeBeforeReadForTest(func(dir, rel string) {
		if rel != "a.txt" || moved != "" {
			return
		}
		moved = dir + ".moved"
		if err := os.Rename(dir, moved); err != nil {
			t.Error(err)
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: claude\n"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("swapped in\n"), 0o644); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		restore()
		if moved != "" {
			_ = os.RemoveAll(moved)
		}
	})
	return &moved
}

// Bootstrap normalizes and collects through ONE root: a directory replaced
// in the middle of the normalization is neither collected from the old root
// with the new path's content nor read from the new directory; the final
// check refuses it (errCollectedDirChanged) and nothing is stored.
func TestBootstrap_NormalizeAndCollectThroughOneRoot(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	dir := filepath.Join(writeCollectedTestTree(t, map[string]string{
		"cfg/a.txt":       "x\r\n",
		"cfg/config.yaml": "harness: claude\n",
	}), "cfg")
	moved := replaceDirDuringNormalize(t)
	_, err := srv.harnessConfigStore("claude").Bootstrap(context.Background(), "cfg", dir, store.HarnessConfigScopeGlobal, "", "", false)
	if *moved == "" {
		t.Fatal("the normalization hook did not run (Bootstrap did not normalize through transfer.NormalizeRoot)")
	}
	if !errors.Is(err, errCollectedDirChanged) {
		t.Fatalf("Bootstrap error = %v, want errCollectedDirChanged", err)
	}
	if hc, ok := harnessConfigsByName(t, s)["cfg"]; ok {
		t.Errorf("a harness-config was stored from a directory replaced during normalization: %+v", hc)
	}
}

// BootstrapSource (the staged built-in path) does the same: the staged
// directory replaced during normalization is refused and nothing is stored.
func TestBootstrapSource_NormalizeAndCollectThroughOneRoot(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	res := testBundledResource(storage.ResourceKindHarnessConfig, "cfg", map[string]string{
		"a.txt":       "x\r\n",
		"config.yaml": "harness: claude\n",
	})
	moved := replaceDirDuringNormalize(t)
	_, err := srv.harnessConfigStore("claude").BootstrapSource(context.Background(), NewFSResourceSource(res), BootstrapOptions{OverwritePolicy: OverwriteBuiltinManaged})
	if *moved == "" {
		t.Fatal("the normalization hook did not run (BootstrapSource did not normalize through transfer.NormalizeRoot)")
	}
	if !errors.Is(err, errCollectedDirChanged) {
		t.Fatalf("BootstrapSource error = %v, want errCollectedDirChanged", err)
	}
	if hc, ok := harnessConfigsByName(t, s)["cfg"]; ok {
		t.Errorf("a harness-config was stored from a staged directory replaced during normalization: %+v", hc)
	}
}
