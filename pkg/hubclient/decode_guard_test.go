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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// decodeResponseAllowlist lists the hubclient functions that deliberately
// call apiclient.DecodeResponse, which returns (nil, nil) on a 204, instead
// of apiclient.DecodeRequired. Keys are "file:Receiver.Method" (or
// "file:Func"). Each entry covers exactly one DecodeResponse call in that
// function: a second call added to an allowlisted function fails the guard
// too. Every other function that decodes a response with an apiclient helper
// must use DecodeRequired, so a call that needs a body never returns a nil
// result. (Direct json.NewDecoder decodes are outside this guard.)
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

// apiclientImportPath is the import path the guard resolves in each file, so
// an aliased import of the package is still checked.
const apiclientImportPath = "github.com/GoogleCloudPlatform/scion/pkg/apiclient"

// decodeCall is one call to apiclient.DecodeResponse or DecodeRequired.
type decodeCall struct {
	key  string // "file:Receiver.Method" or "file:Func"
	kind string // "DecodeResponse" or "DecodeRequired"
	pos  token.Position
}

// TestDecodeGuard_DecodeResponseOnlyInAllowlist parses the hubclient sources
// and requires that apiclient.DecodeResponse is called only from the
// allowlisted functions, once each, and that each allowlisted function still
// calls it. A call migrated back to DecodeResponse, or a new call added with
// it, fails here and must either use DecodeRequired or be added to the
// allowlist with a reason.
func TestDecodeGuard_DecodeResponseOnlyInAllowlist(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	var calls []decodeCall
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fileCalls, err := scanDecodeCalls(fset, name, src)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, fileCalls...)
	}

	for _, problem := range checkDecodeCalls(calls, decodeResponseAllowlist) {
		t.Error(problem)
	}
}

// TestDecodeGuard_ResolvesImportAlias pins that the scan follows an aliased
// import of the apiclient package.
func TestDecodeGuard_ResolvesImportAlias(t *testing.T) {
	src := `package hubclient

import ac "` + apiclientImportPath + `"

func (s *fooService) Get() { _, _ = ac.DecodeResponse[int](nil) }
func (s *fooService) List() { _, _ = ac.DecodeRequired[int](nil) }
`
	calls, err := scanDecodeCalls(token.NewFileSet(), "foo.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(callSummaries(calls))
	want := "[foo.go:fooService.Get DecodeResponse foo.go:fooService.List DecodeRequired]"
	if got != want {
		t.Fatalf("scan = %s, want %s", got, want)
	}

	problems := checkDecodeCalls(calls, map[string]string{})
	if len(problems) != 1 || !strings.Contains(problems[0], "foo.go:fooService.Get calls apiclient.DecodeResponse") {
		t.Fatalf("problems = %q, want one for foo.go:fooService.Get", problems)
	}
}

// TestDecodeGuard_IgnoresOtherPackages pins that a selector named apiclient
// counts only when it refers to the apiclient import.
func TestDecodeGuard_IgnoresOtherPackages(t *testing.T) {
	src := `package hubclient

import apiclient "example.com/other/apiclient"

func (s *fooService) Get() { _, _ = apiclient.DecodeResponse[int](nil) }
`
	calls, err := scanDecodeCalls(token.NewFileSet(), "foo.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("scan = %v, want no calls", callSummaries(calls))
	}
}

// TestDecodeGuard_RejectsDotImport pins that a dot import of apiclient, which
// the scan cannot follow, is reported instead of silently skipped.
func TestDecodeGuard_RejectsDotImport(t *testing.T) {
	src := `package hubclient

import . "` + apiclientImportPath + `"

func (s *fooService) Get() { _, _ = DecodeResponse[int](nil) }
`
	if _, err := scanDecodeCalls(token.NewFileSet(), "foo.go", []byte(src)); err == nil {
		t.Fatal("scan of a dot import succeeded, want an error")
	}
}

// TestDecodeGuard_SecondCallInAllowlistedFunction pins that the allowlist
// covers one call per function: a second DecodeResponse call added to an
// allowlisted function is still reported.
func TestDecodeGuard_SecondCallInAllowlistedFunction(t *testing.T) {
	src := `package hubclient

import "` + apiclientImportPath + `"

func (s *fooService) List() {
	_, _ = apiclient.DecodeResponse[int](nil)
	_, _ = apiclient.DecodeResponse[int](nil)
	_, _ = apiclient.DecodeRequired[int](nil)
}
`
	calls, err := scanDecodeCalls(token.NewFileSet(), "foo.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]string{"foo.go:fooService.List": "test"}
	problems := checkDecodeCalls(calls, allow)
	if len(problems) != 1 || !strings.Contains(problems[0], "foo.go:fooService.List calls apiclient.DecodeResponse 2 times") {
		t.Fatalf("problems = %q, want one for the second call", problems)
	}

	// A single call in the same function passes.
	if problems := checkDecodeCalls(calls[1:], allow); len(problems) != 0 {
		t.Fatalf("problems = %q, want none for one call", problems)
	}
}

// scanDecodeCalls parses one source file and returns its calls to
// apiclient.DecodeResponse and apiclient.DecodeRequired, resolving the local
// name of the apiclient import. A dot import is an error because the calls
// could not be told apart from local functions.
func scanDecodeCalls(fset *token.FileSet, name string, src []byte) ([]decodeCall, error) {
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	local := ""
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != apiclientImportPath {
			continue
		}
		local = "apiclient"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	switch local {
	case "", "_":
		return nil, nil
	case ".":
		return nil, fmt.Errorf("%s: dot import of %s; the decode guard cannot check it, import it by name", name, apiclientImportPath)
	}

	var calls []decodeCall
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		key := name + ":" + funcKey(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if kind := apiclientDecodeCall(n, local); kind != "" {
				calls = append(calls, decodeCall{key: key, kind: kind, pos: fset.Position(n.Pos())})
			}
			return true
		})
	}
	return calls, nil
}

