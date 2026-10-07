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
