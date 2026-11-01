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

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentRunIDWriteMethods are the store methods that change an agent's
// recorded run.
var agentRunIDWriteMethods = map[string]bool{
	"SetAgentRunID":            true,
	"CompareAndSwapAgentRunID": true,
	"RevertAgentRunID":         true,
	"AdoptAgentRunID":          true,
	"SettleLaunchedRun":        true,
}

// wantAgentRunIDWriters is the complete set of functions outside the store
// that change an agent's recorded run through the store's run methods, by
// group: the dispatcher, the broker's launch report, the move revert worker
// and the heartbeat adoption of a stranded run (ptone/scion#3661). The
// delete engine's soft finish changes it through DeletionFields instead
// (TestAgentRunIDSoftFinishWritersAreAClosedSet).
var wantAgentRunIDWriters = map[string]string{
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.beginRun":               "dispatcher",
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.adoptBrokerRunID":       "dispatcher",
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.swapRunID":              "dispatcher",
	"pkg/hub/handlers_agent_launch_report.go Server.settleLaunchedRun":     "launch report",
	"pkg/hub/reincarnate_move_worker.go Server.rollbackMove":               "revert worker",
	"pkg/hub/broker_heartbeat_run_adoption.go Server.classifyReportedRuns": "heartbeat adoption",
}

// referencesOutsideStore returns "file Func" for every non-test function in
// the module, outside the store packages, that references a selector named
// in names (a call or a method value).
func referencesOutsideStore(t *testing.T, names map[string]bool) []string {
	t.Helper()
	return moduleReferences(t, names, filepath.Join("pkg", "store"), filepath.Join("pkg", "ent"))
}

// moduleReferences returns "file Func" for every non-test function in the
// module, outside skip (module-relative directories), that references a
// selector named in names.
func moduleReferences(t *testing.T, names map[string]bool, skip ...string) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "web": true, "docs-site": true,
		"extras": true, "vendor": true, ".scion": true,
	}
	for _, dir := range skip {
		skipDirs[dir] = true
	}
	var refs []string
	fset := token.NewFileSet()
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of the whole module: SkipDir only for module-level directories (.git, node_modules, web, docs-site, extras, vendor, .scion and the caller's pkg/store, pkg/ent), none under pkg/hub, so it covers every directory below pkg/hub.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && names[sel.Sel.Name] {
					found = true
				}
				return !found
			})
			if found {
				refs = append(refs, filepath.ToSlash(rel)+" "+funcKey(fn))
			}
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(refs)
	return refs
}

// entRunIDSetters are the generated ent builder methods that write a
// run_id column.
var entRunIDSetters = map[string]bool{
	"SetRunID":         true,
	"SetNillableRunID": true,
	"ClearRunID":       true,
	"UpdateRunID":      true, // agent upsert
}

// wantEntRunIDSetterCallers is every function outside the generated ent
// code that calls an ent run_id setter: the two agent run writes behind
// the store's run methods, the delete engine's soft finish
// (applyAgentDeletionFields, which writes the run only for a
// DeletionFields.SoftFinishRunID, which FinalizeAgentDeletion's soft mode
// alone accepts, ptone/scion#5016), the agent credential insert and the
// broker-reported run upsert (each its own run_id column).
var wantEntRunIDSetterCallers = []string{
	"pkg/store/entadapter/agent_deletion_store.go applyAgentDeletionFields",
	"pkg/store/entadapter/agent_store.go AgentStore.setAgentRunIDOnce",
	"pkg/store/entadapter/agent_store.go AgentStore.swapAgentRunID",
	"pkg/store/entadapter/broker_reported_run_store.go AgentStore.RecordBrokerReportedRuns",
	"pkg/store/entadapter/credential_store.go createAgentCredential",
}

