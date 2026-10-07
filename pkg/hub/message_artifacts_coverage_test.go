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

//go:build !no_sqlite

package hub

// ---------------------------------------------------------------------------
// Artifact references in messages (ptone/scion#3222): call-site guards.
//
// The "artifacts" metadata key is hub-reserved. It enters a message only
// through admitMessageArtifacts, at the sites in artifactAdmittingSites.
// Every agent-recipient dispatch site either is one of those or calls
// messaging.StripReservedMetadata, which removes the key.
//
// These tests read the source, so they fail when code changes, not only
// when behavior does:
//   - TestArtifactRefsStrippedAtEveryDispatchSite: every site in
//     strippedSites (which TestReservedKeyStripCoverage keeps in step with
//     the AST-discovered dispatch inventory of
//     msg_containment_callsite_test.go) must actually contain a call to
//     StripReservedMetadata or admitMessageArtifacts. A new dispatch site
//     fails TestExternalEffectCallSiteClassification until classified,
//     then TestReservedKeyStripCoverage until listed, then this test until
//     its body strips.
//   - TestArtifactRefsAdmittedOnlyAtListedSites: admitMessageArtifacts is
//     called from exactly the listed sites, nowhere else.
//   - TestArtifactRefsAdmittedFlagOnlyAtListedSites: only those sites set
//     StructuredMessage.ArtifactRefsAdmitted to anything but false.
//   - TestArtifactRefsRecordedOnlyFromAdmittedRefs: references are
//     persisted only by recordMessageArtifacts (from the admitting sites)
//     and by deliverToUser behind the ArtifactRefsAdmitted flag.
// ---------------------------------------------------------------------------

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

// artifactAdmittingSites are the only functions that may call
// admitMessageArtifacts, each with the sender whose request credential the
// read check runs under.
var artifactAdmittingSites = map[stripSiteKey]string{
	{"agent_dm_operation.go", "ExecuteAgentDM"}:                   "sending agent (outbound endpoint to an agent, handleAgentMessage agent fork, mention fan-out with hub-built metadata)",
	{"handlers_agent_messaging.go", "handleAgentMessage"}:         "sending user or caller on the non-agent branch",
	{"handlers_agent_messaging.go", "handleAgentOutboundMessage"}: "sending agent, to a user or conversation",
	{"handlers_chat_v2.go", "sendAgentRouted"}:                    "sending web chat user",
}

// artifactRefRecorders are the only functions that may call
// recordMessageArtifacts (persisting admitted refs right after the row is
// created) or the broker's recordArtifactRefs hook.
var artifactRefRecorders = map[string]map[stripSiteKey]bool{
	"recordMessageArtifacts": {
		{"agent_dm_operation.go", "ExecuteAgentDM"}:                   true,
		{"handlers_agent_messaging.go", "handleAgentMessage"}:         true,
		{"handlers_agent_messaging.go", "handleAgentOutboundMessage"}: true,
		{"handlers_chat_v2.go", "sendAgentRouted"}:                    true,
	},
	"recordArtifactRefs": {
		{"messagebroker.go", "deliverToUser"}: true,
	},
	"AddMessageRefs": {
		{"message_artifacts.go", "recordMessageArtifacts"}: true,
	},
}

// hubCallSites returns, for every non-test .go file directly in pkg/hub,
// the (file, function) pairs that call one of symbols.
func hubCallSites(t *testing.T, symbols ...string) map[string]map[stripSiteKey]bool {
	t.Helper()
	want := make(map[string]bool, len(symbols))
	out := make(map[string]map[stripSiteKey]bool, len(symbols))
	for _, s := range symbols {
		want[s] = true
		out[s] = map[stripSiteKey]bool{}
	}
	forEachHubFile(t, func(name string, fset *token.FileSet, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sym := extractCallSymbol(call)
			if want[sym] {
				fn := enclosingFuncName(fset, f, fset.Position(call.Pos()).Offset)
				out[sym][stripSiteKey{name, fn}] = true
			}
			return true
		})
	})
	return out
}

