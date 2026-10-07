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

// TestServerWiresInstanceID checks that every hubmetrics.NewMeterProvider
// call in server_foreground.go passes the hub server's per-process instance
// ID. hubmetrics generates a random ID when none is given, which keeps
// replicas on separate series, but only the wired ID matches the instance ID
// the hub uses for dispatch claims and broker affinity.
func TestServerWiresInstanceID(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	require.NoError(t, err)

	show := func(n ast.Node) string {
		var b strings.Builder
		require.NoError(t, printer.Fprint(&b, fset, n))
		return b.String()
	}

	var calls []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if show(call.Fun) == "hubmetrics.NewMeterProvider" {
			calls = append(calls, call)
		}
		return true
	})
	require.NotEmpty(t, calls, "no hubmetrics.NewMeterProvider call found in server_foreground.go")

	for _, call := range calls {
		var args []string
		for _, a := range call.Args {
			args = append(args, show(a))
		}
		assert.Contains(t, args, "hubmetrics.WithInstanceID(hubSrv.InstanceID())",
			"hubmetrics.NewMeterProvider at %s must pass hubmetrics.WithInstanceID(hubSrv.InstanceID())",
			fset.Position(call.Pos()))
	}
}
