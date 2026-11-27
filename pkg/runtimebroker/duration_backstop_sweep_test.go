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
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type backstopFakeRuntime struct {
	scionrt.Runtime
	name       string
	mu         sync.Mutex
	agents     []api.AgentInfo
	listErr    error
	listFn     func(ctx context.Context) ([]api.AgentInfo, error)
	lastFilter map[string]string
	stopErr    error
	stopped    []scionrt.RunRef
}

func (f *backstopFakeRuntime) Name() string { return f.name }

func (f *backstopFakeRuntime) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	f.mu.Lock()
	f.lastFilter = make(map[string]string, len(filter))
	for k, v := range filter {
		f.lastFilter[k] = v
	}
	f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]api.AgentInfo, len(f.agents))
	copy(out, f.agents)
	return out, nil
}

func (f *backstopFakeRuntime) Stop(_ context.Context, ref scionrt.RunRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, ref)
	if f.stopErr != nil {
		return f.stopErr
	}
	for i := range f.agents {
		if f.agents[i].ContainerID == ref.ID || scionrt.AgentOperationID(f.agents[i]) == ref.ID {
			f.agents[i].Phase = string(state.PhaseStopped)
		}
	}
	return nil
}

func (f *backstopFakeRuntime) stoppedRefs() []scionrt.RunRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]scionrt.RunRef, len(f.stopped))
	copy(out, f.stopped)
	return out
}

func TestDurationBackstopSweeper_StopsExpiredAndReportsLimitsExceeded(t *testing.T) {
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	expired := now.Add(-1 * time.Minute).Format(time.RFC3339)
	future := now.Add(5 * time.Minute).Format(time.RFC3339)

	dockerRT := &backstopFakeRuntime{
		name: "docker",
		agents: []api.AgentInfo{
			{
				ID:          "agent-expired-label",
				Name:        "expired-docker",
				ContainerID: "ctr-1",
				RunID:       "run-1",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{"scion.name": "expired-docker", api.LabelRunDeadline: expired},
			},
			{
				ID:          "agent-future",
				Name:        "future-docker",
				ContainerID: "ctr-2",
				RunID:       "run-2",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{"scion.name": "future-docker", api.LabelRunDeadline: future},
			},
			{
				ID:          "agent-no-deadline",
				Name:        "no-deadline-docker",
				ContainerID: "ctr-3",
				RunID:       "run-3",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{"scion.name": "no-deadline-docker"},
			},
		},
	}
	k8sRT := &backstopFakeRuntime{
		name: "kubernetes",
		agents: []api.AgentInfo{
			{
				ID:          "agent-expired-annot",
				Name:        "expired-k8s",
				ContainerID: "pod-1",
				RunID:       "run-k8s-1",
				Phase:       string(state.PhaseRunning),
				Annotations: map[string]string{api.LabelRunDeadline: expired},
				Kubernetes: &api.AgentK8sMetadata{
					Namespace: "other-ns",
					PodName:   "pod-1",
				},
			},
		},
	}

	var (
		mu       sync.Mutex
		reported []api.AgentInfo
	)
	sw := newDurationBackstopSweeper(
		func() []scionrt.Runtime { return []scionrt.Runtime{dockerRT, k8sRT} },
		func(_ context.Context, a api.AgentInfo) {
			mu.Lock()
			reported = append(reported, a)
			mu.Unlock()
		},
		slog.Default(),
	)
	sw.now = func() time.Time { return now }

	sw.sweep(context.Background())

	dockerStops := dockerRT.stoppedRefs()
	if len(dockerStops) != 1 || dockerStops[0].ID != "ctr-1" || dockerStops[0].RunID != "run-1" {
		t.Fatalf("docker stops = %+v, want [{ID:ctr-1 RunID:run-1}]", dockerStops)
	}
	k8sStops := k8sRT.stoppedRefs()
	if len(k8sStops) != 1 || k8sStops[0].ID != "other-ns/pod-1" || k8sStops[0].RunID != "run-k8s-1" {
		t.Fatalf("k8s stops = %+v, want [{ID:other-ns/pod-1 RunID:run-k8s-1}]", k8sStops)
	}
	if len(reported) != 2 {
		t.Fatalf("reported count = %d, want 2: %+v", len(reported), reported)
	}
	if dockerRT.lastFilter["scion.agent"] != "true" || len(dockerRT.lastFilter) != 1 {
		t.Errorf("docker List filter = %v, want map[scion.agent:true]", dockerRT.lastFilter)
	}
	if k8sRT.lastFilter["scion.agent"] != "true" || len(k8sRT.lastFilter) != 1 {
		t.Errorf("k8s List filter = %v, want map[scion.agent:true]", k8sRT.lastFilter)
	}
}

