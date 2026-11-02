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
	// The start-options entry point (ptone/scion#4669) is a start.
	"DispatchAgentStartWithOptions": true,
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

// How a function whose run intent is recorded by another function (its
// recordedBy) is tied to that recorder. For the checked kinds the guard
// checks that each step's call is present, so a missing or renamed call
// fails it.
//
// Only the existence of each call edge is checked: a selector call whose
// method name matches, anywhere in the caller's body, including inside
// nested function literals. The guard does not check the order of a call
// relative to the record, that the lifecycle dispatch sits inside the
// closure handed to the next step, or that a closure holding the call is
// ever invoked. Those remain for review.
const (
	// linkRecorderCalls: the recorder calls this function, after it
	// recorded the intent this function dispatches under.
	linkRecorderCalls = "recorderCalls"
	// linkCallsRecorder: this function reaches the recorder through the
	// named hops, in order, and hands it the dispatch as a closure. Each
	// step (this function, then each hop, then the recorder) must call the
	// next one.
	linkCallsRecorder = "callsRecorder"
	// linkHandedOff: this function returns a dispatch closure that its
	// callers hand to the recorder as data, which then runs it after
	// recording the intent. A call graph cannot show that, so the link is
	// not checked; reason gives the path.
	linkHandedOff = "handedOff"
)

type lifecycleDispatchRule struct {
	kind string
	// recordedBy, when set for intentRecorded, names the function that
	// records the run intent this function dispatches under, instead of the
	// function itself. The recorder must call recordRunIntent or
	// swapRunIntent. For withStartClaim that is its own record on the
	// start-claims-off branch; with start claims on, acquiring the start
	// claim records the running intent.
	recordedBy string
	// link says how this function is tied to recordedBy: one of the link*
	// kinds. It is required when recordedBy is set, and must be empty
	// otherwise.
	link string
	// hops are the functions between this function and recordedBy, in call
	// order, for linkCallsRecorder.
	hops []string
	// reason says, for linkHandedOff, how the closure reaches the recorder.
	reason string
}

