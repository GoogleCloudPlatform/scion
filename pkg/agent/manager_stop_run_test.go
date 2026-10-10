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

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Run-scoped Stop (ptone/scion#2550 P3).

func stopRunRuntime(entries []api.AgentInfo, stopped *[]runtime.RunRef) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return entries, nil
		},
		StopFunc: func(_ context.Context, ref runtime.RunRef) error {
			*stopped = append(*stopped, ref)
			return nil
		},
	}
}

func TestStop_RunIDMatchStopsThatEntry(t *testing.T) {
	var stopped []runtime.RunRef
	mgr := &AgentManager{Runtime: stopRunRuntime([]api.AgentInfo{
		{Name: "dev", ContainerID: "cid-b", RunID: "run-b"},
	}, &stopped)}

	if err := mgr.Stop(context.Background(), "dev", "", "run-b"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := runtime.RunRef{ID: "cid-b", RunID: "run-b"}
	if len(stopped) != 1 || stopped[0] != want {
		t.Fatalf("stopped = %v, want [%v]", stopped, want)
	}
}

func TestStop_RunIDMismatchIsNotFoundWithoutStopOrFallback(t *testing.T) {
	var stopped []runtime.RunRef
	mgr := &AgentManager{Runtime: stopRunRuntime([]api.AgentInfo{
		// The name now belongs to run B, recreated after run A.
		{Name: "dev", ContainerID: "cid-b", RunID: "run-b"},
	}, &stopped)}

	err := mgr.Stop(context.Background(), "dev", "", "run-a")
	if !errors.Is(err, ErrStopRunNotFound) {
		t.Fatalf("Stop err = %v, want ErrStopRunNotFound", err)
	}
	// No Stop at all: neither run B's container nor the bare name.
	if len(stopped) != 0 {
		t.Fatalf("stale run-scoped stop reached the runtime: %v", stopped)
	}
}

func TestStop_RunIDNoEntryIsNotFoundWithoutNameFallback(t *testing.T) {
	var stopped []runtime.RunRef
	mgr := &AgentManager{Runtime: stopRunRuntime(nil, &stopped)}

	err := mgr.Stop(context.Background(), "dev", "", "run-a")
	if !errors.Is(err, ErrStopRunNotFound) {
		t.Fatalf("Stop err = %v, want ErrStopRunNotFound", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("run-scoped stop fell back to the bare name: %v", stopped)
	}
}

func TestStop_RunIDListErrorDoesNotFallBack(t *testing.T) {
	var stopped []runtime.RunRef
	rt := stopRunRuntime(nil, &stopped)
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, errors.New("list failed")
	}
	mgr := &AgentManager{Runtime: rt}

	if err := mgr.Stop(context.Background(), "dev", "", "run-a"); err == nil {
		t.Fatal("expected the list error, got nil")
	}
	if len(stopped) != 0 {
		t.Fatalf("run-scoped stop fell back to the bare name after a list error: %v", stopped)
	}
}

func TestStop_RunIDPrefersExactRunAndAcceptsLegacyEntry(t *testing.T) {
	t.Run("exact run wins over a legacy entry", func(t *testing.T) {
		var stopped []runtime.RunRef
		mgr := &AgentManager{Runtime: stopRunRuntime([]api.AgentInfo{
			{Name: "dev", ContainerID: "cid-legacy"},
			{Name: "dev", ContainerID: "cid-a", RunID: "run-a"},
		}, &stopped)}
		if err := mgr.Stop(context.Background(), "dev", "", "run-a"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(stopped) != 1 || stopped[0].ID != "cid-a" {
			t.Fatalf("stopped = %v, want cid-a", stopped)
		}
	})
	t.Run("legacy unlabelled entry still matches by name", func(t *testing.T) {
		var stopped []runtime.RunRef
		mgr := &AgentManager{Runtime: stopRunRuntime([]api.AgentInfo{
			{Name: "dev", ContainerID: "cid-legacy"},
		}, &stopped)}
		if err := mgr.Stop(context.Background(), "dev", "", "run-a"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(stopped) != 1 || stopped[0].ID != "cid-legacy" {
			t.Fatalf("stopped = %v, want cid-legacy", stopped)
		}
	})
}

func TestStop_EmptyRunIDUnchanged(t *testing.T) {
	t.Run("resolves by name and ignores run labels", func(t *testing.T) {
		var stopped []runtime.RunRef
		mgr := &AgentManager{Runtime: stopRunRuntime([]api.AgentInfo{
			{Name: "dev", ContainerID: "cid-b", RunID: "run-b"},
		}, &stopped)}
		if err := mgr.Stop(context.Background(), "dev", "", ""); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(stopped) != 1 || stopped[0].ID != "cid-b" {
			t.Fatalf("stopped = %v, want cid-b", stopped)
		}
	})
	t.Run("falls back to the bare name", func(t *testing.T) {
		var stopped []runtime.RunRef
		mgr := &AgentManager{Runtime: stopRunRuntime(nil, &stopped)}
		if err := mgr.Stop(context.Background(), "abc123", "", ""); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		want := runtime.RunRef{ID: "abc123"}
		if len(stopped) != 1 || stopped[0] != want {
			t.Fatalf("stopped = %v, want [%v]", stopped, want)
		}
	})
}

func TestStopTarget_StopsResolvedEntryWithoutList(t *testing.T) {
	var stopped []runtime.RunRef
	rt := stopRunRuntime(nil, &stopped)
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		t.Fatal("StopTarget must not re-resolve by name")
		return nil, nil
	}
	mgr := &AgentManager{Runtime: rt}
	ref := runtime.RunRef{ID: "cid-b", RunID: "run-b"}
	if err := mgr.StopTarget(context.Background(), ref); err != nil {
		t.Fatalf("StopTarget: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != ref {
		t.Fatalf("stopped = %v, want [%v]", stopped, ref)
	}
}
