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
}

// wantAgentRunIDWriters is the complete set of functions outside the store
// that change an agent's recorded run, by group: the dispatcher, the
// broker's launch report and the move revert worker.
var wantAgentRunIDWriters = map[string]string{
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.beginRun":           "dispatcher",
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.adoptBrokerRunID":   "dispatcher",
	"pkg/hub/httpdispatcher.go HTTPAgentDispatcher.swapRunID":          "dispatcher",
	"pkg/hub/handlers_agent_launch_report.go Server.settleLaunchedRun": "launch report",
	"pkg/hub/reincarnate_move_worker.go Server.rollbackMove":           "revert worker",
}

// funcKey names a function declaration as Recv.Name (or Name).
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
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
}

// wantEntRunIDSetterCallers is every function outside the generated ent
// code that calls an ent run_id setter: the two agent run writes behind
// the store's run methods, and the agent credential insert (its own
// run_id column).
var wantEntRunIDSetterCallers = []string{
	"pkg/store/entadapter/agent_store.go AgentStore.setAgentRunIDOnce",
	"pkg/store/entadapter/agent_store.go AgentStore.swapAgentRunID",
	"pkg/store/entadapter/credential_store.go createAgentCredential",
}

// TestAgentRunIDStoreWritersAreAClosedSet: below the store interface, the
// agent's run_id column is written only by the functions behind
// SetAgentRunID, CompareAndSwapAgentRunID and RevertAgentRunID, so a new
// store method (or direct ent use anywhere in the module) that writes it
// fails here.
func TestAgentRunIDStoreWritersAreAClosedSet(t *testing.T) {
	assert.Equal(t, wantEntRunIDSetterCallers, moduleReferences(t, entRunIDSetters, filepath.Join("pkg", "ent")))
}

// TestAgentRunIDWritersAreAClosedSet: the functions that change an agent's
// recorded run are exactly the dispatcher, the broker launch report and
// the revert worker; the launch report is reached only from the broker
// launch handler, and no agent-token request reaches any of them.
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
