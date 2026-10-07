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

package hubclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// decodeResponseAllowlist lists the hubclient functions that deliberately
// call apiclient.DecodeResponse, which returns (nil, nil) on a 204, instead
// of apiclient.DecodeRequired. Keys are "file:Receiver.Method" (or
// "file:Func"). Every other function that decodes a response must use
// DecodeRequired, so a call that needs a body never returns a nil result.
var decodeResponseAllowlist = map[string]string{
	// Empty-list fallbacks: a nil result is mapped to an empty list.
	"conversations.go:conversationService.List":                         "nil result maps to an empty list",
	"conversations.go:conversationService.ListMessages":                 "nil result maps to an empty list",
	"messages.go:messageService.List":                                   "nil result maps to an empty list",
	"notifications.go:notificationService.List":                         "nil result maps to an empty list",
	"notifications.go:subscriptionService.List":                         "nil result maps to an empty list",
	"notifications.go:subscriptionService.BulkCreate":                   "nil result maps to an empty list",
	"notifications.go:subscriptionTemplateService.List":                 "nil result maps to an empty list",
	"gcp_service_accounts.go:gcpServiceAccountService.ListWithWarnings": "nil result maps to an empty list",

	// Already guarded with a key-specific "hub returned no content" error.
	"env.go:envService.Get":        "keeps the key-specific no-content error text",
	"secrets.go:secretService.Get": "keeps the key-specific no-content error text",

	// Treat no body as a valid outcome.
	"secrets.go:secretService.AgentSet":            "handles the 204 update response before decoding",
	"agents.go:agentService.SendOutboundMessage":   "the CLI prints a minimal confirmation on a 204",
	"agents.go:agentService.BroadcastMessage":      "the CLI prints \"Broadcast accepted.\" on a nil result",
	"workspace.go:workspaceService.FinalizeSyncTo": "sync treats a nil finalize body as empty",
	"skills.go:skillService.Resolve":               "a nil result means nothing resolved",
	"client.go:client.Health":                      "reachability probe; the body is optional",
}

// TestDecodeGuard_DecodeResponseOnlyInAllowlist parses the hubclient sources
// and requires that apiclient.DecodeResponse is called only from the
// allowlisted functions, and that each allowlisted function still calls it.
// A call migrated back to DecodeResponse, or a new call added with it, fails
// here and must either use DecodeRequired or be added to the allowlist with
// a reason.
func TestDecodeGuard_DecodeResponseOnlyInAllowlist(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	requiredCalls := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := name + ":" + funcKey(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch apiclientDecodeCall(n) {
				case "DecodeResponse":
					if _, ok := decodeResponseAllowlist[key]; !ok {
						t.Errorf("%s: %s calls apiclient.DecodeResponse; use apiclient.DecodeRequired, or add it to decodeResponseAllowlist with a reason",
							fset.Position(n.Pos()), key)
					}
					found[key] = true
				case "DecodeRequired":
					requiredCalls++
				}
				return true
			})
		}
	}

	var stale []string
	for key := range decodeResponseAllowlist {
		if !found[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("decodeResponseAllowlist entry %s no longer calls apiclient.DecodeResponse; remove it", key)
	}

	// Sanity check that the scan sees the migrated calls at all.
	if requiredCalls == 0 {
		t.Error("found no apiclient.DecodeRequired calls; the source scan is broken")
	}
}

// funcKey returns "Receiver.Method" for a method or the name for a function.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// apiclientDecodeCall returns the function name if n is a call to
// apiclient.DecodeResponse[...] or apiclient.DecodeRequired[...].
func apiclientDecodeCall(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	fun := call.Fun
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "apiclient" {
		return ""
	}
	switch sel.Sel.Name {
	case "DecodeResponse", "DecodeRequired":
		return sel.Sel.Name
	}
	return ""
}
