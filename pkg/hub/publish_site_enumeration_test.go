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
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPersistedRowEffectEnumeration uses go/ast to find every call site of
// externally visible effects that require a persisted message row in non-test
// .go files under pkg/hub. Each site must appear in the "guarded" set (the
// call only executes when CreateMessage has already succeeded). Any unaccounted
// site causes a hard failure.
//
// Effect categories in scope:
//   - SSE publish: the event publisher's PublishUserMessage (a 4-argument
//     call on an `events` field or variable). Every one must be in the
//     guarded set. Every other PublishUserMessage call must be one of the
//     broker proxy calls listed in proxyExcluded (by file, function and
//     receiver, exactly once each): persistence for those is handled by
//     the deliverToUser callback, not by the caller. A PublishUserMessage
//     selector that is not called directly (a method value or method
//     expression) is reported as unclassified. See scanPublishUserMessage.
//
// Effect categories deliberately NOT enumerated:
//   - Watermark updates (TouchDMActivity, TouchTopicActivity): currently
//     structurally guarded by early return or conditional scope in all
//     existing call sites.
//   - Attachment links (linkAttachmentRefs, LinkAttachmentToMessage):
//     currently structurally guarded by early return or conditional scope
//     in all existing call sites.
//   - Audit log entries (messageLog.Info): currently structurally guarded
//     by early return or conditional scope in all existing call sites.
//
// The latter three categories are omitted because adding them would require
// receiver-type resolution beyond go/ast's capability. The enumerated
// category is the one with externally visible user impact (SSE event
// delivery).
func TestPersistedRowEffectEnumeration(t *testing.T) {
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
		// before publish. Also covers the unreachable-default override,
		// which shares this call site.
		"handlers_chat_v2.go:sendHumanToHuman": "CreateMessage error triggers early return before publish",

		// deliverToUser: CreateMessage error triggers early return.
		"messagebroker.go:deliverToUser": "CreateMessage error triggers early return before all effects",

		// handleBrokerInbound: publish in else branch of CreateMessage
		// error check.
		"handlers_broker_inbound.go:handleBrokerInbound": "Publish in else branch of CreateMessage error check",

		// dispatchRoutedRecipient: publish in else branch of CreateMessage
		// error check.
		"handlers_broker_inbound_routed.go:dispatchRoutedRecipient": "Publish in else branch of CreateMessage error check",

		// agent_dm_operation.go:ExecuteAgentDM: the shared agent DM operation
		// (#1688). CreateMessage error triggers early return before publish.
		"agent_dm_operation.go:ExecuteAgentDM": "CreateMessage error triggers early return before publish",

		// handleAgentOutboundMessage deliveryUserDirect path: CreateMessage
		// error triggers early return before publish (only non-broker,
		// non-agent-DM recipient path remains after #1688 extraction).
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

		// applyBrokerMessageFailure (ptone/scion#1866): unlike every other
		// guarded site, this function never calls CreateMessage — it acts on
		// a row a prior request already persisted. The row's existence is
		// confirmed by the GetMessage lookup earlier in the function
		// (returns false on ErrNotFound/lookup error), and the publish is
		// reached only after s.markFailed has already written
		// dispatch_state=failed for that row (also returns false on error).
		// The publish mirrors that committed write onto the in-memory copy;
		// it is not gating persistence, persistence already happened.
		"message_delivery_failures.go:applyBrokerMessageFailure": "Acts on an already-persisted row (confirmed via GetMessage) after markFailed has already committed; publish mirrors the committed write, not a pending one",
	}

	// -------------------------------------------------------------------
	// Broker proxy calls: MessageBrokerProxy.PublishUserMessage hands the
	// message to the bus; persistence happens later in its deliverToUser
	// callback, not at the caller. Every PublishUserMessage call that is
	// not the event publish (see classifyPublishUserMessageCall) must be
	// one of these, keyed by file, enclosing function and receiver
	// expression, with the exact number of such calls expected (see
	// checkProxyCalls).
	// -------------------------------------------------------------------
	proxyExcluded := map[proxyCallKey]int{
		// bp.PublishUserMessage: broker path to a user.
		{"handlers_agent_messaging.go", "handleAgentOutboundMessage", "bp"}: 1,
		// nd.brokerProxy.PublishUserMessage: notification delivery.
		{"notifications.go", "publishToBroker", "nd.brokerProxy"}: 1,
		// p.PublishUserMessage: group fan-out to user recipients.
		{"messagebroker.go", "PublishToGroup", "p"}: 1,
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

	var sites []publishCallSite
	var proxyCalls []proxyCall
	var unclassified []string

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
		ev, px, un := scanPublishUserMessage(fset, f, name)
		sites = append(sites, ev...)
		proxyCalls = append(proxyCalls, px...)
		unclassified = append(unclassified, un...)
	}

	for _, u := range unclassified {
		t.Errorf("unclassified PublishUserMessage use at %s: a PublishUserMessage selector that is not "+
			"called directly (method value or method expression) cannot be checked; call it directly.", u)
	}
	for _, e := range checkProxyCalls(proxyCalls, proxyExcluded) {
		t.Error(e)
	}

	if len(sites) == 0 {
		t.Fatal("found zero persisted-row effect call sites — the scanner is broken")
	}

	// -------------------------------------------------------------------
	// Match each site to the accounted set. We match on file:enclosingFunc.
	// When a function contains multiple enumerated calls, they are
	// disambiguated by target kind ("publish" vs "notify") and, within the
	// same target kind, by a suffix heuristic.
	// -------------------------------------------------------------------

	// Group sites by file:func to detect duplicates.
	type groupKey struct{ file, fn string }
	grouped := make(map[groupKey][]publishCallSite)
	for _, s := range sites {
		k := groupKey{s.file, s.funcName}
		grouped[k] = append(grouped[k], s)
	}

	// For each site, build a lookup key and check membership.
	var unaccounted []string
	matched := make(map[string]bool)

	for gk, groupSites := range grouped {
		if len(groupSites) == 1 {
			// Single call in this function — try bare key first.
			key := gk.file + ":" + gk.fn
			if accounted[key] {
				matched[key] = true
			} else {
				unaccounted = append(unaccounted, key+
					" (line "+itoa(groupSites[0].line)+")")
			}
		} else {
			// Multiple enumerated calls in the same function.
			// Check if they have mixed targets (publish + notify).
			hasPublish := false
			hasNotify := false
			for _, s := range groupSites {
				switch s.target {
				case "publish":
					hasPublish = true
				case "notify":
					hasNotify = true
				}
			}
			mixedTargets := hasPublish && hasNotify

			// Sub-group by target kind for disambiguation within each kind.
			targetGroups := make(map[string][]publishCallSite)
			for _, s := range groupSites {
				targetGroups[s.target] = append(targetGroups[s.target], s)
			}

			for target, tgSites := range targetGroups {
				if len(tgSites) == 1 && mixedTargets {
					// Single call of this target kind in a mixed-target
					// function — use target as suffix.
					key := gk.file + ":" + gk.fn + ":" + target
					if accounted[key] {
						matched[key] = true
					} else {
						unaccounted = append(unaccounted, key+
							" (line "+itoa(tgSites[0].line)+")")
					}
				} else if len(tgSites) == 1 && !mixedTargets {
					// Two calls of the same target? Should not happen
					// with len(tgSites)==1, but handle bare key.
					key := gk.file + ":" + gk.fn
					if accounted[key] {
						matched[key] = true
					} else {
						unaccounted = append(unaccounted, key+
							" (line "+itoa(tgSites[0].line)+")")
					}
				} else {
					// Multiple calls of the same target kind — use
					// target-specific disambiguation suffixes.
					for i, s := range tgSites {
						found := false
						for _, suffix := range persistedRowDisambiguationSuffixes(gk.file, gk.fn, target, i, len(tgSites)) {
							candidate := gk.file + ":" + gk.fn + ":" + suffix
							if accounted[candidate] {
								matched[candidate] = true
								found = true
								break
							}
						}
						if !found {
							// Try bare key (fallback).
							key := gk.file + ":" + gk.fn
							if accounted[key] && !matched[key] {
								matched[key] = true
							} else {
								unaccounted = append(unaccounted,
									gk.file+":"+gk.fn+
										" (line "+itoa(s.line)+", "+target+" call #"+itoa(i+1)+")")
							}
						}
					}
				}
			}
		}
	}

	if len(unaccounted) > 0 {
		t.Errorf("Found %d persisted-row effect call site(s) not in guarded list:\n", len(unaccounted))
		for _, u := range unaccounted {
			t.Errorf("  - %s", u)
		}
		t.Error("\nEvery PublishUserMessage site (event publish) " +
			"must be in the guarded set " +
			"(preceded by a successful CreateMessage). " +
			"Add the new site to the guarded list in this test.")
	}

	// Verify all expected entries were actually found.
	for key := range accounted {
		if !matched[key] {
			t.Errorf("Expected call site %q is listed but was not found in source. "+
				"The function may have been renamed, moved, or deleted.", key)
		}
	}

	t.Logf("Verified %d persisted-row effect call sites: %d guarded, %d broker proxy calls excluded",
		len(sites), len(guarded), len(proxyCalls))
}

