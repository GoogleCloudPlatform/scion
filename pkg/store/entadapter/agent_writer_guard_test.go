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

package entadapter

// TestAgentWritersAreClassified is the writer-coverage guard of WP1 design
// 2.8. It type-checks pkg/store and pkg/store/entadapter (non-test files)
// and reports every agent mutation: a call to a mutating method on any
// expression of type *ent.AgentClient (client.Agent, tx.Agent, a
// parameter typed *ent.AgentClient, ...), a call to (*ent.Agent).Update,
// and any string literal that looks like raw agents-table SQL. Each
// finding is keyed by (file, enclosing top-level function) and must be
// classified in agentWriterClasses. An observed writer must call the
// observer entry point; a writer still pending a later slice is listed in
// the pending class, which that slice must empty. A helper called from
// more than one top-level writer must be classified per caller.

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

type writerClass string

const (
	classObserved writerClass = "observed"
	classPending  writerClass = "pending" // observed in slice 2; must be emptied there
	classExempt   writerClass = "exempt"
	// classObserverOff: a path the writer takes only while no observer is
	// installed (with one it routes the write through an observed path).
	// The guard checks that the caller loads the observer slot.
	classObserverOff writerClass = "observer_off"
)

type writerEntry struct {
	class  writerClass
	reason string
}