func forEachHubFile(t *testing.T, fn func(name string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	dir := findHubDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		fn(name, fset, f)
	}
}

// funcCalls reports whether the function (file, function) in pkg/hub
// contains a call to any of symbols. ok is false when no such function
// exists.
func funcCalls(t *testing.T, key stripSiteKey, symbols ...string) (calls, ok bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(findHubDir(t), key.file), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", key.file, err)
	}
	for _, decl := range f.Decls {
		fd, isFn := decl.(*ast.FuncDecl)
		if !isFn || fd.Name.Name != key.function || fd.Body == nil {
			continue
		}
		ok = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, isCall := n.(*ast.CallExpr); isCall {
				for _, s := range symbols {
					if extractCallSymbol(call) == s {
						calls = true
					}
				}
			}
			return !calls
		})
	}
	return calls, ok
}

func TestArtifactRefsStrippedAtEveryDispatchSite(t *testing.T) {
	if len(strippedSites) == 0 {
		t.Fatal("strippedSites is empty: scanner or table broken")
	}
	for key := range strippedSites {
		calls, ok := funcCalls(t, key, "StripReservedMetadata", "admitMessageArtifacts")
		if !ok {
			t.Errorf("%s:%s listed in strippedSites but not found", key.file, key.function)
			continue
		}
		if !calls {
			t.Errorf("%s:%s dispatches to an agent but calls neither messaging.StripReservedMetadata "+
				"nor admitMessageArtifacts: client-supplied artifact references would reach the recipient",
				key.file, key.function)
		}
	}
	// The admitting sites must strip too (admitMessageArtifacts strips
	// first): check the helper itself.
	if calls, ok := funcCalls(t, stripSiteKey{"message_artifacts.go", "admitMessageArtifacts"}, "StripReservedMetadata"); !ok || !calls {
		t.Error("admitMessageArtifacts must call messaging.StripReservedMetadata before re-adding admitted refs")
	}
}

func TestArtifactRefsAdmittedOnlyAtListedSites(t *testing.T) {
	got := hubCallSites(t, "admitMessageArtifacts")["admitMessageArtifacts"]
	if len(got) == 0 {
		t.Fatal("found no admitMessageArtifacts call sites: scanner broken")
	}
	for key := range got {
		if _, ok := artifactAdmittingSites[key]; !ok {
			t.Errorf("admitMessageArtifacts called from unlisted %s:%s; artifact refs may enter messages only at artifactAdmittingSites", key.file, key.function)
		}
	}
	for key := range artifactAdmittingSites {
		if !got[key] {
			t.Errorf("STALE artifactAdmittingSites entry %s:%s: no admitMessageArtifacts call", key.file, key.function)
		}
	}
}

func TestArtifactRefsAdmittedFlagOnlyAtListedSites(t *testing.T) {
	var found []string
	forEachHubFile(t, func(name string, fset *token.FileSet, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "ArtifactRefsAdmitted" {
						continue
					}
					if i < len(x.Rhs) {
						if id, ok := x.Rhs[i].(*ast.Ident); ok && id.Name == "false" {
							continue
						}
					}
					fn := enclosingFuncName(fset, f, fset.Position(x.Pos()).Offset)
					found = append(found, name+":"+fn)
					if _, ok := artifactAdmittingSites[stripSiteKey{name, fn}]; !ok {
						t.Errorf("ArtifactRefsAdmitted set outside an admitting site at %s:%s", name, fn)
					}
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "ArtifactRefsAdmitted" {
					fn := enclosingFuncName(fset, f, fset.Position(x.Pos()).Offset)
					t.Errorf("ArtifactRefsAdmitted set in a composite literal at %s:%s; set it only from admitMessageArtifacts' result", name, fn)
				}
			}
			return true
		})
	})
	if len(found) == 0 {
		t.Fatal("found no ArtifactRefsAdmitted assignments: scanner broken")
	}
}