// publishCallSite is one event publish call found by the scanner.
type publishCallSite struct {
	file     string // base filename
	line     int
	funcName string // enclosing function name
	target   string // "publish" (the only enumerated target today)
}

// proxyCallKey identifies a non-event PublishUserMessage call: file,
// enclosing function and the receiver expression as written.
type proxyCallKey struct {
	file, funcName, receiver string
}

// proxyCall is one non-event PublishUserMessage call found by the scanner.
type proxyCall struct {
	key  proxyCallKey
	line int
}

// scanPublishUserMessage finds, in one parsed file, every event publish
// call (classifyPublishUserMessageCall == publishCallEvent), every other
// PublishUserMessage call with its receiver expression, and every
// PublishUserMessage selector that is not the function of a call (a method
// value such as f := s.events.PublishUserMessage, or a method expression),
// which a call-based scan could not see and so is reported as
// unclassified ("file:func (line N)").
func scanPublishUserMessage(fset *token.FileSet, f *ast.File, name string) (events []publishCallSite, proxies []proxyCall, unclassified []string) {
	calledFuns := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			// Pre-order: a call is visited before its Fun, so the selector
			// check below already knows which selectors are called.
			calledFuns[x.Fun] = true
			kind := classifyPublishUserMessageCall(x)
			if kind == publishCallNone {
				return true
			}
			pos := fset.Position(x.Pos())
			funcName := enclosingFuncName(fset, f, pos.Offset)
			if kind == publishCallEvent {
				events = append(events, publishCallSite{file: name, line: pos.Line, funcName: funcName, target: "publish"})
				return true
			}
			receiver := ""
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				receiver = types.ExprString(sel.X)
			}
			proxies = append(proxies, proxyCall{key: proxyCallKey{file: name, funcName: funcName, receiver: receiver}, line: pos.Line})
		case *ast.SelectorExpr:
			if x.Sel.Name == "PublishUserMessage" && !calledFuns[x] {
				pos := fset.Position(x.Pos())
				unclassified = append(unclassified, fmt.Sprintf("%s:%s (line %d)", name, enclosingFuncName(fset, f, pos.Offset), pos.Line))
			}
		}
		return true
	})
	return events, proxies, unclassified
}

