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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// When two auxiliary runtimes both hold a project-matched entry for the
// slug, the stop must land on the SAME runtime the target container ID came
// from — the sorted-first one every time, never whichever one Go's map
// iteration happens to visit first for the manager.
func TestSubstrateBroker_StopTwoAuxMatches_TargetAndManagerAgree(t *testing.T) {
	for i := 0; i < 20; i++ {
		defaultStops, stopsA, stopsB := &stopRecorder{}, &stopRecorder{}, &stopRecorder{}
		srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, defaultStops)
		matching := func(containerID string, rec *stopRecorder) *runtime.MockRuntime {
			return &runtime.MockRuntime{
				NameFunc: func() string { return containerID },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{
						Name:        "dev",
						ContainerID: containerID,
						Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID},
					}}, nil
				},
				StopFunc: rec.stop,
			}
		}
		// Registered in reverse order, so insertion order can't stand in for
		// sorting.
		addAuxRuntime(t, srv, "aux-b", matching("c-b", stopsB))
		addAuxRuntime(t, srv, "aux-a", matching("c-a", stopsA))

		w := httptest.NewRecorder()
		srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
		if w.Code != http.StatusAccepted {
			t.Fatalf("iteration %d: status=%d body=%s, want 202", i, w.Code, w.Body.String())
		}
		if a, b := stopsA.calls(), stopsB.calls(); len(a) != 1 || a[0] != "c-a" || len(b) != 0 {
			t.Fatalf("iteration %d: Stop calls aux-a=%v aux-b=%v, want only aux-a [c-a]", i, a, b)
		}
	}
}