func TestArtifactRefsRecordedOnlyFromAdmittedRefs(t *testing.T) {
	symbols := make([]string, 0, len(artifactRefRecorders))
	for s := range artifactRefRecorders {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	got := hubCallSites(t, symbols...)
	for _, sym := range symbols {
		allowed := artifactRefRecorders[sym]
		if len(got[sym]) == 0 {
			t.Errorf("found no %s call sites: scanner broken", sym)
		}
		for key := range got[sym] {
			if !allowed[key] {
				t.Errorf("%s called from unlisted %s:%s", sym, key.file, key.function)
			}
		}
		for key := range allowed {
			if !got[sym][key] {
				t.Errorf("STALE %s entry %s:%s", sym, key.file, key.function)
			}
		}
	}
}

// TestArtifactRefsAdmittedAfterAuthzInChatV2: sendAgentRouted admits
// references only after its message authorization, so a denied send does
// no artifact lookups.
func TestArtifactRefsAdmittedAfterAuthzInChatV2(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(findHubDir(t), "handlers_chat_v2.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var authz, admit []token.Pos
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "sendAgentRouted" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch extractCallSymbol(call) {
				case "authorizeAgentMessage":
					authz = append(authz, call.Pos())
				case "admitMessageArtifacts":
					admit = append(admit, call.Pos())
				}
			}
			return true
		})
	}
	if len(authz) == 0 || len(admit) != 1 {
		t.Fatalf("sendAgentRouted: found %d authorizeAgentMessage and %d admitMessageArtifacts calls; want >=1 and exactly 1", len(authz), len(admit))
	}
	if admit[0] < authz[0] {
		t.Errorf("sendAgentRouted admits artifact references (line %d) before authorizing the send (line %d)",
			fset.Position(admit[0]).Line, fset.Position(authz[0]).Line)
	}
}

