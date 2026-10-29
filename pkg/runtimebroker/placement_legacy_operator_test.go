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

package runtimebroker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// plBareGlobal is a global settings file whose Kubernetes runtime entry
// sets no placement: the operator policy is empty.
const plBareGlobal = `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
`

// expectNoPodEffects fails when a refused start or restart stopped or
// started anything.
func expectNoPodEffects(t *testing.T, f *plFixture) {
	t.Helper()
	if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
		t.Errorf("stop=%d start=%d, want 0 and 0", f.mgr.stopCalls, f.mgr.startCalls)
	}
}

// With an empty operator policy on a legacy Runtime Broker, an agent's
// stored tolerations or runtime class are refused at start and at restart
// before anything is stopped or started; extra node selector keys are
// still allowed.
func TestLegacyEmptyPolicyAgentTolerationRefused(t *testing.T) {
	for _, tc := range []struct {
		name, stored, want string
	}{
		{"tolerations", `{"kubernetes": {"tolerations": [{"key": "dedicated", "operator": "Exists"}]}}`,
			"kubernetes.tolerations[0] in the agent's stored config is set, but the agent runtime configures no tolerations"},
		{"runtime class", `{"kubernetes": {"runtimeClassName": "kata"}}`,
			"kubernetes.runtimeClassName in the agent's stored config is set, but the agent runtime configures no runtime class"},
	} {
		for _, op := range []string{"start", "restart"} {
			t.Run(tc.name+" "+op, func(t *testing.T) {
				f, _ := newOrderFixture(t, plBareGlobal, plGoodRecord(), tc.stored)
				w := postAgentOpFor(t, f.srv, plOrderAgent, op, projectBody(f.dotScion, ""))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
				expectNoPodEffects(t, f)
			})
		}
	}

	f, _ := newOrderFixture(t, plBareGlobal, plGoodRecord(), `{"kubernetes": {"nodeSelector": {"disk": "ssd"}}}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion, ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("an extra selector key: got %d %s", w.Code, w.Body.String())
	}
	opts := f.mgr.lastStartOpts
	if !opts.PlacementResolved || opts.ResolvedPlacement == nil || !opts.ResolvedPlacement.OperatorOwned || opts.ResolvedPlacement.InEffect() {
		t.Fatalf("start options carry %+v (resolved %v), want empty operator-owned placement", opts.ResolvedPlacement, opts.PlacementResolved)
	}
}

// An agent's stored node selector value for a key the runtime entry sets
// is refused on a legacy Runtime Broker (it was ignored with a warning
// before), naming the agent's stored config.
func TestLegacyAgentSelectorConflictRefused(t *testing.T) {
	f, _ := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": {"nodeSelector": {"pool": "other"}}}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion, ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `kubernetes.nodeSelector key \"pool\" in the agent's stored config conflicts`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	expectNoPodEffects(t, f)
}

// A restart whose persisted agent config conflicts with the operator
// placement is refused before the restart endpoint's stop.
func TestLegacyRestartPersistedConflictRejectedBeforeStop(t *testing.T) {
	f, _ := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": {"tolerations": [{"key": "other", "operator": "Exists"}]}}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "restart", projectBody(f.dotScion, ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kubernetes.tolerations[0] in the agent's stored config is not one of the agent runtime's tolerations") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	expectNoPodEffects(t, f)
}

// A legacy restart whose stored config exists but does not parse is
// refused before the stop; one with no stored config yet is left to the
// start, which checks the config it provisions.
func TestLegacyRestartUnreadableConfigRefusedBeforeStop(t *testing.T) {
	f, _ := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": `)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "restart", projectBody(f.dotScion, ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), placementAgentConfigCause) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	expectNoPodEffects(t, f)
}

// A legacy restart on the Kubernetes runtime whose project path is empty
// after every recovery cannot read the stored config before the stop, so
// it is refused before the stop.
func TestLegacyPathlessRestartRefusedBeforeStop(t *testing.T) {
	f, _ := newOrderFixture(t, plBareGlobal, plGoodRecord(), `{}`)
	for i := range f.mgr.agents {
		f.mgr.agents[i].ProjectPath = ""
	}
	w := postAgentOpFor(t, f.srv, plOrderAgent, "restart", `{}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), placementFlatRestartNoProjectCause) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	expectNoPodEffects(t, f)
}

// An orphaned agent (no project ID, or its project deleted) is restarted by
// a current Hub with neither projectPath nor projectSlug, and with or
// without the old project ID; with no project recorded by its container
// either, the restart is refused before the stop, like an older Hub's.
func TestLegacyOrphanedAgentRestartRefusedBeforeStop(t *testing.T) {
	for name, query := range map[string]string{"no project ID": "", "deleted project": "?projectId=gone-project"} {
		t.Run(name, func(t *testing.T) {
			f, _ := newOrderFixture(t, plBareGlobal, plGoodRecord(), `{}`)
			for i := range f.mgr.agents {
				f.mgr.agents[i].ProjectPath = ""
			}
			w := postAgentOpFor(t, f.srv, plOrderAgent, "restart"+query, `{}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), placementFlatRestartNoProjectCause) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			expectNoPodEffects(t, f)
		})
	}
}