// TestAgentRunIDStoreWritersAreAClosedSet: below the store interface, the
// agent's run_id column is written only by the functions behind
// SetAgentRunID, CompareAndSwapAgentRunID, RevertAgentRunID and
// AdoptAgentRunID, and by FinalizeAgentDeletion's soft mode for
// DeletionFields.SoftFinishRunID (applyAgentDeletionFields,
// ptone/scion#5016; its hub callers are fenced by
// TestAgentRunIDSoftFinishWritersAreAClosedSet), so a new store method (or
// direct ent use anywhere in the module) that writes it fails here.
func TestAgentRunIDStoreWritersAreAClosedSet(t *testing.T) {
	assert.Equal(t, wantEntRunIDSetterCallers, moduleReferences(t, entRunIDSetters, filepath.Join("pkg", "ent")))

	// An agent upsert could write run_id through its conflict update
	// (UpdateNewValues) without naming a run_id setter, so nothing outside
	// the generated code upserts agents: no conflict clause on an Agent
	// create builder, and no use of the agent upsert types.
	assert.Empty(t, moduleReferences(t, map[string]bool{
		"AgentUpsert": true, "AgentUpsertOne": true, "AgentUpsertBulk": true,
	}, filepath.Join("pkg", "ent")), "agent upsert type outside the generated ent code")
	assert.Empty(t, agentUpsertCalls(t), "agent upsert outside the generated ent code")
}

// TestAgentSingleWriterColumnsAreAClosedSet: module-wide, outside the
// generated ent code, agents.effective_config is written only by
// SetAgentEffectiveConfig and agents.launch_run_id only by beginLaunch
// (WP1 slice 3 addendum v12 section 1). The store package's
// TestAgentColumnSetterGuard checks the same by type inside pkg/store;
// this scan covers every other package by name. The writer guard's proof
// package (entadapter testdata) calls both families on purpose.
func TestAgentSingleWriterColumnsAreAClosedSet(t *testing.T) {
	skip := []string{filepath.Join("pkg", "ent"), filepath.Join("pkg", "store", "entadapter", "testdata", "writerguardproof")}
	assert.Equal(t, []string{"pkg/store/entadapter/effective_config_store.go AgentStore.SetAgentEffectiveConfig"},
		moduleReferences(t, map[string]bool{
			"SetEffectiveConfig": true, "SetNillableEffectiveConfig": true,
			"ClearEffectiveConfig": true, "UpdateEffectiveConfig": true,
		}, skip...))
	assert.Equal(t, []string{"pkg/store/entadapter/launch_store.go AgentStore.beginLaunch"},
		moduleReferences(t, map[string]bool{
			"SetLaunchRunID": true, "SetNillableLaunchRunID": true,
			"ClearLaunchRunID": true, "UpdateLaunchRunID": true,
		}, skip...))
}

// softFinishRunFields are the store.DeletionFields that write an agent's
// recorded run on a soft finalize (ptone/scion#5016).
var softFinishRunFields = map[string]bool{
	"SoftFinishRunID":          true,
	"SoftFinishPreviousRunIDs": true,
}

// TestAgentRunIDSoftFinishWritersAreAClosedSet: outside the store, the only
// function that names the soft-finish run fields, as a selector or as a
// composite literal key, is the delete engine's soft finish. With the store
// methods in wantAgentRunIDWriters, these are every function that changes
// an agent's recorded run.
func TestAgentRunIDSoftFinishWritersAreAClosedSet(t *testing.T) {
	assert.Equal(t,
		[]string{"pkg/hub/agent_delete_engine.go deletionEngine.finish"},
		moduleFieldUses(t, softFinishRunFields, filepath.Join("pkg", "store"), filepath.Join("pkg", "ent")))
}

// moduleFieldUses returns "file Func" for every non-test function in the
// module, outside skip (module-relative directories), that names a field
// in names: a selector (x.Name) or a composite literal key (T{Name: v}),
// which moduleReferences does not see.
func moduleFieldUses(t *testing.T, names map[string]bool, skip ...string) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "web": true, "docs-site": true,
		"extras": true, "vendor": true, ".scion": true,
	}
	for _, dir := range skip {
		skipDirs[dir] = true
	}
	var uses []string
	fset := token.NewFileSet()
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of the whole module from its root: SkipDir only for the module-relative directories in skipDirs (.git, node_modules, web, docs-site, extras, vendor, .scion and the caller's pkg/store and pkg/ent), so it covers every package directory below pkg/hub.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if namesField(fn.Body, names) {
				uses = append(uses, filepath.ToSlash(rel)+" "+funcKey(fn))
			}
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(uses)
	return uses
}