// TestArtifactRefResolverReachableOnlyFromRequestContexts pins the
// in-process path to the artifact service that message references add:
// artifactRefResolver is called only by resolveArtifactRefs, which resolves
// only when the authentication middleware's credential context binds ctx's
// current identity; and that credential context is recorded only by the
// authentication middleware files, so in-process code cannot satisfy the
// check.
func TestArtifactRefResolverReachableOnlyFromRequestContexts(t *testing.T) {
	got := hubCallSites(t, "artifactRefResolver", "contextWithCredentialContext")

	wantResolver := map[stripSiteKey]bool{{"message_artifacts.go", "resolveArtifactRefs"}: true}
	if len(got["artifactRefResolver"]) == 0 {
		t.Fatal("found no artifactRefResolver call sites: scanner broken")
	}
	for key := range got["artifactRefResolver"] {
		if !wantResolver[key] {
			t.Errorf("artifactRefResolver called from %s:%s; only resolveArtifactRefs may build the in-process artifact service", key.file, key.function)
		}
	}

	authFiles := map[string]bool{"auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true}
	if len(got["contextWithCredentialContext"]) == 0 {
		t.Fatal("found no contextWithCredentialContext call sites: scanner broken")
	}
	for key := range got["contextWithCredentialContext"] {
		if !authFiles[key.file] {
			t.Errorf("contextWithCredentialContext called from %s:%s; only authentication middleware may record a request credential context (resolveArtifactRefs relies on it)", key.file, key.function)
		}
	}

	if calls, ok := funcCalls(t, stripSiteKey{"message_artifacts.go", "resolveArtifactRefs"}, "requestCredentialBindsIdentity"); !ok || !calls {
		t.Error("resolveArtifactRefs must check that the request credential binds the current identity before resolving")
	}
}

// TestCredentialSubjectKeyOnlyInIdentityAndAuthFiles widens the setter pin
// above to every reference: the credential subject key and the
// contextWithCredentialContext function may be named (called, passed as a
// value, or used as a key) only in identity.go, where they are defined, and
// in the three authentication middleware files. Anything else could record
// or overwrite the identity a request credential is bound to.
func TestCredentialSubjectKeyOnlyInIdentityAndAuthFiles(t *testing.T) {
	allowed := map[string]bool{"identity.go": true, "auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true}
	names := map[string]bool{"credentialSubjectContextKey": true, "contextWithCredentialContext": true}
	found := map[string]int{}
	forEachHubFile(t, func(name string, fset *token.FileSet, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !names[id.Name] {
				return true
			}
			found[id.Name]++
			if !allowed[name] {
				t.Errorf("%s referenced at %s; only identity.go and the authentication middleware files may name it",
					id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	})
	for n := range names {
		if found[n] == 0 {
			t.Errorf("found no references to %s: scanner broken", n)
		}
	}
}

// firstCallOffset returns the byte offset in file of the first call to
// symbol in the body of function, or -1. Offsets, not token.Pos values, so
// results from separate parses of the same file compare correctly.
func firstCallOffset(t *testing.T, file, function, symbol string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(findHubDir(t), file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	off := -1
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != function || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && extractCallSymbol(call) == symbol {
				if o := fset.Position(call.Pos()).Offset; off < 0 || o < off {
					off = o
				}
			}
			return true
		})
	}
	return off
}

// TestArtifactRefsAdmittedAfterAuthzAtEverySite extends the chat v2 order
// guard to the other three admitting sites, so a denied send never does
// artifact lookups:
//   - ExecuteAgentDM: authorizeAgentMessage runs before admission in the
//     same function.
//   - handleAgentOutboundMessage: resolveOutboundRouting (sender ownership
//     and conversation authorization for the outbound request) runs before
//     admission; the agent-recipient branch delegates to ExecuteAgentDM.
//   - handleAgentMessage has no authorization step of its own: each of its
//     callers authorizes the request (authorizeAgentMessage or
//     authorizeAgentTargetAction) before calling it, so the guard checks
//     every caller instead. In handleAgentAction the target check is
//     skipped only for self access (an agent acting on itself); there the
//     sender reads artifacts under its own credential anyway.
func TestArtifactRefsAdmittedAfterAuthzAtEverySite(t *testing.T) {
	inFunc := []struct{ file, function, authz string }{
		{"agent_dm_operation.go", "ExecuteAgentDM", "authorizeAgentMessage"},
		{"handlers_agent_messaging.go", "handleAgentOutboundMessage", "resolveOutboundRouting"},
		{"handlers_chat_v2.go", "sendAgentRouted", "authorizeAgentMessage"},
	}
	for _, c := range inFunc {
		authz := firstCallOffset(t, c.file, c.function, c.authz)
		admit := firstCallOffset(t, c.file, c.function, "admitMessageArtifacts")
		if authz < 0 || admit < 0 {
			t.Errorf("%s:%s: %s or admitMessageArtifacts not found", c.file, c.function, c.authz)
			continue
		}
		if admit < authz {
			t.Errorf("%s:%s admits artifact references (offset %d) before %s (offset %d)",
				c.file, c.function, admit, c.authz, authz)
		}
	}

	callers := hubCallSites(t, "handleAgentMessage")["handleAgentMessage"]
	if len(callers) == 0 {
		t.Fatal("found no handleAgentMessage callers: scanner broken")
	}
	for key := range callers {
		call := firstCallOffset(t, key.file, key.function, "handleAgentMessage")
		authz := -1
		for _, sym := range []string{"authorizeAgentMessage", "authorizeAgentTargetAction"} {
			if o := firstCallOffset(t, key.file, key.function, sym); o >= 0 && (authz < 0 || o < authz) {
				authz = o
			}
		}
		if authz < 0 || call < authz {
			t.Errorf("%s:%s calls handleAgentMessage without authorizing the request first; handleAgentMessage admits artifact references and has no authorization step of its own",
				key.file, key.function)
		}
	}
}
