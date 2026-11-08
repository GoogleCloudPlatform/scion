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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerSendMethods are the RuntimeBrokerClient methods that send a create
// or start request (and its resolved env) to a broker, with their argument
// counts, which tell them apart from same-named store methods.
// TestDispatchSendSitesSealMetadataEnv reads both from the interface and
// fails if they drift, or if an interface method that takes a resolved env
// (map[string]string) or a create request is not listed here.
var brokerSendMethods = map[string]int{
	"CreateAgent":           4,
	"CreateAgentWithGather": 4,
	"StartAgent":            18,
	"RestartAgent":          7,
}

// sealedSendWrappers are the only functions allowed to call a broker send
// method from dispatch code; each calls its seal before its send.
var sealedSendWrappers = map[string]string{
	"sendCreate":           "sealCreateRequest",
	"sendCreateWithGather": "sealCreateRequest",
	"sendStart":            "sealDispatchMetadataEnv",
	"sendRestart":          "sealDispatchMetadataEnv",
}

// brokerTransportTypes are the RuntimeBrokerClient implementations whose
// same-named methods delegate a send to the next transport, through one of
// brokerTransportFields.
var brokerTransportTypes = map[string]bool{
	"AuthenticatedBrokerClient":  true,
	"HTTPRuntimeBrokerClient":    true,
	"HybridBrokerClient":         true,
	"ControlChannelBrokerClient": true,
}

// brokerTransportFields are the fields a transport delegates through.
var brokerTransportFields = map[string]bool{"transport": true, "httpClient": true, "controlChannel": true}

// sealRecvTypeName returns the receiver type name of fn, or "".
func sealRecvTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// sealSiteReport is what findSealBypasses found.
type sealSiteReport struct {
	bypass []string // send calls outside the allowed set
	sends  int      // send calls inside wrappers
	sealed map[string]bool
}

// findSealBypasses checks every function of files against sendMethods
// (name to argument count). A broker send call is allowed only in a sealed
// send wrapper, after its seal call, or in a same-named method of a
// transport type that delegates through a transport field. Every other
// send call, a method expression (RuntimeBrokerClient.StartAgent(c, ...)),
// and any other reference to a send method outside the transport types (a
// method value, through any base) is a bypass.
//
// Two things are not checked here, the wrappers being four lines each:
// a transport method that changes the env or request before it delegates,
// and a seal that runs on a different value or only on some paths before
// the send (the order check is lexical).
func findSealBypasses(fset *token.FileSet, files []*ast.File, sendMethods map[string]int) sealSiteReport {
	rep := sealSiteReport{sealed: map[string]bool{}}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			seal, isWrapper := sealedSendWrappers[fn.Name.Name]
			sealPos := token.NoPos
			calls := map[*ast.SelectorExpr]bool{}
			transport := brokerTransportTypes[sealRecvTypeName(fn)]
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, isIdent := call.Fun.(*ast.Ident); isIdent && isWrapper && id.Name == seal && sealPos == token.NoPos {
					sealPos = call.Pos()
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				arity, isSend := sendMethods[sel.Sel.Name]
				if !isSend {
					return true
				}
				calls[sel] = true
				if len(call.Args) != arity {
					// A method expression takes the receiver first; its type
					// may be written (*T) or (T).
					if id, ok := unwrapTypeExpr(sel.X).(*ast.Ident); ok && len(call.Args) == arity+1 && (id.Name == "RuntimeBrokerClient" || brokerTransportTypes[id.Name]) && !transport {
						rep.bypass = append(rep.bypass, fmt.Sprintf("%s: %s calls %s through a method expression", fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name))
					}
					// Otherwise another API of the same name (a store method).
					return true
				}
				pos := fset.Position(call.Pos())
				switch {
				case isWrapper:
					rep.sends++
					if sealPos == token.NoPos || sealPos > call.Pos() {
						rep.bypass = append(rep.bypass, fmt.Sprintf("%s: %s sends before %s", pos, fn.Name.Name, seal))
					} else {
						rep.sealed[fn.Name.Name] = true
					}
				case transport && fn.Name.Name == sel.Sel.Name && isTransportField(sel.X):
					// A transport implementation delegating downwards.
				default:
					rep.bypass = append(rep.bypass, fmt.Sprintf("%s: %s calls %s", pos, fn.Name.Name, sel.Sel.Name))
				}
				return true
			})
			// A send method named other than as a call (a method value,
			// through any base) is a bypass outside the transports.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || calls[sel] || transport {
					return true
				}
				if _, isSend := sendMethods[sel.Sel.Name]; isSend {
					rep.bypass = append(rep.bypass, fmt.Sprintf("%s: %s takes %s as a value", fset.Position(sel.Pos()), fn.Name.Name, sel.Sel.Name))
				}
				return true
			})
		}
	}
	sort.Strings(rep.bypass)
	return rep
}

// unwrapTypeExpr strips parentheses and a pointer star from a method
// expression's type, so (*T) and (T) read as T.
func unwrapTypeExpr(x ast.Expr) ast.Expr {
	for {
		switch e := x.(type) {
		case *ast.ParenExpr:
			x = e.X
		case *ast.StarExpr:
			x = e.X
		default:
			return x
		}
	}
}

