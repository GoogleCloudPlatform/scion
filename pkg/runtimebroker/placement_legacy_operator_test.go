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
	"net/http"
	"strings"
	"testing"

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
