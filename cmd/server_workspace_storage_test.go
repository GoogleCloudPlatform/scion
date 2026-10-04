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

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestValidateHubWorkspaceStorage_FromSettings exercises the hub-load path:
// settings.yaml is read by the real loader (config.LoadGlobalConfig) and
// handed to validateHubWorkspaceStorage, which runServerStart calls before
// the hub starts. An unusable workspace_storage block must fail startup.
func TestValidateHubWorkspaceStorage_FromSettings(t *testing.T) {
	tests := []struct {
		name    string
		storage string // YAML under server.workspace_storage; empty omits the block
		wantErr string // empty means startup proceeds
	}{
		{name: "no workspace_storage block"},
		{name: "local", storage: "backend: local"},
		{
			name: "nfs with shares",
			storage: `backend: nfs
    nfs:
      mount_root: /mnt/nfs
      shares:
        - id: ws1
          server: 10.0.0.2
          export: /scion-workspaces`,
		},
		{
			// Previously the hub silently served local project paths for
			// this config (hubManagedProjectPath requires shares).
			name:    "nfs without shares",
			storage: "backend: nfs\n    nfs:\n      mount_root: /mnt/nfs",
			wantErr: "no NFS shares are defined",
		},
		{name: "cloudrun-volume", storage: "backend: cloudrun-volume\n    cloudrun_volume:\n      volume_name: workspaces"},
		{
			name:    "cloudrun-volume without volume_name",
			storage: "backend: cloudrun-volume\n    cloudrun_volume:\n      subpath_root: projects",
			wantErr: "cloudrun_volume.volume_name is not set",
		},
		{
			name:    "gke-shared-volume without volume_name",
			storage: "backend: gke-shared-volume\n    gke_shared_volume:\n      pv_claim_name: scion-workspaces",
			wantErr: "gke_shared_volume.volume_name is not set",
		},
		{
			name:    "gke-shared-volume with absolute subpath_root",
			storage: "backend: gke-shared-volume\n    gke_shared_volume:\n      volume_name: workspaces\n      subpath_root: /projects",
			wantErr: "gke_shared_volume.subpath_root",
		},
		{name: "unknown backend", storage: "backend: s3", wantErr: `backend "s3" is not supported`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// LoadGlobalConfig also reads ~/.scion; isolate it.
			t.Setenv("HOME", t.TempDir())

			settings := "schema_version: \"1\"\nserver:\n  mode: hosted\n"
			if tt.storage != "" {
				settings += "  workspace_storage:\n    " + tt.storage + "\n"
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.LoadGlobalConfig(dir)
			if err != nil {
				t.Fatalf("LoadGlobalConfig: %v", err)
			}
			if tt.storage != "" && cfg.WorkspaceStorage == nil {
				t.Fatal("workspace_storage was not loaded; the test would pass vacuously")
			}

			err = validateHubWorkspaceStorage(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHubWorkspaceStorage: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateHubWorkspaceStorage: expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), "invalid server.workspace_storage") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to name server.workspace_storage and contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestServerForeground_WiresHubWorkspaceStorageValidation pins the call that
// makes the check above matter: runServerStart must call
// validateHubWorkspaceStorage, under `if enableHub`, so a broker-only
// process keeps its warn-and-skip NFS behaviour.
func TestServerForeground_WiresHubWorkspaceStorageValidation(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "server_foreground.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	calls, gated := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runServerStart" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ifStmt, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			cond, ok := ifStmt.Cond.(*ast.Ident)
			if !ok || cond.Name != "enableHub" {
				return true
			}
			ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
				if call, ok := m.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "validateHubWorkspaceStorage" {
						gated++
					}
				}
				return true
			})
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "validateHubWorkspaceStorage" {
					calls++
				}
			}
			return true
		})
	}

	if calls != 1 || gated != 1 {
		t.Fatalf("runServerStart calls validateHubWorkspaceStorage %d time(s), %d under `if enableHub`; want exactly one, gated", calls, gated)
	}
}
