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
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerWiresInstanceID checks that server_foreground.go exports metrics
// and traces with the hub server's per-process instance ID. Both providers
// must take their options from one hubTelemetryIdentity built from hubSrv,
// so they carry the instance ID the hub uses for dispatch claims and broker
// affinity, and the same one on both signals. TestHubTelemetryIdentity checks
// that those options set service.instance.id to that ID. The identity must
// not be reassigned, or have a field changed, after it is built.
func TestServerWiresInstanceID(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	require.NoError(t, err)

	show := func(n ast.Node) string {
		var b strings.Builder
		require.NoError(t, printer.Fprint(&b, fset, n))
		return b.String()
	}

	// The identity variable each provider's options come from, and the
	// sources the identity variables are built from.
	identitySources := map[string]string{}
	providerOptions := map[string][]string{}
	// Every statement that writes to a variable or to one of its fields,
	// keyed by the variable's name.
	writes := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IncDecStmt:
			if root := rootIdent(n.X); root != "" {
				writes[root] = append(writes[root], show(n))
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if root := rootIdent(lhs); root != "" {
					writes[root] = append(writes[root], show(n))
				}
			}
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			call, ok := n.Rhs[0].(*ast.CallExpr)
			if ok && show(call.Fun) == "newHubTelemetryIdentity" && len(call.Args) > 0 {
				identitySources[show(n.Lhs[0])] = show(call.Args[0])
			}
		case *ast.CallExpr:
			fun := show(n.Fun)
			if fun != "hubmetrics.NewMeterProvider" && fun != "hubtracing.NewTracerProvider" {
				return true
			}
			var opts string
			if n.Ellipsis.IsValid() && len(n.Args) > 0 {
				opts = show(n.Args[len(n.Args)-1])
			}
			providerOptions[fun] = append(providerOptions[fun], opts)
		}
		return true
	})

	want := map[string]string{
		"hubmetrics.NewMeterProvider":  "metricsOptions",
		"hubtracing.NewTracerProvider": "tracingOptions",
	}
	used := map[string]bool{}
	for fun, method := range want {
		calls := providerOptions[fun]
		require.NotEmpty(t, calls, "no %s call found in server_foreground.go", fun)
		for _, opts := range calls {
			recv, ok := strings.CutSuffix(opts, "."+method+"()")
			if !assert.True(t, ok, "%s must be passed <identity>.%s()..., got %q", fun, method, opts) {
				continue
			}
			src, ok := identitySources[recv]
			if assert.True(t, ok, "%s options come from %q, which is not built by newHubTelemetryIdentity", fun, recv) {
				assert.Equal(t, "hubSrv", src, "the telemetry identity must be built from the hub server")
			}
			used[recv] = true
		}
	}
	assert.Len(t, used, 1, "metrics and traces must share one telemetry identity, got %v", used)
	for recv := range used {
		// The one write allowed is the newHubTelemetryIdentity assignment.
		assert.Len(t, writes[recv], 1,
			"the telemetry identity %q must not be changed after it is built, got writes %q", recv, writes[recv])
	}
}

// rootIdent returns the name of the variable that an assignment target
// writes to, following field selectors, index expressions, dereferences and
// parentheses (so "id.instanceID", "id" and "(*id).x" all give "id"). It
// returns "" for targets with no named root, such as "_".
func rootIdent(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			if x.Name == "_" {
				return ""
			}
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return ""
		}
	}
}
