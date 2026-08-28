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

// TestCreateMessageEnumeration uses go/ast to find every CreateMessage call
// site in non-test .go files under pkg/hub. Each site must appear in either
// the "stamped" set (verified to set ConversationID) or the "exempt" set
// (intentionally omits ConversationID, with a documented reason). Any
// unaccounted site causes a hard failure — this is the durable guard that
// prevents a new CreateMessage from silently missing conversation stamping
// when the S4 read-switch activates.
func TestCreateMessageEnumeration(t *testing.T) {
	// -------------------------------------------------------------------
	// Stamped sites: each of these sets ConversationID before calling
	// CreateMessage.
	// -------------------------------------------------------------------
	stamped := map[string]string{
		// handleAgentOutboundMessage: agent → user outbound message.
		"handlers_agent_messaging.go:handleAgentOutboundMessage": "Phase 5 dual-write: agent outbound DM or thread conversation",

		// handleAgentMessage direct-persist path: user/agent → agent.
		"handlers_agent_messaging.go:handleAgentMessage": "Phase 5 dual-write: user/agent → agent (authenticated sender)",

		// handleGroupMessage agent fan-out: authenticated sender → agent.
		"handlers_agent_messaging.go:handleGroupMessage:agent": "Phase 5 dual-write: group set message to agent",

		// handleGroupMessage user fan-out: authenticated sender → user.
		"handlers_agent_messaging.go:handleGroupMessage:user": "Phase 5 dual-write: group set message to user",

		// processMentions: agent mention dispatch (B15).
		"handlers_agent_messaging.go:processMentions": "B15 dual-write: agent mention dispatch conversation stamping",

		// handleBrokerInbound: external channel inbound (B15).
		"handlers_broker_inbound.go:handleBrokerInbound": "B15 dual-write: broker inbound conversation stamping",

		// sendAgentRouted primary: web chat user → agent.
		"handlers_chat_v2.go:sendAgentRouted:primary": "B15 dual-write: web chat user→agent primary message",

		// sendAgentRouted mention fan-out: web chat user → mentioned agent.
		"handlers_chat_v2.go:sendAgentRouted:mention": "B15 dual-write: web chat mention fan-out",

		// sendHumanToHuman: web chat user → user DM or thread.
		"handlers_chat_v2.go:sendHumanToHuman": "B15 dual-write: human-to-human DM/thread conversation stamping",

		// messagebroker deliverToUser: broker-delivered user messages.
		"messagebroker.go:deliverToUser": "Phase 5 dual-write: broker-delivered user message",

		// messagebroker deliverToAgent: broker-delivered agent messages.
		"messagebroker.go:deliverToAgent": "Phase 5 dual-write: broker-delivered agent message",

		// createInboxMessage: agent → user inbox notification (e.g. WAITING_FOR_INPUT).
		"notifications.go:createInboxMessage": "Phase 5 dual-write: agent→user inbox notification DM conversation",
	}

	// -------------------------------------------------------------------
	// Exempt sites: intentionally do NOT set ConversationID, with reasons.
	// -------------------------------------------------------------------
	exempt := map[string]string{
		// broadcastDirect: Broadcast messages are ephemeral fan-outs.
		// Both broker paths also skip conversation resolution for broadcasts.
		// No conversation ownership applies.
		"handlers_agent_messaging.go:broadcastDirect": "Deferred pending group-conversation model; broadcast is one-to-many with no two-party key to derive",
	}

	// Build combined set for lookup.
	accounted := make(map[string]bool, len(stamped)+len(exempt))
	for k := range stamped {
		accounted[k] = true
	}
	for k := range exempt {
		accounted[k] = true
	}

	// -------------------------------------------------------------------
	// Parse all non-test .go files under pkg/hub and find CreateMessage
	// call sites.
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

		// Walk the AST and find every CallExpr ending in CreateMessage.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isCreateMessageCall(call) {
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
		t.Fatal("found zero CreateMessage call sites — the scanner is broken")
	}

	// -------------------------------------------------------------------
	// Match each site to the accounted set. We match on file:enclosingFunc.
	// When a function contains multiple CreateMessage calls, they are
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
			// Multiple CreateMessage calls in the same function.
			// Try disambiguation using known suffix patterns.
			for i, s := range groupSites {
				key := gk.file + ":" + gk.fn
				if accounted[key] && len(groupSites) == 1 {
					matched[key] = true
					continue
				}
				// Try suffixed keys.
				found := false
				for _, suffix := range disambiguationSuffixes(gk.file, gk.fn, i, len(groupSites)) {
					candidate := gk.file + ":" + gk.fn + ":" + suffix
					if accounted[candidate] {
						matched[candidate] = true
						found = true
						break
					}
				}
				if !found {
					// Try bare key (for single-entry funcs that happened to be grouped).
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
		t.Errorf("Found %d CreateMessage call site(s) not in stamped or exempt list:\n", len(unaccounted))
		for _, u := range unaccounted {
			t.Errorf("  - %s", u)
		}
		t.Error("\nEvery CreateMessage site must be in the stamped set (sets ConversationID) " +
			"or the exempt set (documented reason for omission). " +
			"Add the new site to the appropriate list in this test.")
	}

	// Verify all expected entries were actually found.
	for key := range accounted {
		if !matched[key] {
			t.Errorf("Expected call site %q is listed but was not found in source. "+
				"The function may have been renamed, moved, or deleted.", key)
		}
	}

	t.Logf("Verified %d CreateMessage call sites: %d stamped, %d exempt",
		len(sites), len(stamped), len(exempt))
}

// isCreateMessageCall returns true if the call expression is a call to a
// function or method named CreateMessage.
func isCreateMessageCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name == "CreateMessage"
	case *ast.Ident:
		return fn.Name == "CreateMessage"
	}
	return false
}

// enclosingFuncName returns the name of the function or method that contains
// the given byte offset.
func enclosingFuncName(fset *token.FileSet, f *ast.File, offset int) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Offset
		end := fset.Position(fn.Body.End()).Offset
		if offset >= start && offset <= end {
			return fn.Name.Name
		}
	}
	return "<unknown>"
}

// disambiguationSuffixes returns candidate suffixes for multi-call functions.
// The mapping is hard-coded for known cases.
func disambiguationSuffixes(file, fn string, idx, total int) []string {
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
	case file == "messagebroker.go" && total == 2:
		if idx == 0 {
			return []string{"deliverToUser"}
		}
		return []string{"deliverToAgent"}
	}
	return nil
}

// findHubDir locates the pkg/hub directory relative to the test file.
func findHubDir(t *testing.T) string {
	t.Helper()
	// The test runs from pkg/hub, so "." should work, but use an absolute path
	// for robustness.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	// Verify this looks like the hub package.
	if _, err := os.Stat(filepath.Join(wd, "server.go")); err != nil {
		t.Fatalf("working directory %q does not look like pkg/hub (server.go not found)", wd)
	}
	return wd
}

// itoa is a minimal int-to-string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