// checkProxyCalls compares the non-event PublishUserMessage calls found
// with the expected ones (key -> exact count) and returns one error per
// difference: a call whose file, function and receiver is not listed (for
// example an aliased event publisher inside a listed function), a listed
// call that occurs a different number of times, or one that is missing.
func checkProxyCalls(found []proxyCall, expected map[proxyCallKey]int) []string {
	lines := map[proxyCallKey][]int{}
	for _, c := range found {
		lines[c.key] = append(lines[c.key], c.line)
	}
	var errs []string
	for key, ls := range lines {
		want, listed := expected[key]
		switch {
		case !listed:
			errs = append(errs, fmt.Sprintf("unclassified PublishUserMessage call at %s:%s on receiver %q (lines %v): "+
				"neither the event publish (a 4-argument call on an `events` field or variable) nor a listed broker "+
				"proxy call. Add it to guarded (as an event publish) or to proxyExcluded.", key.file, key.funcName, key.receiver, ls))
		case len(ls) != want:
			errs = append(errs, fmt.Sprintf("broker proxy call %s:%s on receiver %q occurs %d times (lines %v), "+
				"proxyExcluded expects exactly %d.", key.file, key.funcName, key.receiver, len(ls), ls, want))
		}
	}
	for key := range expected {
		if len(lines[key]) == 0 {
			errs = append(errs, fmt.Sprintf("broker proxy call %s:%s on receiver %q is listed in proxyExcluded but was "+
				"not found in source. The function may have been renamed, moved, or deleted.", key.file, key.funcName, key.receiver))
		}
	}
	sort.Strings(errs)
	return errs
}