// checkDecodeCalls returns one problem for each DecodeResponse call outside
// the allowlist, each allowlisted function with more than one call, and each
// stale allowlist entry. It also reports a scan that found no DecodeRequired
// calls at all.
func checkDecodeCalls(calls []decodeCall, allowlist map[string]string) []string {
	var problems []string
	responseCalls := map[string]int{}
	requiredCalls := 0
	for _, c := range calls {
		switch c.kind {
		case "DecodeResponse":
			responseCalls[c.key]++
			if _, ok := allowlist[c.key]; !ok {
				problems = append(problems, fmt.Sprintf("%s: %s calls apiclient.DecodeResponse; use apiclient.DecodeRequired, or add it to decodeResponseAllowlist with a reason",
					c.pos, c.key))
			} else if n := responseCalls[c.key]; n > 1 {
				problems = append(problems, fmt.Sprintf("%s: %s calls apiclient.DecodeResponse %d times; the allowlist covers one call per function, so use apiclient.DecodeRequired here",
					c.pos, c.key, n))
			}
		case "DecodeRequired":
			requiredCalls++
		}
	}

	var stale []string
	for key := range allowlist {
		if responseCalls[key] == 0 {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		problems = append(problems, fmt.Sprintf("decodeResponseAllowlist entry %s no longer calls apiclient.DecodeResponse; remove it", key))
	}

	// Sanity check that the scan sees the migrated calls at all.
	if requiredCalls == 0 {
		problems = append(problems, "found no apiclient.DecodeRequired calls; the source scan is broken")
	}
	return problems
}

// callSummaries renders calls as "key kind" pairs for test messages.
func callSummaries(calls []decodeCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.key+" "+c.kind)
	}
	return out
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
// <local>.DecodeResponse[...] or <local>.DecodeRequired[...], where local is
// the file's name for the apiclient import.
func apiclientDecodeCall(n ast.Node, local string) string {
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
	if !ok || pkg.Name != local {
		return ""
	}
	switch sel.Sel.Name {
	case "DecodeResponse", "DecodeRequired":
		return sel.Sel.Name
	}
	return ""
}
