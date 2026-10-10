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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The stop-target lookup is the same for every broker, whatever runtimes it
// has: a docker default plus ordinary auxiliary runtimes, none of which
// implements any optional capability. These tests pin that general
// behaviour.

const stopLookupProjectID = "7d0c2f6e-3a41-4b8e-9f10-2b5c8d9e0a11"

// stopRecorder records the ids a MockRuntime's Stop receives.
type stopRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *stopRecorder) stop(_ context.Context, ref runtime.RunRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ref.ID)
	return nil
}

func (r *stopRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// newPlainDockerBroker returns a broker whose default runtime is a plain
// (capability-free) "docker" MockRuntime with no agents.
func newPlainDockerBroker(t *testing.T, defaultStops *stopRecorder) *Server {
	t.Helper()
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		StopFunc: defaultStops.stop,
	}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	return New(DefaultServerConfig(), mgr, rt)
}

// matchingAuxRuntime reports one project-labelled "dev" entry with the given
// container ID on every List call.
func matchingAuxRuntime(name, containerID string, stops *stopRecorder) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name:        "dev",
				ContainerID: containerID,
				Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: stopLookupProjectID},
			}}, nil
		},
		StopFunc: stops.stop,
	}
}

func failingAuxRuntime(name string) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, errors.New("simulated list failure")
		},
	}
}

func doStop(srv *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.stopAgent(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop", nil), "dev", stopLookupProjectID)
	return w
}

// A stop for an agent living on an auxiliary runtime is dispatched to that
// runtime's manager, with the container ID that runtime reported.
func TestStop_DockerDefaultWithAuxRuntime_DispatchesToMatchingManager(t *testing.T) {
	defaultStops, auxStops := &stopRecorder{}, &stopRecorder{}
	srv := newPlainDockerBroker(t, defaultStops)
	addAuxRuntime(t, srv, "kubernetes", matchingAuxRuntime("kubernetes", "pod-dev", auxStops))

	w := doStop(srv)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if got := auxStops.calls(); len(got) != 1 || got[0] != "pod-dev" {
		t.Errorf("aux Stop calls = %v, want [pod-dev]", got)
	}
	if got := defaultStops.calls(); len(got) != 0 {
		t.Errorf("default Stop calls = %v, want none", got)
	}
}

// When two auxiliary runtimes both report a matching entry, the target and
// the manager Stop is sent to come from the same (sorted-first) runtime on
// every call — never a container ID from one runtime sent to another.
func TestStop_TwoAuxMatches_TargetAndManagerAgree(t *testing.T) {
	for i := 0; i < 30; i++ {
		defaultStops, stopsA, stopsB := &stopRecorder{}, &stopRecorder{}, &stopRecorder{}
		srv := newPlainDockerBroker(t, defaultStops)
		// Registered in reverse order, so insertion order can't stand in
		// for sorting.
		addAuxRuntime(t, srv, "aux-b", matchingAuxRuntime("aux-b", "c-b", stopsB))
		addAuxRuntime(t, srv, "aux-a", matchingAuxRuntime("aux-a", "c-a", stopsA))

		w := doStop(srv)
		if w.Code != http.StatusAccepted {
			t.Fatalf("iteration %d: status=%d body=%s, want 202", i, w.Code, w.Body.String())
		}
		if a, b := stopsA.calls(), stopsB.calls(); len(a) != 1 || a[0] != "c-a" || len(b) != 0 {
			t.Fatalf("iteration %d: Stop calls aux-a=%v aux-b=%v, want only aux-a [c-a]", i, a, b)
		}
	}
}

// A List error on one auxiliary runtime does not block a match found on
// another: the stop succeeds on the matching runtime, whichever sorts first.
func TestStop_AuxListErrorWithMatchElsewhere_Succeeds(t *testing.T) {
	for _, tc := range []struct{ name, failing, matching string }{
		{"failing sorts first", "aux-a-failing", "aux-b-matching"},
		{"matching sorts first", "aux-a-matching", "aux-b-failing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaultStops, auxStops := &stopRecorder{}, &stopRecorder{}
			srv := newPlainDockerBroker(t, defaultStops)
			addAuxRuntime(t, srv, tc.failing, failingAuxRuntime(tc.failing))
			addAuxRuntime(t, srv, tc.matching, matchingAuxRuntime(tc.matching, "c-match", auxStops))

			w := doStop(srv)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
			}
			if got := auxStops.calls(); len(got) != 1 || got[0] != "c-match" {
				t.Errorf("matching aux Stop calls = %v, want [c-match]", got)
			}
		})
	}
}

// A List error on an auxiliary runtime with no match anywhere is an explicit
// failure, not the idempotent 202: the failing runtime may be the one that
// holds the agent, so "not found" cannot be claimed.
func TestStop_AuxListErrorWithNoMatch_ExplicitErrorNot202(t *testing.T) {
	defaultStops := &stopRecorder{}
	srv := newPlainDockerBroker(t, defaultStops)
	addAuxRuntime(t, srv, "kubernetes", failingAuxRuntime("kubernetes"))

	w := doStop(srv)
	if w.Code < 500 {
		t.Fatalf("status=%d body=%s, want 5xx (an unlistable runtime may hold the agent)", w.Code, w.Body.String())
	}
	if got := defaultStops.calls(); len(got) != 0 {
		t.Errorf("default Stop calls = %v, want none", got)
	}
}

// The same rule holds for LookupContainerID itself (and so for every caller
// that uses it): no match plus an auxiliary List error wraps
// ErrAgentListUnavailable and is not ErrAgentNotFound, while a genuinely
// absent agent with every runtime listable is ErrAgentNotFound.
func TestLookupContainerID_AuxListErrorWithNoMatch_IsListUnavailable(t *testing.T) {
	srv := newPlainDockerBroker(t, &stopRecorder{})
	addAuxRuntime(t, srv, "kubernetes", failingAuxRuntime("kubernetes"))

	_, err := srv.LookupContainerID(context.Background(), "dev", stopLookupProjectID)
	if !errors.Is(err, ErrAgentListUnavailable) || errors.Is(err, ErrAgentNotFound) {
		t.Errorf("err = %v, want ErrAgentListUnavailable and not ErrAgentNotFound", err)
	}

	clean := newPlainDockerBroker(t, &stopRecorder{})
	addAuxRuntime(t, clean, "kubernetes", &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }})
	if _, err := clean.LookupContainerID(context.Background(), "dev", stopLookupProjectID); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("absent agent, all runtimes listable: err = %v, want ErrAgentNotFound", err)
	}
}