func TestDurationBackstopSweeper_NoDoubleStopWhenSciontoolExitedFirst(t *testing.T) {
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	expired := now.Add(-2 * time.Minute).Format(time.RFC3339)

	t.Run("already stopped phase is skipped", func(t *testing.T) {
		rt := &backstopFakeRuntime{
			name: "docker",
			agents: []api.AgentInfo{
				{
					Name:            "clean-exit",
					ContainerID:     "ctr-done",
					RunID:           "run-done",
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "Exited (10) 5 seconds ago",
					Labels:          map[string]string{api.LabelRunDeadline: expired},
				},
				{
					Name:            "legacy-exited",
					ContainerID:     "ctr-legacy",
					RunID:           "run-legacy",
					ContainerStatus: "Exited (0) 2 seconds ago",
					Labels:          map[string]string{api.LabelRunDeadline: expired},
				},
			},
		}
		reports := 0
		sw := newDurationBackstopSweeper(
			func() []scionrt.Runtime { return []scionrt.Runtime{rt} },
			func(context.Context, api.AgentInfo) { reports++ },
			slog.Default(),
		)
		sw.now = func() time.Time { return now }
		sw.sweep(context.Background())

		if len(rt.stoppedRefs()) != 0 {
			t.Errorf("expected 0 stops for already-stopped agents, got %+v", rt.stoppedRefs())
		}
		if reports != 0 {
			t.Errorf("expected 0 reports for already-stopped agents, got %d", reports)
		}
	})

	t.Run("ErrRunConflict, ErrRunMismatch, or already-stopped Stop error emits no report and logs at Debug", func(t *testing.T) {
		for _, stopErr := range []error{
			scionrt.ErrRunConflict,
			scionrt.ErrRunMismatch,
			errors.New("Error: No such container: ctr-gone"),
		} {
			rt := &backstopFakeRuntime{
				name:    "docker",
				stopErr: stopErr,
				agents: []api.AgentInfo{
					{
						Name:        "raced-agent",
						ContainerID: "ctr-gone",
						RunID:       "run-old",
						Phase:       string(state.PhaseRunning),
						Labels:      map[string]string{api.LabelRunDeadline: expired},
					},
				},
			}
			var logRecs []slog.Record
			captureHandler := &backstopCaptureHandler{records: &logRecs}
			reports := 0
			sw := newDurationBackstopSweeper(
				func() []scionrt.Runtime { return []scionrt.Runtime{rt} },
				func(context.Context, api.AgentInfo) { reports++ },
				slog.New(captureHandler),
			)
			sw.now = func() time.Time { return now }
			sw.sweep(context.Background())

			if reports != 0 {
				t.Errorf("stopErr=%v: expected 0 reports, got %d", stopErr, reports)
			}
			for _, rec := range logRecs {
				if rec.Level >= slog.LevelError {
					t.Errorf("stopErr=%v: unexpected Error log %q", stopErr, rec.Message)
				}
			}
		}
	})
}

func TestDurationBackstopSweeper_SurvivesBrokerRestart(t *testing.T) {
	launch := time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)
	deadline := launch.Add(2*time.Minute + api.DurationBackstopTotalGrace) // 2m max_duration + 12m total grace

	rt := &backstopFakeRuntime{
		name: "docker",
		agents: []api.AgentInfo{
			{
				ID:          "agent-restart",
				Name:        "restart-agent",
				ContainerID: "ctr-restart",
				RunID:       "run-restart",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{api.LabelRunDeadline: deadline.Format(time.RFC3339)},
			},
		},
	}

	// Simulate broker 1 sweeping during a slow start (launch+6m: past 2m+2m,
	// still within 2m+12m): nothing stopped.
	reports1 := 0
	sw1 := newDurationBackstopSweeper(
		func() []scionrt.Runtime { return []scionrt.Runtime{rt} },
		func(context.Context, api.AgentInfo) { reports1++ },
		slog.Default(),
	)
	sw1.now = func() time.Time { return launch.Add(6 * time.Minute) }
	sw1.sweep(context.Background())
	if len(rt.stoppedRefs()) != 0 || reports1 != 0 {
		t.Fatalf("before deadline: stops=%d reports=%d, want 0/0", len(rt.stoppedRefs()), reports1)
	}

	// Simulate broker restart: brand-new sweeper with zero in-memory state,
	// now past the 14m deadline.
	reports2 := 0
	sw2 := newDurationBackstopSweeper(
		func() []scionrt.Runtime { return []scionrt.Runtime{rt} },
		func(context.Context, api.AgentInfo) { reports2++ },
		slog.Default(),
	)
	sw2.now = func() time.Time { return launch.Add(15 * time.Minute) }
	sw2.sweep(context.Background())
	if len(rt.stoppedRefs()) != 1 || reports2 != 1 {
		t.Fatalf("after restart past deadline: stops=%d reports=%d, want 1/1", len(rt.stoppedRefs()), reports2)
	}
}