// agentWriterClasses classifies every agent mutation site at the slice 2
// head, keyed "file:Func" (methods as "file:Recv.Method"; a per-caller
// helper as "file:helper via file:Caller").
var agentWriterClasses = map[string]writerEntry{
	// Observed in slice 1.
	"agent_store.go:AgentStore.CreateAgent":                               {classObserved, "created"},
	"agent_store.go:AgentStore.DeleteAgent":                               {classObserved, "deleted (hard)"},
	"agent_deletion_finalize.go:CompositeStore.finalizeAgentDeletionOnce": {classObserved, "deleted (soft, hard, create_rolled_back)"},
	"agent_deletion_store.go:applyAgentDeletionFields via agent_deletion_finalize.go:CompositeStore.finalizeAgentDeletionOnce": {classObserved, "soft finalize; the observe call is in the caller"},
	"composite.go:CompositeStore.DeleteProject":                                                             {classObserved, "deleted (project_deleted); project cursor cleanup"},
	"composite.go:CompositeStore.purgeDeletedAgents via composite.go:CompositeStore.PurgeDeletedAgents":     {classObserved, "no event after an observed soft delete; N2 resync; cursor cleanup"},
	"composite.go:CompositeStore.purgeDeletedAgents via composite.go:CompositeStore.PurgeDeletedAgentsByID": {classObserved, "no event after an observed soft delete; N2 resync; cursor cleanup"},

	// Observed in slice 2 (design 2.8). The pending class is empty.
	"agent_store.go:buildAgentUpdate via agent_store.go:AgentStore.updateAgentObserved":                                      {classObserved, "configured, metadata_changed, reincarnated, restored, started, stopped (UpdateAgent with an observer)"},
	"agent_store.go:AgentStore.updateAgentRunningTx via agent_store.go:AgentStore.updateAgentObserved":                       {classObserved, "UpdateAgent running write with an observer"},
	"agent_store.go:buildAgentUpdate via agent_store.go:AgentStore.UpdateAgent":                                              {classObserverOff, "UpdateAgent non-running CAS; with an observer UpdateAgent routes every write through updateAgentObserved"},
	"agent_store.go:buildAgentUpdate via agent_store.go:AgentStore.tryRunningFastPath":                                       {classObserverOff, "UpdateAgent running fast path; with an observer UpdateAgent routes every write through updateAgentObserved"},
	"agent_store.go:AgentStore.updateAgentRunningTx via agent_store.go:AgentStore.UpdateAgent":                               {classObserverOff, "UpdateAgent running path; with an observer UpdateAgent routes every write through updateAgentObserved"},
	"agent_store.go:buildAgentUpdate via agent_store.go:AgentStore.updateAgentRunningTx":                                     {classExempt, "nested helper, classified at its caller: updateAgentRunningTx via updateAgentObserved (observed) and via UpdateAgent (observer_off)"},
	"agent_store.go:AgentStore.UpdateAgentStatus":                                                                            {classObserved, "started, stopped, halted, launch_failed (every branch, timestamps-only included)"},
	"agent_store.go:AgentStore.markAgentContainerMissing via agent_store.go:AgentStore.MarkAgentContainerMissing":            {classObserved, "halted, stopped(error), launch_failed(launch_error)"},
	"agent_store.go:AgentStore.markAgentContainerMissing via agent_store.go:AgentStore.MarkAgentContainerMissingIfUnchanged": {classObserved, "halted, stopped(error), launch_failed(launch_error)"},
	"agent_store.go:AgentStore.setAgentRunIDOnce":                                                                            {classObserved, "run_mint (step 0): pending_run_change_at"},
	"agent_store.go:AgentStore.swapAgentRunID via agent_store.go:AgentStore.CompareAndSwapAgentRunID":                        {classObserved, "run_adopt (cas): rebind, adoption provenance"},
	"agent_store.go:AgentStore.swapAgentRunID via agent_store.go:AgentStore.RevertAgentRunID":                                {classObserved, "run_revert (step 0)"},
	"agent_store.go:AgentStore.swapAgentRunID via agent_store.go:AgentStore.AdoptAgentRunID":                                 {classObserved, "run_adopt (context source: heartbeat_adopt, or cas for the launch-report settle): rebind, adoption provenance"},
	"agent_store.go:AgentStore.swapAgentRunID via agent_store.go:AgentStore.SettleLaunchedRun":                               {classObserved, "run_adopt (cas, Completed launch-report settle): rebind, adoption provenance"},
	"launch_store.go:AgentStore.beginLaunch via launch_store.go:AgentStore.BeginLaunchWithOptions":                           {classObserved, "fingerprint only"},
	"launch_store.go:AgentStore.beginLaunch via launch_store.go:AgentStore.BeginStartLaunch":                                 {classObserved, "fingerprint only"},
	"launch_store.go:AgentStore.MarkLaunchAccepted":                                                                          {classObserved, "launch_accepted"},
	"launch_store.go:AgentStore.endLaunch via launch_store.go:AgentStore.EndLaunch":                                          {classObserved, "launch_failed when coded"},
	"launch_store.go:AgentStore.endLaunch via launch_store.go:AgentStore.EndLaunchWithCode":                                  {classObserved, "launch_failed when coded"},
	"launch_report.go:AgentStore.applyLaunchReportPreRunning":                                                                {classObserved, "configured, started, launch_failed (observed in ApplyLaunchReport)"},
	"launch_report.go:AgentStore.applyLaunchReportActive":                                                                    {classObserved, "configured, started, launch_failed (observed in ApplyLaunchReport)"},
	"launch_report.go:AgentStore.applyLaunchReportEnded":                                                                     {classObserved, "started (late report; observed in ApplyLaunchReport)"},
	"launch_report.go:failedReportEndUpdate via launch_report.go:AgentStore.applyLaunchReportPreRunning":                     {classObserved, "launch_failed(failed): active pre-running failure, LaunchEnd hint kept (#4223 refactor; observed in ApplyLaunchReport)"},
	"launch_report.go:failedReportEndUpdate via launch_report.go:AgentStore.applyLaunchReportEnded":                          {classObserved, "failed report after running_observed (#4223): LaunchReport evidence only, no LaunchEnd hint; error phase: P7a, P5 or P1, never P5b; start-kind: P7/P7a(stopped) (observed in ApplyLaunchReport)"},
	"launch_reaper.go:reapRow":                                {classObserved, "launch_failed (per savepoint)"},
	"start_claim_store.go:AgentStore.ClaimAgentReincarnation": {classObserved, "records the reincarnation base run"},
	"agent_deletion_store.go:applyAgentDeletionFields via agent_deletion_store.go:AgentStore.updateAgentDeletionOnce": {classObserved, "delete claim and rollback (phase, deleted_at)"},

	// Observed in slice 3.
	"effective_config_store.go:AgentStore.SetAgentEffectiveConfig": {classObserved, "configured (the broker's effective-configuration echo)"},

	// Exempt: cannot change a fingerprint or snapshot input.
	"run_intent_store.go:AgentStore.SwapRunIntent":                                                  {classExempt, "run_intent is desired state"},
	"run_intent_store.go:AgentStore.RevertRunIntent":                                                {classExempt, "run_intent is desired state"},
	"run_intent_store.go:AgentStore.BackfillRunIntent":                                              {classExempt, "run_intent is desired state"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ClaimAgentStart":          {classExempt, "claim and intent columns, and an ended start launch's launch_error/launch_cause (ptone/scion#4191); none is a fingerprint or snapshot input"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ClaimAgentStop":           {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.RenewAgentStart":          {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.MarkStartUnconfirmed":     {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ReleaseAgentStart":        {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ReleaseSupersededStart":   {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.DemoteExpiredStartClaim":  {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.DemoteOwnerStartClaims":   {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ConvertUnconfirmedToStop": {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.ReleaseUnconfirmedStart":  {classExempt, "claim and intent columns only"},
	"start_claim_store.go:claimUpdate via start_claim_store.go:AgentStore.SettleEndedLaunchClaim":   {classExempt, "claim and intent columns only"},
	"agent_store.go:AgentStore.MarkStaleAgentsOffline":                                              {classExempt, "activity only"},
	"agent_store.go:AgentStore.MarkStalledAgents":                                                   {classExempt, "activity / stalled_from_activity only"},
	"agent_store.go:AgentStore.SetAgentAnnotation":                                                  {classExempt, "annotations are not exported"},
	"agent_store.go:AgentStore.SetAgentSSHHostKey":                                                  {classExempt, "not a snapshot input"},
	"agent_store.go:AgentStore.SetAgentSoftDeleteOpID":                                              {classExempt, "not a snapshot input"},
	"agent_store.go:AgentStore.SetAgentWorkspacePlacement":                                          {classExempt, "not a snapshot input"},
	"agent_store.go:AgentStore.UpdateAgentConfig":                                                   {classObserved, "metadata_changed (held config edits: fold at start, discard)"},
	"agent_store.go:AgentStore.UpdateAgentExposedPorts":                                             {classExempt, "not a snapshot input"},
	"flat_placement_store.go:AgentStore.SetAgentPinnedRuntimeTarget":                                {classExempt, "placement pin"},
	"agent_store.go:AgentStore.SetAgentRuntimeTarget":                                               {classExempt, "applied_config.RuntimeTarget* is not in the snapshot"},
	"agent_store.go:AgentStore.clearRuntimeTargetOnce":                                              {classExempt, "applied_config.RuntimeTarget* is not in the snapshot"},
	"agent_store.go:AgentStore.ReassignAgentsToBroker":                                              {classExempt, "broker_id is an envelope field of later events"},
	"composite.go:CompositeStore.ReconcileHarnessConfigColumn":                                      {classExempt, "denormalized column; the snapshot reads applied_config (resync backstop)"},
	"composite.go:CompositeStore.BackfillEmptyAgentRoles":                                           {classExempt, "role is not a snapshot input"},
}

var (
	agentMutators = map[string]bool{
		"Create": true, "CreateBulk": true, "Update": true, "UpdateOne": true,
		"UpdateOneID": true, "Delete": true, "DeleteOne": true, "DeleteOneID": true,
	}
	rawAgentSQL  = regexp.MustCompile(`(?i)\b(update|delete\s+from|insert\s+into)\s+"?agents"?\b`)
	observeEntry = map[string]bool{
		"observeAgentWrite": true, "observeAgentWriteDeferred": true, "observedAgentWrite": true,
		"observeLaunchReport": true, "observeReap": true,
	}
	entPkgPath      = "github.com/GoogleCloudPlatform/scion/pkg/ent"
	guardedPackages = []string{
		"github.com/GoogleCloudPlatform/scion/pkg/store",
		"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter",
	}
)

// writerFinding is one agent mutation site.
type writerFinding struct {
	key  string // file:Func
	pos  token.Position
	what string
}

// guardReport is the analyzer's output.
type guardReport struct {
	findings []writerFinding
	// callers maps a function key to the keys of the top-level functions
	// that call it.
	callers map[string][]string
	// observes is the set of function keys whose body calls the observer
	// entry point.
	observes map[string]bool
	// loadsObserver is the set of function keys whose body loads the
	// observer slot (a call of X.lifecycle.load()).
	loadsObserver map[string]bool
}

// observedThrough reports whether key calls the observer, or is reached
// only through a chain of single callers one of which does (a helper of
// one writer whose observe call sits in that writer).
func (r guardReport) observedThrough(key string) bool {
	for seen := map[string]bool{}; !seen[key]; {
		seen[key] = true
		if r.observes[key] {
			return true
		}
		callers := r.callers[key]
		if len(callers) != 1 {
			return false
		}
		key = callers[0]
	}
	return false
}

func funcKey(fset *token.FileSet, fd *ast.FuncDecl) string {
	file := filepath.Base(fset.Position(fd.Pos()).Filename)
	name := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		t := fd.Recv.List[0].Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		if id, ok := t.(*ast.Ident); ok {
			name = id.Name + "." + name
		}
	}
	return file + ":" + name
}