// publishCallKind classifies a call for the persisted-row scanners.
type publishCallKind int

const (
	// publishCallNone: not a PublishUserMessage call.
	publishCallNone publishCallKind = iota
	// publishCallEvent: the event publisher's PublishUserMessage.
	publishCallEvent
	// publishCallOther: any other PublishUserMessage call. The scanner
	// requires each to be a listed broker proxy call (proxyExcluded).
	publishCallOther
)

// classifyPublishUserMessageCall classifies every call named
// PublishUserMessage. The event publish is a method call on an `events`
// field or variable (s.events, p.events, nd.events, events) with the event
// publish signature's 4 arguments (ctx, msg, attachments, artifactRefs).
// Every other PublishUserMessage call, whatever its receiver or arity, is
// publishCallOther: the broker proxy's PublishUserMessage (ctx, projectID,
// userID, msg) on bp, p or nd.brokerProxy, but equally an event publisher
// held under another name (ep), which the scanner then reports as
// unclassified rather than ignoring.
func classifyPublishUserMessageCall(call *ast.CallExpr) publishCallKind {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if fn.Name == "PublishUserMessage" {
			return publishCallOther
		}
		return publishCallNone
	case *ast.SelectorExpr:
		if fn.Sel.Name != "PublishUserMessage" {
			return publishCallNone
		}
		if len(call.Args) == 4 {
			switch recv := fn.X.(type) {
			case *ast.SelectorExpr:
				if recv.Sel.Name == "events" {
					return publishCallEvent
				}
			case *ast.Ident:
				if recv.Name == "events" {
					return publishCallEvent
				}
			}
		}
		return publishCallOther
	}
	return publishCallNone
}

// isPublishUserMessageCall reports whether call is the event publisher's
// PublishUserMessage (see classifyPublishUserMessageCall).
func isPublishUserMessageCall(call *ast.CallExpr) bool {
	return classifyPublishUserMessageCall(call) == publishCallEvent
}

// persistedRowDisambiguationSuffixes returns candidate suffixes for multi-call
// functions containing enumerated persisted-row effect calls. The target
// parameter indicates whether this is a "publish" or "notify" call. The
// mapping is hard-coded for known cases.
func persistedRowDisambiguationSuffixes(file, fn, target string, idx, total int) []string {
	switch {
	case file == "handlers_chat_v2.go" && fn == "sendAgentRouted" && target == "publish" && total == 2:
		if idx == 0 {
			return []string{"primary"}
		}
		return []string{"mention"}
	case file == "handlers_agent_messaging.go" && fn == "handleGroupMessage" && target == "publish" && total == 2:
		if idx == 0 {
			return []string{"agent"}
		}
		return []string{"user"}
	}
	return nil
}