// Malformed or invalid placement in the Hub's stored runtimes and profiles
// (the overlay's placement findings) refuses every Kubernetes start with a
// fixed cause; it is never read as no placement.
func TestPlacementOverlayMalformedFailsClosed(t *testing.T) {
	f := newPlacementFixture(t, plBareGlobal, k8s.ConnKubeconfig, plGoodRecord())
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	o := config.NewSettingsOverlay()
	o.Update(map[string]config.V1RuntimeConfig{"gke": {Type: "kubernetes", Context: "ctx-a", Namespace: "agents"}}, nil, nil)
	o.SetPlacementFindings([]string{"runtimes.gke"}, nil)
	config.SetGlobalSettingsOverlay(o)
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil)
	wantRefusal(t, err, placementOverlayInvalidCause)

	o.SetPlacementFindings(nil, nil)
	if _, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionForced, "", ""), nil); err != nil {
		t.Fatalf("valid overlay: %v", err)
	}
}

// A selected named profile the settings do not define, with profile
// placement configured for the runtime, refuses the start instead of
// guessing.
func TestResolvePlacementMissingNamedProfile(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(plGPUProfile), k8s.ConnKubeconfig, plGoodRecord())
	_, err := f.srv.resolvePlacement(context.Background(), f.res(resolutionProfile, "nosuch", ""), nil)
	wantRefusal(t, err, "profile nosuch is not defined in the agent runtime's settings")
}

// requireOperatorOwnedStart fails unless Manager.Start received a resolved,
// operator-owned placement.
func requireOperatorOwnedStart(t *testing.T, what string, opts api.StartOptions) {
	t.Helper()
	if !opts.PlacementResolved || opts.ResolvedPlacement == nil || !opts.ResolvedPlacement.OperatorOwned {
		t.Fatalf("%s: Manager.Start got resolved=%v %+v, want resolved operator-owned placement", what, opts.PlacementResolved, opts.ResolvedPlacement)
	}
}

// Every legacy Kubernetes start leg through the Runtime Broker hands
// Manager.Start a resolved, operator-owned placement, also with an empty
// operator policy: a synchronous and an asynchronous create, a start and a
// restart. The agent-side check for an unresolved placement is then never
// the only check.
func TestLegacyKubernetesStartPathsCarryResolvedPlacement(t *testing.T) {
	f := newPlacementFixture(t, plBareGlobal, k8s.ConnKubeconfig, plGoodRecord())
	f.srv.config.ForceRuntime = "kubernetes"
	if w := postCreate(t, f.srv, map[string]any{"name": "sync-agent", "config": map[string]any{"template": "claude"}}); w.Code != http.StatusCreated {
		t.Fatalf("sync create: got %d %s", w.Code, w.Body.String())
	}
	requireOperatorOwnedStart(t, "sync create", f.mgr.lastStartOpts)

	af, mgr, _ := newAsyncPlacementFixture(t, plBareGlobal, plGoodRecord())
	mgr.release = make(chan struct{})
	if w := postCreate(t, af.srv, asyncCreateBody("async-agent", "L-LEGACY")); w.Code != http.StatusCreated {
		t.Fatalf("async create: got %d %s", w.Code, w.Body.String())
	}
	close(mgr.release)
	if !waitUntil(t, 5*time.Second, func() bool { return len(mgr.recorded()) == 1 }) {
		t.Fatal("the launch did not start")
	}
	requireOperatorOwnedStart(t, "async create", mgr.recorded()[0])

	for _, op := range []string{"start", "restart"} {
		of, _ := newOrderFixture(t, plBareGlobal, plGoodRecord(), `{}`)
		if w := postAgentOpFor(t, of.srv, plOrderAgent, op, projectBody(of.dotScion, "")); w.Code != http.StatusAccepted && w.Code != http.StatusOK {
			t.Fatalf("%s: got %d %s", op, w.Code, w.Body.String())
		}
		requireOperatorOwnedStart(t, op, of.mgr.lastStartOpts)
	}
}

// managerStartSites are the functions of the Runtime Broker that call
// Manager.Start, each after resolving placement (placementForStart) into
// the options it passes: createAgent (synchronous create), startAgent and
// restartAgent, and runLaunch (the asynchronous create and start, with the
// options resolved at admission).
var managerStartSites = map[string]bool{"createAgent": true, "startAgent": true, "restartAgent": true, "runLaunch": true}

// Every Manager.Start call site in the Runtime Broker's code is one of
// managerStartSites, so a new start path cannot skip placement resolution
// unnoticed: adding one fails here until it resolves placement and is
// listed.
func TestManagerStartCallSitesResolvePlacement(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Start" || len(call.Args) != 2 {
					return true
				}
				if !isStartOptionsArg(call.Args[1]) {
					return true
				}
				if !managerStartSites[fn.Name.Name] {
					t.Errorf("%s: %s calls Manager.Start but is not a known placement-resolving start site", fset.Position(call.Pos()), fn.Name.Name)
				}
				found[fn.Name.Name] = true
				return true
			})
		}
	}
	for site := range managerStartSites {
		if !found[site] {
			t.Errorf("start site %s no longer calls Manager.Start; update managerStartSites", site)
		}
	}
}

// isStartOptionsArg reports whether e is the start options argument the
// Runtime Broker passes to Manager.Start: opts or lc.opts.
func isStartOptionsArg(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "opts"
	case *ast.SelectorExpr:
		return v.Sel.Name == "opts"
	}
	return false
}
