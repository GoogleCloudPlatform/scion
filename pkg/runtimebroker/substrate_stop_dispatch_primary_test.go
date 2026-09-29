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

// A match on the default runtime, on either resolution stage, must be
// stopped through the default runtime even while an auxiliary runtime is
// registered — never through the auxiliary one.
func TestSubstrateBroker_StopDefaultRuntimeMatch_DispatchesToDefaultWithAuxRegistered(t *testing.T) {
	cases := []struct {
		name string
		list func(context.Context, map[string]string) ([]api.AgentInfo, error)
	}{
		{"project-scoped stage", func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name: "dev", ContainerID: "c-default",
				Labels: map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: gapProjBID},
			}}, nil
		}},
		{"unlabelled fallback stage", func(_ context.Context, f map[string]string) ([]api.AgentInfo, error) {
			if _, scoped := f[projectkeys.LabelProjectID]; scoped {
				return nil, nil
			}
			return []api.AgentInfo{{Name: "dev", ContainerID: "c-default", Labels: map[string]string{"scion.name": "dev"}}}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaultStops, auxStops := &stopRecorder{}, &stopRecorder{}
			srv := newProberBroker(t, tc.list, defaultStops)
			addAuxRuntime(t, srv, "aux-a", &runtime.MockRuntime{
				NameFunc: func() string { return "aux-a" },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				StopFunc: auxStops.stop,
			})

			w := httptest.NewRecorder()
			srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
			}
			if d, a := defaultStops.calls(), auxStops.calls(); len(d) != 1 || d[0] != "c-default" || len(a) != 0 {
				t.Fatalf("Stop calls default=%v aux=%v, want only default [c-default]", d, a)
			}
		})
	}
}

// An unlabelled (legacy) match found only on an auxiliary runtime by the
// fallback stage must be stopped through that auxiliary runtime, not the
// default one.
func TestSubstrateBroker_StopFallbackAuxMatch_DispatchesToThatAux(t *testing.T) {
	defaultStops, auxStops := &stopRecorder{}, &stopRecorder{}
	srv := newProberBroker(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }, defaultStops)
	addAuxRuntime(t, srv, "aux-a", &runtime.MockRuntime{
		NameFunc: func() string { return "aux-a" },
		ListFunc: func(_ context.Context, f map[string]string) ([]api.AgentInfo, error) {
			if _, scoped := f[projectkeys.LabelProjectID]; scoped {
				return nil, nil
			}
			return []api.AgentInfo{{Name: "dev", ContainerID: "c-aux", Labels: map[string]string{"scion.name": "dev"}}}, nil
		},
		StopFunc: auxStops.stop,
	})

	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", gapProjBID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if d, a := defaultStops.calls(), auxStops.calls(); len(a) != 1 || a[0] != "c-aux" || len(d) != 0 {
		t.Fatalf("Stop calls default=%v aux=%v, want only aux [c-aux]", d, a)
	}
}