// namesField reports whether node contains a selector or a composite
// literal key naming a field in names.
func namesField(node ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if names[n.Sel.Name] {
				found = true
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && names[id.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestNamesField pins moduleFieldUses' matcher on a selector and a
// composite literal key, including one that is not the last node.
func TestNamesField(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"set.SoftFinishRunID = &r", true},
		{"_ = store.DeletionFields{SoftFinishPreviousRunIDs: &p}", true},
		{"_ = store.DeletionFields{RunID: &r}", false},
		{"_ = SoftFinishRunID", false},
		{"set.SoftFinishRunID = &r; x.Other = 1", true},
	} {
		t.Run(tc.src, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nfunc f() {\n"+tc.src+"\n}\n", parser.SkipObjectResolution)
			require.NoError(t, err)
			assert.Equal(t, tc.want, namesField(f, softFinishRunFields))
		})
	}
}

// agentUpsertCalls returns the functions outside the generated ent code
// that put a conflict clause (OnConflict, OnConflictColumns) on a chain
// starting from an Agent create builder (….Agent.Create() or
// ….Agent.CreateBulk(…)).
func agentUpsertCalls(t *testing.T) []string {
	t.Helper()
	var calls []string
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	fset := token.NewFileSet()
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of the whole module: SkipDir only for module-level directories (.git, node_modules, web, docs-site, extras, vendor, .scion, pkg/ent), none under pkg/hub, so it covers every directory below pkg/hub.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch rel {
			case ".git", "node_modules", "web", "docs-site", "extras", "vendor", ".scion", filepath.Join("pkg", "ent"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "OnConflict" || sel.Sel.Name == "OnConflictColumns") && chainFromAgentCreate(sel.X) {
					found = true
				}
				return !found
			})
			if found {
				calls = append(calls, filepath.ToSlash(rel)+" "+funcKey(fn))
			}
		}
		return nil
	})
	require.NoError(t, err)
	return calls
}

// TestAgentRunIDWritersAreAClosedSet: the functions that change an agent's
// recorded run are exactly the dispatcher, the broker launch report, the
// revert worker and the heartbeat adoption; the launch report is reached
// only from the broker launch handler, and no agent-token request reaches
// any of them.
func TestAgentRunIDWritersAreAClosedSet(t *testing.T) {
	t.Run("writers", func(t *testing.T) {
		want := make([]string, 0, len(wantAgentRunIDWriters))
		for k := range wantAgentRunIDWriters {
			want = append(want, k)
		}
		sort.Strings(want)
		assert.Equal(t, want, referencesOutsideStore(t, agentRunIDWriteMethods))
	})

	t.Run("launch report is reached only from the broker launch handler", func(t *testing.T) {
		assert.Equal(t,
			[]string{"pkg/hub/handlers_agent_launch_report.go Server.handleAgentLaunchReport"},
			referencesOutsideStore(t, map[string]bool{"settleLaunchedRun": true}))
	})

	t.Run("no agent-token route", func(t *testing.T) {
		for pattern, meta := range routeMetadataTable {
			if meta.Classification != RouteAgentToken {
				continue
			}
			assert.False(t, strings.HasPrefix(pattern, "/api/v1/runtime-brokers/"),
				"agent-token route %q is under the broker launch report path", pattern)
		}
	})

	t.Run("agent token on the launch report is refused and changes nothing", func(t *testing.T) {
		ctx := context.Background()
		srv, s, _, project := setupCredentialTestServer(t)
		agent := createCredTestAgent(t, s, tid("agent-run-writer"), project.ID, tid("user-cred-test"))
		_, err := s.SetAgentRunID(ctx, agent.ID, "run-current", nil)
		require.NoError(t, err)
		agent, err = s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		token, err := srv.issueAgentTokenForTest(ctx, agent)
		require.NoError(t, err)

		body, err := json.Marshal(AgentLaunchReport{
			LaunchID: "run-other",
			Agent:    &RemoteAgentInfo{ID: agent.ID, RunID: "run-other"},
		})
		require.NoError(t, err)
		brokerID := agent.RuntimeBrokerID
		if brokerID == "" {
			brokerID = tid("broker-any")
		}
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/runtime-brokers/"+brokerID+"/agents/"+agent.ID+"/launch", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, "body: %s", rec.Body.String())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-current", got.RunID)
	})
}