func isNamed(t types.Type, pkgPath, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

// analyzeAgentWriters loads patterns and reports agent mutations.
func analyzeAgentWriters(t *testing.T, patterns ...string) guardReport {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  ".",
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load %v: %v", patterns, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d package load errors", n)
	}
	rep := guardReport{callers: map[string][]string{}, observes: map[string]bool{}, loadsObserver: map[string]bool{}}
	declKey := map[types.Object]string{} // func object -> key
	type callSite struct {
		caller string
		callee types.Object
	}
	var calls []callSite
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := funcKey(pkg.Fset, fd)
				if obj := pkg.TypesInfo.Defs[fd.Name]; obj != nil {
					declKey[obj] = key
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind == token.STRING {
							if s, err := strconv.Unquote(n.Value); err == nil && rawAgentSQL.MatchString(s) {
								rep.findings = append(rep.findings, writerFinding{key, pkg.Fset.Position(n.Pos()), "raw agents SQL"})
							}
						}
					case *ast.CallExpr:
						var callee types.Object
						switch fn := n.Fun.(type) {
						case *ast.Ident:
							callee = pkg.TypesInfo.Uses[fn]
							if observeEntry[fn.Name] {
								rep.observes[key] = true
							}
						case *ast.SelectorExpr:
							callee = pkg.TypesInfo.Uses[fn.Sel]
							if fn.Sel.Name == "load" {
								if inner, ok := fn.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "lifecycle" {
									rep.loadsObserver[key] = true
								}
							}
							if observeEntry[fn.Sel.Name] {
								rep.observes[key] = true
							}
							if agentMutators[fn.Sel.Name] {
								if xt := pkg.TypesInfo.TypeOf(fn.X); xt != nil {
									switch {
									case isNamed(xt, entPkgPath, "AgentClient"):
										rep.findings = append(rep.findings, writerFinding{key, pkg.Fset.Position(n.Pos()), "AgentClient." + fn.Sel.Name})
									case fn.Sel.Name == "Update" && isNamed(xt, entPkgPath, "Agent"):
										rep.findings = append(rep.findings, writerFinding{key, pkg.Fset.Position(n.Pos()), "Agent.Update"})
									}
								}
							}
						}
						if callee != nil {
							calls = append(calls, callSite{key, callee})
						}
					}
					return true
				})
			}
		}
	}
	for _, c := range calls {
		if k, ok := declKey[c.callee]; ok && k != c.caller {
			rep.callers[k] = appendUnique(rep.callers[k], c.caller)
		}
	}
	return rep
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// classify checks rep against classes and returns the problems found.
func classify(rep guardReport, classes map[string]writerEntry) []string {
	var problems []string
	used := map[string]bool{}
	seen := map[string]bool{}
	for _, f := range rep.findings {
		if seen[f.key] {
			continue
		}
		seen[f.key] = true
		callers := rep.callers[f.key]
		if len(callers) > 1 {
			// A shared helper: one entry per caller, none for the helper.
			if _, ok := classes[f.key]; ok {
				problems = append(problems, fmt.Sprintf("%s is a helper with %d callers %v; classify it per caller (\"%s via <caller>\"), not once", f.key, len(callers), callers, f.key))
			}
			for _, c := range callers {
				k := f.key + " via " + c
				e, ok := classes[k]
				if !ok {
					problems = append(problems, fmt.Sprintf("unclassified agent writer %s (%s at %s)", k, f.what, f.pos))
					continue
				}
				used[k] = true
				// The observe call may sit in the helper or in the caller.
				if e.class == classObserved && !rep.observedThrough(c) && !rep.observes[f.key] {
					problems = append(problems, fmt.Sprintf("%s is observed but neither %s nor the helper calls the observer", k, c))
				}
				if e.class == classObserverOff && !loadsUpward(rep, c) {
					problems = append(problems, fmt.Sprintf("%s is observer_off but %s (or its caller) does not load the observer", k, c))
				}
			}
			continue
		}
		e, ok := classes[f.key]
		if !ok {
			problems = append(problems, fmt.Sprintf("unclassified agent writer %s (%s at %s)", f.key, f.what, f.pos))
			continue
		}
		used[f.key] = true
		if e.class == classObserved && !rep.observedThrough(f.key) {
			problems = append(problems, fmt.Sprintf("%s is classified observed but does not call the observer", f.key))
		}
		if e.class == classObserverOff && !loadsUpward(rep, f.key) {
			problems = append(problems, fmt.Sprintf("%s is observer_off but does not load the observer", f.key))
		}
		if e.class == classExempt && e.reason == "" {
			problems = append(problems, fmt.Sprintf("%s is exempt without a reason", f.key))
		}
	}
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !used[k] {
			problems = append(problems, fmt.Sprintf("stale classification %s: no agent mutation found there", k))
		}
	}
	return problems
}

