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
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// The NFS home experiment is registered as a server-layer experiment, off
// by default.
func TestK8sNFSHomeExperiment_Registered(t *testing.T) {
	exp, ok := experiments.Default().Lookup(experiments.K8sNFSHome)
	if !ok {
		t.Fatalf("%s is not registered", experiments.K8sNFSHome)
	}
	if exp.Default {
		t.Error("the experiment must be off by default")
	}
	if !exp.HasLayer(experiments.LayerServer) || exp.HasLayer(experiments.LayerWeb) {
		t.Errorf("layers = %v, want server only", exp.Layers)
	}
	if !slices.Contains(dispatchExperimentNames, experiments.K8sNFSHome) {
		t.Error("the experiment must be sent with dispatches")
	}
}

func dispatchExperimentsServer(t *testing.T, raw string) *Server {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if raw != "" {
		fakeStore.seed("experiments", json.RawMessage(raw))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{store: createTestStore(t), maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	return srv
}

// Create, start and restart dispatches carry the experiment only while it is
// enabled, and an admin toggle applies to the next dispatch.
func TestDispatch_Experiments_OnWire(t *testing.T) {
	srv := dispatchExperimentsServer(t, `{"overrides":{"hub.k8s_nfs_home":true}}`)
	d := srv.CreateAuthenticatedDispatcher()
	if d.dispatchExperimentsProvider == nil {
		t.Fatal("CreateAuthenticatedDispatcher did not install the dispatch experiments provider")
	}
	req, err := d.buildCreateRequest(context.Background(), hubDefaultsDispatchAgent(), "test")
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	if req.Config == nil || req.Config.HubAgentDefaults == nil ||
		!slices.Equal(req.Config.HubAgentDefaults.Experiments, []string{experiments.K8sNFSHome}) {
		t.Fatalf("create dispatch: want experiments [%s], got %+v", experiments.K8sNFSHome, req.Config)
	}
	start := startHubAgentDefaults(d.autoExposePortsDefault(), d.dispatchExperiments())
	if start == nil || !slices.Equal(start.Experiments, []string{experiments.K8sNFSHome}) {
		t.Fatalf("start dispatch: want experiments [%s], got %+v", experiments.K8sNFSHome, start)
	}

	// The broker decodes the same names.
	blob, err := json.Marshal(req.Config)
	if err != nil {
		t.Fatal(err)
	}
	var got runtimebroker.CreateAgentConfig
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if !got.HubAgentDefaults.ExperimentEnabled(experiments.K8sNFSHome) {
		t.Errorf("broker side does not see the experiment in %s", blob)
	}

	off := dispatchExperimentsServer(t, "")
	d = off.CreateAuthenticatedDispatcher()
	if got := d.dispatchExperiments(); got != nil {
		t.Errorf("off by default: want no experiments, got %v", got)
	}
	if got := startHubAgentDefaults(nil, d.dispatchExperiments()); got != nil {
		t.Errorf("off: start dispatch must omit the defaults, got %+v", got)
	}
}
