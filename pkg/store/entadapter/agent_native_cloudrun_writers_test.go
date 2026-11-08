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

package entadapter

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appliedConfigWriterKind says how a writer of agents.applied_config keeps
// the recorded native Cloud Run decision (see agent_native_cloudrun.go).
type appliedConfigWriterKind int

const (
	// writerChokePoint builds its document with appliedConfigForWrite.
	writerChokePoint appliedConfigWriterKind = iota
	// writerCreate writes a new row, always without the decision.
	writerCreate
	// writerKeyEdit edits named keys of the row's own raw document.
	writerKeyEdit
	// writerRowDerived re-marshals the row's own parsed document.
	writerRowDerived
	// writerNativeSetter is SetAgentNativeCloudRun, the only writer that
	// changes the decision.
	writerNativeSetter
)

// appliedConfigWriters lists, by enclosing function, every call of
// SetAppliedConfig or ClearAppliedConfig in this package. A new writer
// fails TestAppliedConfigWritersPreserveNativeCloudRun until it is listed
// here with how it keeps the decision.
var appliedConfigWriters = map[string]appliedConfigWriterKind{
	"CreateAgent":                writerCreate,
	"buildAgentUpdate":           writerChokePoint,
	"UpdateAgentConfig":          writerChokePoint,
	"clearRuntimeTargetOnce":     writerKeyEdit,
	"SetAgentRuntimeTarget":      writerKeyEdit,
	"setAgentNativeCloudRunOnce": writerNativeSetter,
	"applySucceededAgentInfo":    writerRowDerived,
	"backfillAgentRole":          writerRowDerived,
}

// funcCalls returns the names of the functions and methods fn calls.
func funcCalls(fn *ast.FuncDecl) map[string]bool {
	calls := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			calls[f.Name] = true
		case *ast.SelectorExpr:
			calls[f.Sel.Name] = true
		}
		return true
	})
	return calls
}

// TestAppliedConfigWritersPreserveNativeCloudRun pins the store invariant:
// every function of this package that writes agents.applied_config is
// listed in appliedConfigWriters, and each keeps the recorded native Cloud
// Run decision the way its kind says.
func TestAppliedConfigWritersPreserveNativeCloudRun(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := map[string]*ast.FuncDecl{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := funcCalls(fn)
			if calls["SetAppliedConfig"] || calls["ClearAppliedConfig"] {
				found[fn.Name.Name] = fn
			}
		}
	}
	var unlisted []string
	for name := range found {
		if _, ok := appliedConfigWriters[name]; !ok {
			unlisted = append(unlisted, name)
		}
	}
	sort.Strings(unlisted)
	assert.Empty(t, unlisted, "new applied_config writers: keep the recorded native Cloud Run decision (agent_native_cloudrun.go) and list them in appliedConfigWriters")
	for name := range appliedConfigWriters {
		assert.Contains(t, found, name, "listed writer %s does not write applied_config; remove it from appliedConfigWriters", name)
	}
	for name, fn := range found {
		kind, ok := appliedConfigWriters[name]
		if !ok {
			continue
		}
		calls := funcCalls(fn)
		switch kind {
		case writerChokePoint:
			assert.True(t, calls["appliedConfigForWrite"], "%s must build its document with appliedConfigForWrite", name)
			assert.False(t, calls["marshalAppliedConfig"], "%s must not marshal the caller's config directly", name)
		case writerCreate:
			assert.True(t, calls["marshalAppliedConfigNative"], "%s must write without the decision (marshalAppliedConfigNative)", name)
			assert.False(t, calls["marshalAppliedConfig"], "%s must not marshal the caller's config directly", name)
		case writerKeyEdit, writerNativeSetter:
			assert.False(t, calls["marshalAppliedConfig"], "%s edits keys of the row's raw document only", name)
		case writerRowDerived:
			assert.True(t, calls["parseAppliedConfig"], "%s must start from the row's own document", name)
			assert.True(t, calls["marshalAppliedConfigNative"] && calls["rowNativeCloudRun"], "%s must write the row's own exact reading of the decision", name)
			assert.False(t, calls["marshalAppliedConfig"], "%s must not marshal the parsed config directly", name)
		}
	}
}

// knownAppliedConfigDecoders lists the functions of the store packages that
// decode a stored document into an AgentAppliedConfig. An entry with no
// reason must set NativeCloudRun from rowNativeCloudRun; an entry with a
// reason never touches the field. Any other decoder must set it.
var knownAppliedConfigDecoders = map[string]string{
	"parseAppliedConfig":             "",
	"unmarshalAppliedConfigSnapshot": "",
	"AppliedConfigProfile":           "reads only Profile",
}