// loadsUpward reports whether key, or every caller of key, loads the
// observer slot.
func loadsUpward(rep guardReport, key string) bool {
	if rep.loadsObserver[key] {
		return true
	}
	callers := rep.callers[key]
	if len(callers) == 0 {
		return false
	}
	for _, c := range callers {
		if !rep.loadsObserver[c] {
			return false
		}
	}
	return true
}

func TestAgentWritersAreClassified(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks pkg/store and pkg/store/entadapter")
	}
	rep := analyzeAgentWriters(t, guardedPackages...)
	if len(rep.findings) == 0 {
		t.Fatal("the analyzer found no agent mutation at all; it is broken")
	}
	for _, p := range classify(rep, agentWriterClasses) {
		t.Error(p)
	}
	var pending []string
	for k, e := range agentWriterClasses {
		if e.class == classPending {
			pending = append(pending, k)
		}
	}
	sort.Strings(pending)
	if len(pending) > 0 {
		t.Errorf("writers still pending slice 2 (%d): %s", len(pending), strings.Join(pending, ", "))
	}
	t.Logf("%d agent mutation sites classified", len(rep.findings))
}

// TestAgentWriterGuardProof runs the analyzer on a testdata package with
// an unclassified agent delete through a *ent.Tx, a parameter typed
// *ent.AgentClient, raw SQL, an observed writer that lost its observe
// call and a helper shared by two writers; each must be reported.
func TestAgentWriterGuardProof(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks a testdata package")
	}
	rep := analyzeAgentWriters(t, "./testdata/writerguardproof")
	classes := map[string]writerEntry{
		"proof.go:observedButSilent":           {classObserved, "x"},
		"proof.go:shared via proof.go:callerA": {classExempt, "x"},
		"proof.go:offPathWithoutLoad":          {classObserverOff, "x"},
	}
	problems := strings.Join(classify(rep, classes), "\n")
	for _, want := range []string{
		"unclassified agent writer proof.go:deleteThroughTx (AgentClient.Delete",
		"unclassified agent writer proof.go:deleteThroughParam (AgentClient.DeleteOneID",
		"unclassified agent writer proof.go:rawSQL (raw agents SQL",
		"unclassified agent writer proof.go:entityUpdate (Agent.Update",
		"proof.go:observedButSilent is classified observed but does not call the observer",
		"unclassified agent writer proof.go:shared via proof.go:callerB",
		"proof.go:offPathWithoutLoad is observer_off but does not load the observer",
	} {
		if !strings.Contains(problems, want) {
			t.Errorf("guard missed %q; it reported:\n%s", want, problems)
		}
	}
}