// isTransportField reports whether x is a field selector naming a
// transport field or the dispatcher's client (x.transport, x.client, ...).
func isTransportField(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && (brokerTransportFields[sel.Sel.Name] || sel.Sel.Name == "client")
}

// TestDispatchSendSitesSealMetadataEnv enumerates every call of a broker
// send method in this package and in every area moved out of it
// (findSealBypasses). A new send site that bypasses the seal fails here.
func TestDispatchSendSitesSealMetadataEnv(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// Reads the non-test Go files of pkg/hub and of every guardScanSubdirs
	// directory (globGuardScanDirs), so a send site moved out of package hub
	// is still checked.
	names, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
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
	fromInterface := envSendMethods(t, files)
	assert.Equal(t, brokerSendMethods, fromInterface, "brokerSendMethods must list every RuntimeBrokerClient method that takes a resolved env or a create request, with its argument count")
	rep := findSealBypasses(fset, files, fromInterface)
	assert.Empty(t, rep.bypass, "broker sends must go through a sealed send wrapper (dispatch_metadata_seal.go)")
	assert.Equal(t, len(sealedSendWrappers), rep.sends, "each wrapper makes exactly one send")
	for name := range sealedSendWrappers {
		assert.True(t, rep.sealed[name], "%s must seal before it sends", name)
	}
}

// The checker itself catches an unsealed caller of a send method that
// shares its name but has another receiver, a wrapper that sends before it
// seals, a transport delegating through a non-transport value, and a send
// method taken as a value.
func TestFindSealBypassesCatchesBypasses(t *testing.T) {
	args18 := strings.TrimSuffix(strings.Repeat("nil, ", 18), ", ")
	cases := map[string]string{
		"same name on another receiver": `func (d *HTTPAgentDispatcher) StartAgent() { d.client.StartAgent(` + args18 + `) }`,
		"send before seal":              `func (d *HTTPAgentDispatcher) sendRestart() { d.client.RestartAgent(nil, nil, nil, nil, nil, nil, nil); sealDispatchMetadataEnv(nil, nil, nil, nil) }`,
		"transport through a local":     `func (c *HybridBrokerClient) CreateAgent() { other.CreateAgent(nil, nil, nil, nil) }`,
		"method value":                  `func (d *HTTPAgentDispatcher) helper() { f := d.client.CreateAgent; _ = f }`,
		"method value through a local":  `func (d *HTTPAgentDispatcher) helper() { c := d.client; f := c.StartAgent; _ = f }`,
		"method expression":             `func (d *HTTPAgentDispatcher) helper(c RuntimeBrokerClient) { RuntimeBrokerClient.StartAgent(c, ` + args18 + `) }`,
		"pointer method expression":     `func (d *HTTPAgentDispatcher) helper(c *HybridBrokerClient) { (*HybridBrokerClient).StartAgent(c, ` + args18 + `) }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", "package hub\n"+body+"\n", 0)
			require.NoError(t, err)
			assert.NotEmpty(t, findSealBypasses(fset, []*ast.File{f}, brokerSendMethods).bypass)
		})
	}
	fset := token.NewFileSet()
	ok, err := parser.ParseFile(fset, "ok.go", `package hub
func (d *HTTPAgentDispatcher) sendRestart() { sealDispatchMetadataEnv(nil, nil, nil, nil); d.client.RestartAgent(nil, nil, nil, nil, nil, nil, nil) }
func (c *HybridBrokerClient) RestartAgent() { c.httpClient.RestartAgent(nil, nil, nil, nil, nil, nil, nil) }
`, 0)
	require.NoError(t, err)
	assert.Empty(t, findSealBypasses(fset, []*ast.File{ok}, brokerSendMethods).bypass, "a sealed wrapper and a transport delegation pass")
}

// envSendMethods reads the RuntimeBrokerClient interface from files and
// returns its methods that take a map[string]string (a resolved env) or a
// *RemoteCreateAgentRequest, with their argument counts.
func envSendMethods(t *testing.T, files []*ast.File) map[string]int {
	t.Helper()
	out := map[string]int{}
	found := false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "RuntimeBrokerClient" {
				return true
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			found = true
			for _, m := range iface.Methods.List {
				ft, ok := m.Type.(*ast.FuncType)
				if !ok || len(m.Names) == 0 {
					continue
				}
				args, carriesEnv := 0, false
				for _, p := range ft.Params.List {
					n := len(p.Names)
					if n == 0 {
						n = 1
					}
					args += n
					switch pt := p.Type.(type) {
					case *ast.MapType:
						k, kok := pt.Key.(*ast.Ident)
						v, vok := pt.Value.(*ast.Ident)
						carriesEnv = carriesEnv || (kok && vok && k.Name == "string" && v.Name == "string")
					case *ast.StarExpr:
						if id, ok := pt.X.(*ast.Ident); ok && id.Name == "RemoteCreateAgentRequest" {
							carriesEnv = true
						}
					}
				}
				if carriesEnv {
					out[m.Names[0].Name] = args
				}
			}
			return false
		})
	}
	require.True(t, found, "the RuntimeBrokerClient interface is in this package")
	return out
}