// lifecycleDispatchCallers lists every non-test function in this package
// that calls a lifecycle dispatch method. A new call site has to be added
// here, with how it records run intent.
var lifecycleDispatchCallers = map[string]lifecycleDispatchRule{
	"Server.handleAgentLifecycle":    {kind: intentRecorded},
	"Server.suspendAgent":            {kind: intentRecorded},
	"Server.stopHeldAgent":           {kind: intentRecorded},
	"Server.handleStopAllAgents":     {kind: intentRecorded},
	"Server.autoSuspendStalledAgent": {kind: intentRecorded},
	// The scheduled create-and-start runs under a start claim.
	"Server.dispatchAgentEventHandler": {kind: intentRecorded, recordedBy: "Server.withStartClaim",
		link: linkCallsRecorder, hops: []string{"Server.createUnderClaim"}},
	"Server.handleExistingAgent":    {kind: intentRecorded},
	"Server.createAgentInProject":   {kind: intentRecorded},
	"deletionEngine.dispatch":       {kind: intentRecorded},
	"Server.dispatchAgentDeletions": {kind: intentRecorded},
	// The create-and-start after a workspace sync runs under a start claim.
	"Server.handleWorkspaceSyncToFinalize": {kind: intentRecorded, recordedBy: "Server.withStartClaim",
		link: linkCallsRecorder, hops: []string{"Server.createUnderClaim"}},
	"dispatchDeleteFailedCreate": {kind: intentRecorded, recordedBy: "Server.cleanupFailedCreate",
		link: linkHandedOff, reason: "its callers set the closure as createRollback.DeleteRuntime; cleanupFailedCreate records the stopped intent, then runs it"},
	// The delete of the run the broker reported holding (ptone/scion#5016)
	// is called from deletionEngine.dispatch, after that function recorded
	// the stopped intent for this delete; a second record here would
	// overwrite the prior intent the engine's rollback restores.
	"deletionEngine.retryReportedRunDelete": {kind: intentRecorded, recordedBy: "deletionEngine.dispatch",
		link: linkRecorderCalls},

	"Server.execDispatchStart":   {kind: intentExecutor},
	"Server.execDispatchStop":    {kind: intentExecutor},
	"Server.execDispatchRestart": {kind: intentExecutor},
	"Server.execDispatchDelete":  {kind: intentExecutor},
	"Server.execDispatchCreate":  {kind: intentExecutor},
	// Reincarnation stops and restarts the container in place; the agent
	// is meant to keep running throughout.
	"Server.runReincarnationWorker": {kind: intentUnchanged},
	// Starts run under a start claim, which records intent running
	// (withStartClaim records it itself while start claims are off).
	"Server.startAgentCore": {kind: intentRecorded, recordedBy: "Server.withStartClaim",
		link: linkCallsRecorder},
	// A start that completed after a stop was accepted: intent is already
	// stopped.
	"Server.compensatingStop": {kind: intentUnchanged},
	// The start-claim reaper stops a container an unconfirmed start left
	// running past its hold; the agent's intent is left as it is.
	"Server.stopUnconfirmedStart": {kind: intentUnchanged},
	// The backstop stops an agent whose intent is already stopped.
	"Server.stopForStoppedIntent": {kind: intentUnchanged},
	// Removes a run that landed after its agent was deleted or a delete
	// claimed it: the row is gone, or the delete engine records the stopped
	// intent itself (ptone/scion#3055).
	"HTTPAgentDispatcher.compensateLandedRun": {kind: intentUnchanged},
	// The retained-files purge sends a delete for a soft-deleted agent,
	// then purges the record right after. It is not a run transition: a
	// soft-deleted row cannot take a running intent, and the readers of
	// run intent skip soft-deleted rows (ptone/scion#4744).
	"Server.settleRetainedFiles": {kind: intentUnchanged},
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
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards DispatchAgent lifecycle callers.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards DispatchAgent lifecycle callers.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards DispatchAgent lifecycle callers.
	files, err := globGuardScanDirs(".", "*.go")
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
		rel := filepath.Dir(name)
		if f.Name.Name != guardScanPackage(rel) {
			continue
		}
		// Functions of a guardScanSubdirs package are keyed by its slash
		// path ("maintenance/Name"), so they cannot shadow a pkg/hub
		// function of the same name.
		prefix := ""
		if rel != "." {
			prefix = filepath.ToSlash(rel) + "/"
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcs[prefix+funcDeclKey(fd)] = selectorCallNames(fd)
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

	// Every recordedBy entry has a known link to its recorder, and every
	// checked link is present in the code.
	var names []string
	for name := range lifecycleDispatchCallers {
		names = append(names, name)
	}
	sort.Strings(names)
	links := map[string]int{}
	for _, name := range names {
		rule := lifecycleDispatchCallers[name]
		if rule.recordedBy != "" {
			links[rule.link]++
		}
		for _, msg := range recorderLinkErrors(funcs, name, rule) {
			t.Error(msg)
		}
	}
	for _, link := range []string{linkRecorderCalls, linkCallsRecorder} {
		if links[link] == 0 {
			t.Errorf("no recordedBy entry has link %s; the recorder-link check checked nothing of that kind", link)
		}
	}
}

// recorderLinkErrors checks how the function name is tied to its
// rule.recordedBy, against funcs (as parseHubFuncs returns it). It returns
// one message per problem. A call is matched by the callee's name only (the
// method name for Recv.Name, whatever the receiver), as selectorCallNames
// records it, so the callee of every checked step must be a method. Only
// the presence of each call is checked; see the link* kinds for what is
// not.
func recorderLinkErrors(funcs map[string]map[string]int, name string, rule lifecycleDispatchRule) []string {
	var errs []string
	reported := map[string]bool{}
	report := func(msg string) {
		if !reported[msg] {
			reported[msg] = true
			errs = append(errs, msg)
		}
	}
	if rule.recordedBy == "" {
		if rule.link != "" || len(rule.hops) > 0 || rule.reason != "" {
			errs = append(errs, fmt.Sprintf("%s has a recorder link but no recordedBy; set recordedBy or remove the link", name))
		}
		return errs
	}
	if rule.kind != intentRecorded {
		errs = append(errs, fmt.Sprintf("%s sets recordedBy but its kind is %q, not %q", name, rule.kind, intentRecorded))
	}
	// calls adds a message unless caller calls callee.
	calls := func(caller, callee string) {
		callNames, ok := funcs[caller]
		if !ok {
			report(fmt.Sprintf("%s: no function %s in package hub; update the recorder link", name, caller))
			return
		}
		recv, method, isMethod := strings.Cut(callee, ".")
		if !isMethod || recv == "" || strings.Contains(recv, "/") {
			report(fmt.Sprintf("%s: link step %s is not a method of this package; the link check matches selector calls only", name, callee))
			return
		}
		if _, ok := funcs[callee]; !ok {
			report(fmt.Sprintf("%s: no function %s in package hub; update the recorder link", name, callee))
			return
		}
		if callNames[method] == 0 {
			report(fmt.Sprintf("%s: %s does not call %s, so %s no longer records the run intent for %s; restore the call or update lifecycleDispatchCallers", name, caller, callee, rule.recordedBy, name))
		}
	}
	switch rule.link {
	case linkRecorderCalls:
		if len(rule.hops) > 0 || rule.reason != "" {
			errs = append(errs, fmt.Sprintf("%s: link %s takes no hops or reason", name, rule.link))
		}
		calls(rule.recordedBy, name)
	case linkCallsRecorder:
		if rule.reason != "" {
			errs = append(errs, fmt.Sprintf("%s: link %s takes no reason", name, rule.link))
		}
		chain := append(append([]string{name}, rule.hops...), rule.recordedBy)
		for i := 0; i+1 < len(chain); i++ {
			calls(chain[i], chain[i+1])
		}
	case linkHandedOff:
		if len(rule.hops) > 0 {
			errs = append(errs, fmt.Sprintf("%s: link %s takes no hops", name, rule.link))
		}
		if strings.TrimSpace(rule.reason) == "" {
			errs = append(errs, fmt.Sprintf("%s: link %s is not checked and needs a reason that says how the closure reaches %s", name, rule.link, rule.recordedBy))
		}
	case "":
		errs = append(errs, fmt.Sprintf("%s sets recordedBy %s but no link; set link to %s, %s or %s", name, rule.recordedBy, linkRecorderCalls, linkCallsRecorder, linkHandedOff))
	default:
		errs = append(errs, fmt.Sprintf("%s has unknown link %q; use %s, %s or %s", name, rule.link, linkRecorderCalls, linkCallsRecorder, linkHandedOff))
	}
	return errs
}

// TestRecorderLinkErrors checks recorderLinkErrors on a synthetic call
// graph: each broken link is reported, and the intact ones are not.
func TestRecorderLinkErrors(t *testing.T) {
	funcs := map[string]map[string]int{
		"S.recorder": {"recordRunIntent": 1, "callee": 1},
		"S.callee":   {"DispatchAgentDelete": 1},
		"S.starter":  {"hop": 1},
		"S.hop":      {"recorder": 1},
		"S.direct":   {"recorder": 1},
		"S.viaBad":   {"callee": 1},
		"S.wrapper":  {"inner": 1},
		"m/X.inner":  {"recorder": 1},
		"S.lost":     {"DispatchAgentStart": 1},
		"plain":      {"DispatchAgentDelete": 1},
	}
	rec := func(link string) lifecycleDispatchRule {
		return lifecycleDispatchRule{kind: intentRecorded, recordedBy: "S.recorder", link: link}
	}
	withHops := func(hops ...string) lifecycleDispatchRule {
		r := rec(linkCallsRecorder)
		r.hops = hops
		return r
	}
	withLink := func(r lifecycleDispatchRule, hops []string, reason string) lifecycleDispatchRule {
		r.hops, r.reason = hops, reason
		return r
	}
	handed := rec(linkHandedOff)
	handed.reason = "set as a field the recorder runs"
	cases := []struct {
		desc string
		name string
		rule lifecycleDispatchRule
		want string // substring of the only error; empty for none
	}{
		{"no recordedBy", "S.lost", lifecycleDispatchRule{kind: intentRecorded}, ""},
		{"recorder calls", "S.callee", rec(linkRecorderCalls), ""},
		{"calls recorder directly", "S.direct", withHops(), ""},
		{"calls recorder through a hop", "S.starter", withHops("S.hop"), ""},
		{"handed off with reason", "plain", handed, ""},
		{"recorder does not call", "S.lost", rec(linkRecorderCalls), "S.recorder does not call S.lost"},
		{"hop missing from the chain", "S.starter", withHops(), "S.starter does not call S.recorder"},
		{"hop not called", "S.starter", withHops("S.direct"), "S.starter does not call S.direct"},
		{"hop does not call recorder", "S.viaBad", withHops("S.callee"), "S.callee does not call S.recorder"},
		{"hop renamed", "S.starter", withHops("S.hop2"), "no function S.hop2"},
		{"plain function link step", "plain", rec(linkRecorderCalls), "is not a method"},
		{"handed off without reason", "plain", rec(linkHandedOff), "needs a reason"},
		{"missing link", "S.callee", rec(""), "but no link"},
		{"unknown link", "S.callee", rec("calls"), `unknown link "calls"`},
		{"link without recordedBy", "S.callee", lifecycleDispatchRule{kind: intentRecorded, link: linkRecorderCalls}, "no recordedBy"},
		{"hops without recordedBy", "S.callee", lifecycleDispatchRule{kind: intentRecorded, hops: []string{"S.hop"}}, "no recordedBy"},
		{"reason without recordedBy", "S.callee", lifecycleDispatchRule{kind: intentRecorded, reason: "why"}, "no recordedBy"},
		{"recordedBy on a non-recording kind", "S.callee", lifecycleDispatchRule{kind: intentExecutor, recordedBy: "S.recorder", link: linkRecorderCalls}, "its kind is"},
		{"recorder calls with hops", "S.callee", withLink(rec(linkRecorderCalls), []string{"S.hop"}, ""), "takes no hops or reason"},
		{"recorder calls with reason", "S.callee", withLink(rec(linkRecorderCalls), nil, "why"), "takes no hops or reason"},
		{"calls recorder with reason", "S.direct", withLink(rec(linkCallsRecorder), nil, "why"), "takes no reason"},
		{"handed off with hops", "plain", withLink(rec(linkHandedOff), []string{"S.hop"}, "why"), "takes no hops"},
		{"handed off with blank reason", "plain", withLink(rec(linkHandedOff), nil, " \t"), "needs a reason"},
		{"link step in a subpackage", "S.wrapper", withLink(rec(linkCallsRecorder), []string{"m/X.inner"}, ""), "is not a method of this package"},
		{"link step with no receiver", "S.direct", lifecycleDispatchRule{kind: intentRecorded, recordedBy: ".recorder", link: linkCallsRecorder}, "is not a method of this package"},
		{"recorder missing", "S.callee", lifecycleDispatchRule{kind: intentRecorded, recordedBy: "S.gone", link: linkRecorderCalls}, "no function S.gone"},
	}
	for _, tc := range cases {
		errs := recorderLinkErrors(funcs, tc.name, tc.rule)
		if tc.want == "" {
			if len(errs) != 0 {
				t.Errorf("%s: unexpected errors %q", tc.desc, errs)
			}
			continue
		}
		if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
			t.Errorf("%s: errors %q, want one containing %q", tc.desc, errs, tc.want)
		}
	}
}
