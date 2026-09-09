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
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// enclosingFuncName returns the name of the function containing the given
// byte offset. Shared by create_message_enumeration_test.go and
// msg_containment_callsite_test.go.
func enclosingFuncName(fset *token.FileSet, f *ast.File, offset int) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Offset
		end := fset.Position(fn.Body.End()).Offset
		if offset >= start && offset <= end {
			return fn.Name.Name
		}
	}
	return "<unknown>"
}

// findHubDir locates the pkg/hub directory relative to the test file.
// Shared by create_message_enumeration_test.go and
// msg_containment_callsite_test.go.
func findHubDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "server.go")); err != nil {
		t.Fatalf("working directory %q does not look like pkg/hub (server.go not found)", wd)
	}
	return wd
}
