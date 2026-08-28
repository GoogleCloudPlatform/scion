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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishUserMessageEnumeration uses go/ast to find every
// PublishUserMessage call site (arity 2 — the event publish signature) in
// non-test .go files under pkg/hub. Each site must appear in the "guarded"
// set (the call only executes when CreateMessage has already succeeded).
// Broker proxy calls (arity 4) are excluded from the population entirely —
// persistence is handled by the deliverToUser callback, not by the caller.
// Any unaccounted site causes a hard failure — this is the durable guard for
// contract B11/B13: publishing an unpersisted message is not legal.
func TestPublishUserMessageEnumeration(t *testing.T) {
	// -------------------------------------------------------------------
	// Guarded sites: each of these only executes after CreateMessage has
	// succeeded (error return, else-branch, or conditional block).
	// -------------------------------------------------------------------
	guarded := map[string]string{
		// createInboxMessage: CreateMessage error triggers early return
		// before publish.
		"notifications.go:createInboxMessage": "CreateMessage error triggers early return before publish",

		// sendAgentRouted primary: CreateMessage error triggers early return
		// before publish.
		"handlers_chat_v2.go:sendAgentRouted:primary": "CreateMessage error triggers early return before publish",

		// sendAgentRouted mention fan-out: publish in else branch of
		// CreateMessage error check.
		"handlers_chat_v2.go:sendAgentRouted:mention": "Publish in else branch of CreateMessage error check",

		// sendHumanToHuman: CreateMessage error triggers early return
		// before publish.
		"handlers_chat_v2.go:sendHumanToHuman": "CreateMessage error triggers early return before publish",

		// deliverToUser: publish inside if persistOK block.
		"messagebroker.go:deliverToUser": "Publish inside if persistOK block",

		// handleBrokerInbound: publish in else branch of CreateMessage
		// error check.
		"handlers_broker_inbound.go:handleBrokerInbound": "Publish in else branch of CreateMessage error check",

		// handleAgentOutboundMessage: CreateMessage error triggers early
		// return before publish.
		"handlers_agent_messaging.go:handleAgentOutboundMessage": "CreateMessage error triggers early return before publish",

		// handleAgentMessage: publish inside if persistedMsgID != empty
		// block.
		"handlers_agent_messaging.go:handleAgentMessage": "Publish inside if persistedMsgID != empty block",

		// handleGroupMessage agent fan-out: publish in else branch of
		// CreateMessage error check.
		"handlers_agent_messaging.go:handleGroupMessage:agent": "Publish in else branch of CreateMessage error check",

		// handleGroupMessage user fan-out: publish in else branch of
		// CreateMessage error check.
		"handlers_agent_messaging.go:handleGroupMessage:user": "Publish in else branch of CreateMessage error check",

		// processMentions: publish inside if persisted block.
		"handlers_agent_messaging.go:processMentions": "Publish inside if persisted block",
	}

	// Build accounted set from guarded entries.
	accounted := make(map[string]bool, len(guarded))
	for k := range guarded {
		accounted[k] = true
	}

	// -------------------------------------------------------------------
	// Parse all non-test .go files under pkg/hub and find
	// PublishUserMessage call sites.
	// -------------------------------------------------------------------
	hubDir := findHubDir(t)
	fset := token.NewFileSet()

	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}

	type callSite struct {
		file     string // base filename
		line     int
		funcName string // enclosing function name
	}
	var sites []callSite

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fullPath := filepath.Join(hubDir, name)
		f, err := parser.ParseFile(fset, fullPath, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}

		// Walk the AST and find every CallExpr ending in
		// PublishUserMessage.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isPublishUserMessageCall(call) {
				return true
			}
			pos := fset.Position(call.Pos())
			funcName := enclosingFuncName(fset, f, pos.Offset)
			sites = append(sites, callSite{
				file:     name,
				line:     pos.Line,
				funcName: funcName,
			})
			return true
		})
	}

	if len(sites) == 0 {
		t.Fatal("found zero PublishUserMessage call sites — the scanner is broken")
	}

	// -------------------------------------------------------------------
	// Match each site to the accounted set. We match on file:enclosingFunc.
	// When a function contains multiple PublishUserMessage calls, they are
	// disambiguated by a suffix heuristic.
	// -------------------------------------------------------------------

	// Group sites by file:func to detect duplicates.
	type groupKey struct{ file, fn string }
	grouped := make(map[groupKey][]callSite)
	for _, s := range sites {
		k := groupKey{s.file, s.funcName}
		grouped[k] = append(grouped[k], s)
	}

	// For each site, build a lookup key and check membership.
	var unaccounted []string
	matched := make(map[string]bool)

	for gk, groupSites := range grouped {
		if len(groupSites) == 1 {
			key := gk.file + ":" + gk.fn
			if accounted[key] {
				matched[key] = true
			} else {
				unaccounted = append(unaccounted, key+
					" (line "+itoa(groupSites[0].line)+")")
			}
		} else {
			// Multiple PublishUserMessage calls in the same function.
			// Try disambiguation using known suffix patterns.
			for i, s := range groupSites {
				key := gk.file + ":" + gk.fn
				if accounted[key] && len(groupSites) == 1 {
					matched[key] = true
					continue
				}
				// Try suffixed keys.
				found := false
				for _, suffix := range publishDisambiguationSuffixes(gk.file, gk.fn, i, len(groupSites)) {
					candidate := gk.file + ":" + gk.fn + ":" + suffix
					if accounted[candidate] {
						matched[candidate] = true
						found = true
						break
					}
				}
				if !found {
					// Try bare key (for single-entry funcs that
					// happened to be grouped).
					if accounted[key] && !matched[key] {
						matched[key] = true
					} else {
						unaccounted = append(unaccounted,
							gk.file+":"+gk.fn+
								" (line "+itoa(s.line)+", call #"+itoa(i+1)+")")
					}
				}
			}
		}
	}

	if len(unaccounted) > 0 {
		t.Errorf("Found %d PublishUserMessage call site(s) not in guarded list:\n", len(unaccounted))
		for _, u := range unaccounted {
			t.Errorf("  - %s", u)
		}
		t.Error("\nEvery PublishUserMessage site (event publish, arity 2) must be " +
			"in the guarded set (preceded by a successful CreateMessage). " +
			"Add the new site to the guarded list in this test.")
	}

	// Verify all expected entries were actually found.
	for key := range accounted {
		if !matched[key] {
			t.Errorf("Expected call site %q is listed but was not found in source. "+
				"The function may have been renamed, moved, or deleted.", key)
		}
	}

	t.Logf("Verified %d PublishUserMessage call sites (event publish, arity 2): %d guarded",
		len(sites), len(guarded))
}

// isPublishUserMessageCall returns true if the call expression is a call to
// PublishUserMessage with exactly 2 arguments (the event publish signature).
// The broker proxy's PublishUserMessage takes 4 arguments and is excluded —
// persistence is handled by its deliverToUser callback, not by the caller.
func isPublishUserMessageCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name == "PublishUserMessage" && len(call.Args) == 2
	case *ast.Ident:
		return fn.Name == "PublishUserMessage" && len(call.Args) == 2
	}
	return false
}

// publishDisambiguationSuffixes returns candidate suffixes for multi-call
// functions containing PublishUserMessage. The mapping is hard-coded for known
// cases.
func publishDisambiguationSuffixes(file, fn string, idx, total int) []string {
	switch {
	case file == "handlers_chat_v2.go" && fn == "sendAgentRouted" && total == 2:
		if idx == 0 {
			return []string{"primary"}
		}
		return []string{"mention"}
	case file == "handlers_agent_messaging.go" && fn == "handleGroupMessage" && total == 2:
		if idx == 0 {
			return []string{"agent"}
		}
		return []string{"user"}
	}
	return nil
}
