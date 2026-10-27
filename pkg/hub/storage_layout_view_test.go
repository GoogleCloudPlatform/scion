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
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestStorageValuesAllowed: only an unscoped hub admin may see storage
// layout values.
func TestStorageValuesAllowed(t *testing.T) {
	ctx := context.Background()
	admin := contextWithIdentity(ctx, NewAuthenticatedUser("u-admin", "admin@example.com", "Admin", "admin", "cli"))
	member := contextWithIdentity(ctx, NewAuthenticatedUser("u-member", "member@example.com", "Member", "member", "cli"))
	if !storageValuesAllowed(admin) {
		t.Error("an admin may not see values")
	}
	if storageValuesAllowed(member) || storageValuesAllowed(ctx) {
		t.Error("a member or an anonymous caller may see values")
	}

	b := store.RuntimeBroker{ID: "b", WorkspaceStorage: &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "nfs",
		NFS:    &api.BrokerNFSWorkspaceStorage{Server: "10.9.8.7", Export: "/fixture-export", SubPathRoot: "fixture-root", Healthy: true, ExportID: "fixture-export-id"},
		Volume: &api.BrokerVolumeWorkspaceStorage{ClaimName: "fixture-claim", SubPathRoot: "fixture-root"}}}
	for name, c := range map[string]context.Context{"member": member, "anonymous": ctx} {
		got := brokerStorageForViewer(c, b, false)
		if s := fmt.Sprintf("%+v %+v", *got.WorkspaceStorage.NFS, *got.WorkspaceStorage.Volume); strings.Contains(s, "fixture") || strings.Contains(s, "10.9.8.7") {
			t.Errorf("%s: values shown: %s", name, s)
		}
		if got.WorkspaceStorage.Backend != "nfs" || got.WorkspaceStorage.Homes != "nfs" || !got.WorkspaceStorage.NFS.Healthy {
			t.Errorf("%s: backend, homes or health dropped: %+v", name, got.WorkspaceStorage)
		}
		if !got.WorkspaceStorage.Redacted {
			t.Errorf("%s: the redacted descriptor is not marked Redacted", name)
		}
		if b.WorkspaceStorage.NFS.Server != "10.9.8.7" || b.WorkspaceStorage.Redacted {
			t.Fatal("the stored record was modified")
		}
	}
	if got := brokerStorageForViewer(admin, b, false); got.WorkspaceStorage.NFS.Server != "10.9.8.7" || got.WorkspaceStorage.Redacted {
		t.Error("an admin does not see the values")
	}
	if got := brokerStorageForViewer(member, b, true); got.WorkspaceStorage.NFS.Export != "/fixture-export" {
		t.Error("the broker reading its own record does not see the values")
	}
}

// TestMoveRefusals_NameStorageFieldsOnly: the move refusals for brokers on
// different exports, or with different export identity markers, name the
// fields and never the configured values.
func TestMoveRefusals_NameStorageFieldsOnly(t *testing.T) {
	values := []string{"10.0.0.2", "10.0.0.3", "/vol1", "/vol2", "projects", "other", moveTestExportID, "0a0a0a0a-0000-4000-8000-000000000000"}
	for name, mutate := range map[string]func(in *moveEligibilityInput){
		"different server and export": func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage = moveTestStorage("10.0.0.3", "/vol2", "other", true)
		},
		"target local": func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage = &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal}
		},
		"different export identity": func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage.NFS.ExportID = "0a0a0a0a-0000-4000-8000-000000000000"
		},
		"missing export identity": func(in *moveEligibilityInput) {
			in.Dst.WorkspaceStorage.NFS.ExportID = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := eligibleMoveInput(moveTestProbeCalls{})
			mutate(&in)
			v, ref := evaluateMoveEligibility(in)
			if ref == nil {
				t.Fatal("expected a refusal")
			}
			if ref.Status != http.StatusConflict && ref.Status != http.StatusPreconditionFailed {
				t.Fatalf("status %d: %s", ref.Status, ref.Message)
			}
			body := ref.Message + fmt.Sprintf("%+v", v.Checks)
			for _, val := range values {
				if val == "" || (val == "projects" && strings.Contains(body, "project_storage")) {
					continue
				}
				if strings.Contains(body, val) {
					t.Errorf("refusal shows the value %q: %s", val, body)
				}
			}
		})
	}
}

// layoutValueField matches an expression naming a storage layout value: a
// descriptor's nfs or volume field, a Layout's share field, or an nfs
// block's share or mount_root field.
var layoutValueField = regexp.MustCompile(`(NFS|Volume|Share|Shares\[[^]]*\]|share)\.(Server|Export|SubPathRoot|ExportID|ClaimName|PVName|MountRoot|ID)$`)

// TestLayoutValuesFormattedOnlyInRenderer: no pkg/hub production code
// formats a storage layout value into text with fmt; values become text
// only in pkg/projectfs's renderer (RenderLayoutIssues).
func TestLayoutValuesFormattedOnlyInRenderer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					e, ok := m.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					var b strings.Builder
					_ = printer.Fprint(&b, fset, e)
					if layoutValueField.MatchString(b.String()) {
						t.Errorf("%s: fmt.%s formats the storage layout value %s; render it with projectfs.RenderLayoutIssues",
							fset.Position(e.Pos()), sel.Sel.Name, b.String())
					}
					return true
				})
			}
			return true
		})
	}
}