// TestChainFromAgentCreate pins the matcher agentUpsertCalls relies on.
func TestChainFromAgentCreate(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{expr: "s.client.Agent.Create().SetID(id).SetRunID(r)", want: true},
		{expr: "tx.Agent.CreateBulk(builders...)", want: true},
		{expr: "s.client.Agent.Create()", want: true},
		{expr: "s.client.AgentRecovery.Create().SetID(id)", want: false},
		{expr: "s.client.Agent.UpdateOneID(id)", want: false},
		{expr: "builder.SetID(id)", want: false},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			require.NoError(t, err)
			assert.Equal(t, tc.want, chainFromAgentCreate(e))
		})
	}
}

// TestSetAgentRunIDHasOneFreshMintCaller (WP1 attribution design v2.8,
// section 6.1; review r6 L4): a run mint is trusted as fresh because its
// only production caller generates the run ID for its own dispatch. The
// callers of SetAgentRunID are exactly HTTPAgentDispatcher.beginRun, and
// beginRun passes the result of uuid.NewString(), assigned exactly once.
// The scan covers the whole module except pkg/ent (generated code). The
// allowed exceptions are the shared store conformance fixtures listed in
// storetestExceptions (not production), which supply run IDs of their own
// (implementation review r1 OP-3). They are listed file by file, so a new
// caller anywhere else, a new storetest file included, fails here.
func TestSetAgentRunIDHasOneFreshMintCaller(t *testing.T) {
	storetestExceptions := map[string]bool{
		"pkg/store/storetest/domains_usage_reservation_release.go":  true,
		"pkg/store/storetest/domains_usage_reservation_transfer.go": true, // ptone/scion#4181
	}
	var callers, exempt []string
	for _, ref := range moduleReferences(t, map[string]bool{"SetAgentRunID": true}, filepath.Join("pkg", "ent")) {
		file, _, _ := strings.Cut(ref, " ")
		if storetestExceptions[file] {
			exempt = append(exempt, ref)
			continue
		}
		callers = append(callers, ref)
	}
	// The production caller set is exactly beginRun: the exemption above
	// cannot hide a production caller, because only the listed storetest
	// files are skipped.
	assert.Equal(t, []string{"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.beginRun"}, callers)
	// Each exemption is still in use, by its one fixture.
	assert.ElementsMatch(t, []string{
		"pkg/store/storetest/domains_usage_reservation_release.go newReleaseFixture",
		"pkg/store/storetest/domains_usage_reservation_transfer.go createTransferAgent",
	}, exempt)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "httpdispatcher.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var begin *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "beginRun" {
			begin = fd
		}
	}
	require.NotNil(t, begin, "beginRun not found")
	fresh := map[string]bool{}  // identifiers assigned uuid.NewString()
	assigns := map[string]int{} // assignments per identifier, any value
	var mintArgs []ast.Expr
	ast.Inspect(begin.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					assigns[id.Name]++
				}
			}
			for i, rhs := range x.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok && isSelector(call.Fun, "uuid", "NewString") && i < len(x.Lhs) {
					if id, ok := x.Lhs[i].(*ast.Ident); ok {
						fresh[id.Name] = true
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetAgentRunID" && len(x.Args) >= 3 {
				mintArgs = append(mintArgs, x.Args[2])
			}
		}
		return true
	})
	require.Len(t, mintArgs, 1, "beginRun mints once")
	id, ok := mintArgs[0].(*ast.Ident)
	require.True(t, ok, "the minted run ID is an identifier")
	assert.True(t, fresh[id.Name], "beginRun's minted run ID %s must come from uuid.NewString()", id.Name)
	assert.Equal(t, 1, assigns[id.Name], "beginRun's minted run ID %s must be assigned exactly once (no reassignment before the mint)", id.Name)
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}