func TestDurationBackstopSweeper_SlowOrFailingRuntimeDoesNotBlockOthers(t *testing.T) {
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	expired := now.Add(-1 * time.Minute).Format(time.RFC3339)

	failingRT := &backstopFakeRuntime{
		name:    "broken-k8s",
		listErr: errors.New("apiserver unreachable"),
	}
	healthyRT := &backstopFakeRuntime{
		name: "docker",
		agents: []api.AgentInfo{
			{
				Name:        "healthy-expired",
				ContainerID: "ctr-ok",
				RunID:       "run-ok",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{api.LabelRunDeadline: expired},
			},
		},
	}

	reports := 0
	sw := newDurationBackstopSweeper(
		func() []scionrt.Runtime { return []scionrt.Runtime{failingRT, healthyRT} },
		func(context.Context, api.AgentInfo) { reports++ },
		slog.Default(),
	)
	sw.now = func() time.Time { return now }
	sw.sweep(context.Background())

	if len(healthyRT.stoppedRefs()) != 1 || reports != 1 {
		t.Fatalf("healthy runtime stops=%d reports=%d, want 1/1", len(healthyRT.stoppedRefs()), reports)
	}
}

func TestServer_ReportDurationBackstopStop_NoHubConnectionDropsCleanly(t *testing.T) {
	s := &Server{}
	// Must not panic or block when no Hub connections are registered.
	s.reportDurationBackstopStop(context.Background(), api.AgentInfo{
		Name:      "standalone-agent",
		ProjectID: "proj-1",
		RunID:     "run-1",
	})
}

func TestDurationBackstopSweeper_StopsExpiredSubstrateRun(t *testing.T) {
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	expired := now.Add(-1 * time.Minute).Format(time.RFC3339)

	subRT := &backstopFakeRuntime{
		name: "substrate",
		agents: []api.AgentInfo{
			{
				ID:          "agent-sub-1",
				Name:        "sub-agent",
				ContainerID: "scion-proj/sub-agent",
				RunID:       "run-sub-1",
				Phase:       string(state.PhaseRunning),
				Labels: map[string]string{
					"scion.agent":        "true",
					"scion.name":         "sub-agent",
					api.LabelRunID:       "run-sub-1",
					api.LabelRunDeadline: expired,
				},
			},
		},
	}

	var reported []api.AgentInfo
	sw := newDurationBackstopSweeper(
		func() []scionrt.Runtime { return []scionrt.Runtime{subRT} },
		func(_ context.Context, a api.AgentInfo) { reported = append(reported, a) },
		slog.Default(),
	)
	sw.now = func() time.Time { return now }
	sw.sweep(context.Background())

	if subRT.lastFilter["scion.agent"] != "true" || len(subRT.lastFilter) != 1 {
		t.Errorf("substrate List filter = %v, want map[scion.agent:true]", subRT.lastFilter)
	}
	stops := subRT.stoppedRefs()
	if len(stops) != 1 || stops[0].ID != "scion-proj/sub-agent" || stops[0].RunID != "run-sub-1" {
		t.Fatalf("substrate stops = %+v, want [{ID:scion-proj/sub-agent RunID:run-sub-1}]", stops)
	}
	if len(reported) != 1 || reported[0].Name != "sub-agent" || reported[0].RunID != "run-sub-1" {
		t.Fatalf("reported = %+v, want 1 report for sub-agent/run-sub-1", reported)
	}
}

type backstopCaptureHandler struct {
	mu      sync.Mutex
	records *[]slog.Record
}

func (h *backstopCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *backstopCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.records = append(*h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *backstopCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *backstopCaptureHandler) WithGroup(string) slog.Handler      { return h }
