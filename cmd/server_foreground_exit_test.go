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
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubExitSequence_ClosesDecisionAuditBeforeFlushes checks the exit
// order: the decision audit writer closes before any OTel provider
// flushes, and the providers flush in reverse order of registration.
func TestHubExitSequence_ClosesDecisionAuditBeforeFlushes(t *testing.T) {
	var order []string
	flush := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline, "%s flush is bounded", name)
			order = append(order, name)
			return nil
		}
	}
	exit := &hubExitSequence{closeDecisionAudit: func(context.Context) {
		order = append(order, "decision audit")
	}}
	exit.addFlush(flush("tracer"))
	exit.addFlush(flush("meter"))

	exit.run()

	assert.Equal(t, []string{"decision audit", "meter", "tracer"}, order)
}

// TestRunServerStart_DefersHubExitSequence checks that runServerStart
// still wires the exit sequence: it defers exit.run, hands it the
// decision audit close, and registers the tracer and meter flushes with
// it rather than deferring them separately (a separate defer registered
// after exit.run would flush before the writer drains).
func TestRunServerStart_DefersHubExitSequence(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	require.NoError(t, err)

	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "runServerStart" {
			body = fn.Body
		}
	}
	require.NotNil(t, body, "runServerStart not found")

	var deferred []string
	var src strings.Builder
	ast.Inspect(body, func(n ast.Node) bool {
		if d, ok := n.(*ast.DeferStmt); ok {
			var b strings.Builder
			require.NoError(t, printer.Fprint(&b, fset, d.Call))
			deferred = append(deferred, b.String())
		}
		return true
	})
	require.NoError(t, printer.Fprint(&src, fset, body))

	assert.Contains(t, deferred, "exit.run()")
	for _, d := range deferred {
		assert.NotRegexp(t, `\b(tp|mp)\.Shutdown\b|CloseDecisionAudit`, d,
			"runServerStart defers %q outside the exit sequence", d)
	}
	for _, want := range []string{
		"hubSrv.DeferDecisionAuditClose()",
		"closeDecisionAudit: hubSrv.CloseDecisionAudit",
		"exit.addFlush(tp.Shutdown)",
		"exit.addFlush(mp.Shutdown)",
	} {
		assert.True(t, strings.Contains(src.String(), want), "runServerStart is missing %q", want)
	}
}
