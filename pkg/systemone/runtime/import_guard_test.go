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

package runtime_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestImportGuard_RuntimeNeverImportsSystemOneRoot enforces that
// pkg/systemone/runtime never imports the root pkg/systemone package (which
// re-exports Evaluation/EvaluationError/EvaluationFrom and will later construct
// Dialect over *runtime.Runtime), nor pkg/secret, pkg/store, pkg/hub, or
// pkg/ruleengine.
func TestImportGuard_RuntimeNeverImportsSystemOneRoot(t *testing.T) {
	const (
		rootSystemOnePkg = "github.com/GoogleCloudPlatform/scion/pkg/systemone"
		forbiddenSecret  = "github.com/GoogleCloudPlatform/scion/pkg/secret"
		forbiddenStore   = "github.com/GoogleCloudPlatform/scion/pkg/store"
		forbiddenHub     = "github.com/GoogleCloudPlatform/scion/pkg/hub"
		forbiddenEngine  = "github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	)

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)
		for _, imp := range af.Imports {
			pkgPath, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if pkgPath == rootSystemOnePkg {
				t.Fatalf("%s must not import root %s", name, rootSystemOnePkg)
			}
			for _, forbidden := range []string{forbiddenSecret, forbiddenStore, forbiddenHub, forbiddenEngine} {
				if pkgPath == forbidden || strings.HasPrefix(pkgPath, forbidden+"/") {
					t.Fatalf("%s must not import %s", name, pkgPath)
				}
			}
		}
	}
}
