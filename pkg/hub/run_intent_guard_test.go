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
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// lifecycleDispatchMethods are the AgentDispatcher methods that start, stop
// or remove an agent's container.
var lifecycleDispatchMethods = map[string]bool{
	"DispatchAgentStart":            true,
	"DispatchAgentStop":             true,
	"DispatchAgentRestart":          true,
	"DispatchAgentDelete":           true,
	"DispatchAgentCreate":           true,
	"DispatchAgentCreateWithGather": true,
	"DispatchAgentProvision":        true,
}

// How a function that calls a lifecycle dispatch method keeps the agent's
// run intent correct.
const (
	// intentRecorded: the function itself calls recordRunIntent (or
	// swapRunIntent).
	intentRecorded = "records"
	// intentExecutor: the function replays a dispatch whose intent was
	// recorded when it was queued, and writes none itself.
	intentExecutor = "executor"
	// intentUnchanged: the dispatch deliberately leaves intent as it is.
	intentUnchanged = "unchanged"
)

type lifecycleDispatchRule struct {
	kind string
	// recordedBy, when set for intentRecorded, names the function that
	// calls recordRunIntent on this function's behalf (for a function that
	// returns a dispatch closure).
	recordedBy string
}

// lifecycleDispatchCallers lists every non-test function in this package
// that calls a lifecycle dispatch method. A new call site has to be added
// here, with how it records run intent.
var lifecycleDispatchCallers = map[string]lifecycleDispatchRule{
	"Server.handleAgentLifecycle":          {kind: intentRecorded},
	"Server.suspendAgent":                  {kind: intentRecorded},
	"Server.handleStopAllAgents":           {kind: intentRecorded},
	"Server.autoSuspendStalledAgents":      {kind: intentRecorded},
	"Server.dispatchAgentEventHandler":     {kind: intentRecorded},
	"Server.wakeAgentForDM":                {kind: intentRecorded},
	"Server.handleExistingAgent":           {kind: intentRecorded},
	"Server.createAgentInProject":          {kind: intentRecorded},
	"deletionEngine.dispatch":              {kind: intentRecorded},
	"Server.dispatchAgentDeletions":        {kind: intentRecorded},
	"Server.handleWorkspaceSyncToFinalize": {kind: intentRecorded},
	"dispatchDeleteFailedCreate":           {kind: intentRecorded, recordedBy: "Server.cleanupFailedCreate"},

	"Server.execDispatchStart":   {kind: intentExecutor},
	"Server.execDispatchStop":    {kind: intentExecutor},
	"Server.execDispatchRestart": {kind: intentExecutor},
	"Server.execDispatchDelete":  {kind: intentExecutor},
	"Server.execDispatchCreate":  {kind: intentExecutor},
	// Reincarnation stops and restarts the container in place; the agent
	// is meant to keep running throughout.
	"Server.runReincarnationWorker": {kind: intentUnchanged},
}

// funcDeclKey names a function declaration as Recv.Name, or Name for a
// plain function.
func funcDeclKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// selectorCallNames returns the names of the selector calls (x.Name(...))
// in fn's body, including those in nested function literals.
func selectorCallNames(fd *ast.FuncDecl) map[string]int {
	names := map[string]int{}
	if fd.Body == nil {
		return names
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			names[sel.Sel.Name]++
		}
		return true
	})
	return names
}

// parseHubFuncs parses this package's non-test files and returns the calls
// made by each function declaration.
func parseHubFuncs(t *testing.T) map[string]map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	funcs := map[string]map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if f.Name.Name != "hub" {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcs[funcDeclKey(fd)] = selectorCallNames(fd)
		}
	}
	if len(funcs) == 0 {
		t.Fatalf("no functions found in package hub")
	}
	return funcs
}

// TestLifecycleDispatchCallsRecordRunIntent checks that every lifecycle
// dispatch in the hub either records the agent's run intent or is listed as
// a deliberate exception.
func TestLifecycleDispatchCallsRecordRunIntent(t *testing.T) {
	funcs := parseHubFuncs(t)

	var callers []string
	for name, calls := range funcs {
		for m := range lifecycleDispatchMethods {
			if calls[m] > 0 {
				callers = append(callers, name)
				break
			}
		}
	}
	sort.Strings(callers)

	seen := map[string]bool{}
	for _, name := range callers {
		seen[name] = true
		rule, ok := lifecycleDispatchCallers[name]
		if !ok {
			t.Errorf("%s calls a lifecycle dispatch method but is not in lifecycleDispatchCallers; record the run intent with recordRunIntent before the dispatch and add it", name)
			continue
		}
		if rule.kind != intentRecorded {
			continue
		}
		recorder := name
		if rule.recordedBy != "" {
			recorder = rule.recordedBy
		}
		if funcs[recorder]["recordRunIntent"]+funcs[recorder]["swapRunIntent"] == 0 {
			t.Errorf("%s dispatches a lifecycle operation but %s does not call recordRunIntent", name, recorder)
		}
	}
	for name := range lifecycleDispatchCallers {
		if !seen[name] {
			t.Errorf("lifecycleDispatchCallers lists %s, which no longer calls a lifecycle dispatch method; remove it", name)
		}
	}
}