// TestMemberFanoutFollowsPublish pins the member fan-out effect, which
// sends a stored message to members' user subjects: every call
// (fanOutThreadMessageToMembersAsync, recordThreadMembersThenFanOutAsync,
// or the broker proxy's memberFanout hook) must be in a listed function and
// come after a PublishUserMessage call in that function, so it only runs
// for a message whose persisted-row publish is already guarded by
// TestPersistedRowEffectEnumeration. A new call site fails here until it is
// reviewed and listed.
func TestMemberFanoutFollowsPublish(t *testing.T) {
	allowed := map[string]bool{
		"handlers_chat_v2.go:sendAgentRouted":                    true,
		"handlers_chat_v2.go:sendHumanToHuman":                   true,
		"handlers_agent_messaging.go:handleAgentOutboundMessage": true,
		"messagebroker.go:deliverToUser":                         true,
	}
	isFanoutCall := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "fanOutThreadMessageToMembersAsync", "recordThreadMembersThenFanOutAsync", "memberFanout":
			return true
		}
		return false
	}

	hubDir := findHubDir(t)
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}
	found := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(hubDir, name), nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}
		// Earliest PublishUserMessage offset per enclosing function.
		firstPublish := make(map[string]int)
		type fanoutSite struct {
			fn     string
			offset int
			line   int
		}
		var fanouts []fanoutSite
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			fn := enclosingFuncName(fset, f, pos.Offset)
			switch {
			case isPublishUserMessageCall(call):
				if off, seen := firstPublish[fn]; !seen || pos.Offset < off {
					firstPublish[fn] = pos.Offset
				}
			case isFanoutCall(call):
				fanouts = append(fanouts, fanoutSite{fn: fn, offset: pos.Offset, line: pos.Line})
			}
			return true
		})
		for _, site := range fanouts {
			key := name + ":" + site.fn
			found[key] = true
			if !allowed[key] {
				t.Errorf("unlisted member fan-out call at %s (line %d)", key, site.line)
				continue
			}
			if off, ok := firstPublish[site.fn]; !ok || off > site.offset {
				t.Errorf("member fan-out at %s (line %d) does not follow a PublishUserMessage call", key, site.line)
			}
		}
	}
	for key := range allowed {
		if !found[key] {
			t.Errorf("listed member fan-out site %s no longer exists; update the list", key)
		}
	}
}

// TestClassifyPublishUserMessageCall pins how the persisted-row scanners
// classify PublishUserMessage calls: only a 4-argument call on an `events`
// field or variable is the event publish. Every other PublishUserMessage
// call, the broker proxy's 4-argument calls included, is "other", which
// TestPersistedRowEffectEnumeration accepts only for a listed broker proxy
// call (proxyExcluded); an event publisher held under another name (ep) is
// therefore reported as unclassified, not ignored. A rename of the events
// field would also fail loudly, since every listed guarded site must then
// be found.
func TestClassifyPublishUserMessageCall(t *testing.T) {
	for src, want := range map[string]publishCallKind{
		`s.events.PublishUserMessage(ctx, msg, attachments, refs)`:                     publishCallEvent,
		`p.events.PublishUserMessage(ctx, msg, refs, artifactRefs)`:                    publishCallEvent,
		`nd.events.PublishUserMessage(ctx, msg, nil, nil)`:                             publishCallEvent,
		`events.PublishUserMessage(ctx, msg, nil, nil)`:                                publishCallEvent,
		`bp.PublishUserMessage(ctx, agent.ProjectID, result.RecipientID, msg)`:         publishCallOther,
		`p.PublishUserMessage(ctx, projectID, r.Name, &recipMsg)`:                      publishCallOther,
		`nd.brokerProxy.PublishUserMessage(ctx, sub.ProjectID, sub.SubscriberID, msg)`: publishCallOther,
		`ep.PublishUserMessage(ctx, msg, nil, nil)`:                                    publishCallOther,
		`pub.PublishUserMessage(ctx, msg, nil, nil)`:                                   publishCallOther,
		`(*eventBuilder).PublishUserMessage(p, ctx, msg, nil, nil)`:                    publishCallOther,
		`s.events.PublishUserMessage(ctx, msg, attachments)`:                           publishCallOther,
		`PublishUserMessage(ctx, msg, nil, nil)`:                                       publishCallOther,
		`s.events.PublishChatMemberMessage(ctx, msg, nil, nil, ids)`:                   publishCallNone,
		`s.events.PublishUserNotification(ctx, n)`:                                     publishCallNone,
	} {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			t.Fatalf("%s: not a call", src)
		}
		if got := classifyPublishUserMessageCall(call); got != want {
			t.Errorf("classifyPublishUserMessageCall(%s) = %v, want %v", src, got, want)
		}
	}
}