// structReadViolations checks every function of files that decodes into an
// AgentAppliedConfig (json.Unmarshal(x, &v)) against
// knownAppliedConfigDecoders, and returns the decoders found and the
// violations.
func structReadViolations(fset *token.FileSet, files []*ast.File) (map[string]bool, []string) {
	decoders := map[string]bool{}
	var violations []string
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for v := range appliedConfigVars(fn) {
				if !unmarshalsInto(fn, v) {
					continue
				}
				decoders[fn.Name.Name] = true
				assigned, touched := nativeFieldUse(fn, v)
				reason, known := knownAppliedConfigDecoders[fn.Name.Name]
				switch {
				case known && reason != "":
					if touched {
						violations = append(violations, fmt.Sprintf("%s: %s is listed as not touching NativeCloudRun (%s) but does", fset.Position(fn.Pos()), fn.Name.Name, reason))
					}
				case !assigned:
					violations = append(violations, fmt.Sprintf("%s: %s decodes %s without setting NativeCloudRun from rowNativeCloudRun", fset.Position(fn.Pos()), fn.Name.Name, v))
				}
			}
		}
	}
	sort.Strings(violations)
	return decoders, violations
}

// TestNoStructReadOfNativeCloudRun pins the single reading of the recorded
// decision: in the store packages, every decode of a stored document into
// an AgentAppliedConfig sets NativeCloudRun from rowNativeCloudRun (the
// exact-key reader SQL agrees with), unless it is listed as never touching
// the field. A struct decode matches keys in any case, so its own value is
// never used. A planted negative run shows the check fails when the
// assignment is missing from either decoder that must have it.
func TestNoStructReadOfNativeCloudRun(t *testing.T) {
	var names []string
	for _, pattern := range []string{"*.go", "../*.go"} {
		m, err := filepath.Glob(pattern)
		require.NoError(t, err)
		names = append(names, m...)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	decoders, violations := structReadViolations(fset, files)
	assert.Empty(t, violations)
	for name := range knownAppliedConfigDecoders {
		assert.True(t, decoders[name], "%s is a known decoder", name)
	}

	planted := `package entadapter
func parseAppliedConfig(raw string) (*store.AgentAppliedConfig, error) {
	var cfg store.AgentAppliedConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
func unmarshalAppliedConfigSnapshot(raw string) (*store.AgentAppliedConfig, error) {
	var cfg store.AgentAppliedConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
func newDecoder(raw string) bool {
	var cfg store.AgentAppliedConfig
	_ = json.Unmarshal([]byte(raw), &cfg)
	return cfg.NativeCloudRun
}
`
	pf, err := parser.ParseFile(fset, "planted.go", planted, 0)
	require.NoError(t, err)
	_, plantedViolations := structReadViolations(fset, []*ast.File{pf})
	require.Len(t, plantedViolations, 3, "each decoder without the assignment is reported")
	for i, name := range []string{"parseAppliedConfig", "unmarshalAppliedConfigSnapshot", "newDecoder"} {
		found := false
		for _, v := range plantedViolations {
			found = found || strings.Contains(v, " "+name+" ")
		}
		assert.True(t, found, "planted %d: %s is reported", i, name)
	}
}

// appliedConfigVars returns the names fn declares with type
// (store.)AgentAppliedConfig.
func appliedConfigVars(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil {
			return true
		}
		isCfg := false
		switch t := vs.Type.(type) {
		case *ast.Ident:
			isCfg = t.Name == "AgentAppliedConfig"
		case *ast.SelectorExpr:
			isCfg = t.Sel.Name == "AgentAppliedConfig"
		}
		if isCfg {
			for _, id := range vs.Names {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// unmarshalsInto reports whether fn calls json.Unmarshal(..., &v).
func unmarshalsInto(fn *ast.FuncDecl, v string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Unmarshal" {
			return true
		}
		if u, ok := call.Args[1].(*ast.UnaryExpr); ok && u.Op == token.AND {
			if id, ok := u.X.(*ast.Ident); ok && id.Name == v {
				found = true
			}
		}
		return true
	})
	return found
}

// nativeFieldUse reports whether fn assigns v.NativeCloudRun from a
// rowNativeCloudRun call, and whether fn touches v.NativeCloudRun at all.
func nativeFieldUse(fn *ast.FuncDecl, v string) (assigned, touched bool) {
	isField := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NativeCloudRun" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == v
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 && isField(x.Lhs[0]) {
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "rowNativeCloudRun" {
						assigned = true
					}
				}
			}
		case *ast.SelectorExpr:
			if isField(x) {
				touched = true
			}
		}
		return true
	})
	return assigned, touched
}