// TestScanPublishUserMessageFixtures runs the scanner and the broker proxy
// check over fixture source, so each way a new publish could hide is pinned:
// an aliased event publisher inside a listed function, a second call on a
// listed receiver, a missing listed call, a method value and a method
// expression.
func TestScanPublishUserMessageFixtures(t *testing.T) {
	const src = `package hub

func (s *Server) handleAgentOutboundMessage() {
	bp.PublishUserMessage(ctx, projectID, userID, msg)
	pub := s.events
	pub.PublishUserMessage(ctx, msg, nil, nil)
	s.events.PublishUserMessage(ctx, msg, nil, nil)
}

func (p *MessageBrokerProxy) PublishToGroup() {
	p.PublishUserMessage(ctx, projectID, a, msg)
	p.PublishUserMessage(ctx, projectID, b, msg)
}

func (s *Server) methodValue() {
	f := s.events.PublishUserMessage
	f(ctx, msg, nil, nil)
}

func methodExpr() {
	g := (*eventBuilder).PublishUserMessage
	_ = g
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, proxies, unclassified := scanPublishUserMessage(fset, f, "fixture.go")

	if len(events) != 1 || events[0].funcName != "handleAgentOutboundMessage" || events[0].line != 7 {
		t.Errorf("event publish sites = %+v, want the one s.events call at line 7", events)
	}
	if want := []string{"fixture.go:methodValue (line 16)", "fixture.go:methodExpr (line 21)"}; strings.Join(unclassified, "|") != strings.Join(want, "|") {
		t.Errorf("unclassified = %v, want %v", unclassified, want)
	}

	expected := map[proxyCallKey]int{
		{"fixture.go", "handleAgentOutboundMessage", "bp"}:  1,
		{"fixture.go", "PublishToGroup", "p"}:               1,
		{"fixture.go", "publishToBroker", "nd.brokerProxy"}: 1,
	}
	errs := strings.Join(checkProxyCalls(proxies, expected), "\n")
	for _, want := range []string{
		// The alias inside a listed function is not hidden by the listed bp call.
		`unclassified PublishUserMessage call at fixture.go:handleAgentOutboundMessage on receiver "pub" (lines [6])`,
		// A second call on a listed receiver is counted, not collapsed.
		`broker proxy call fixture.go:PublishToGroup on receiver "p" occurs 2 times (lines [11 12])`,
		// A listed call that is gone is reported.
		`broker proxy call fixture.go:publishToBroker on receiver "nd.brokerProxy" is listed in proxyExcluded but was not found`,
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("checkProxyCalls errors missing %q; got:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, `receiver "bp"`) {
		t.Errorf("the listed bp call must pass; got:\n%s", errs)
	}

	// The real proxy calls pass exactly.
	if errs := checkProxyCalls([]proxyCall{
		{key: proxyCallKey{"fixture.go", "handleAgentOutboundMessage", "bp"}, line: 4},
		{key: proxyCallKey{"fixture.go", "PublishToGroup", "p"}, line: 11},
		{key: proxyCallKey{"fixture.go", "publishToBroker", "nd.brokerProxy"}, line: 30},
	}, expected); len(errs) != 0 {
		t.Errorf("an exact match must pass; got %v", errs)
	}
}
