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
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// inspectingRuntime is a mock runtime that reports scripted binds.
type inspectingRuntime struct {
	*runtime.MockRuntime
	results []inspectResult
	calls   int
	stopped []runtime.RunRef
}

type inspectResult struct {
	binds []runtime.BindMount
	err   error
}

// InspectBindMounts reports the scripted binds as both the runtime's view
// and the broker's record (runtime.LabelBindSources), so they match.
func (r *inspectingRuntime) InspectBindMounts(context.Context, string) (runtime.InspectedBinds, error) {
	res := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	if res.err != nil {
		return runtime.InspectedBinds{}, res.err
	}
	return runtime.InspectedBinds{Reported: res.binds, ReportedKnown: true, Labels: bindRecord(res.binds)}, nil
}

// bindRecord is the runtime.LabelBindSources label for binds (base64 of
// their JSON).
func bindRecord(binds []runtime.BindMount) map[string]string {
	data, _ := json.Marshal(binds)
	return map[string]string{runtime.LabelBindSources: base64.StdEncoding.EncodeToString(data)}
}

func newInspectingRuntime(results ...inspectResult) *inspectingRuntime {
	r := &inspectingRuntime{results: results}
	r.MockRuntime = &runtime.MockRuntime{StopFunc: func(_ context.Context, ref runtime.RunRef) error {
		r.stopped = append(r.stopped, ref)
		return nil
	}}
	return r
}

func controlPathHome(t *testing.T) (global, outside string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	global = filepath.Join(home, ".scion")
	outside = filepath.Join(root, "data")
	for _, d := range []string{global, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return global, outside
}

func TestVerifyCreatedMounts(t *testing.T) {
	global, outside := controlPathHome(t)
	ref := runtime.RunRef{ID: "c1", RunID: "r1"}
	clean := inspectResult{binds: []runtime.BindMount{{Source: outside, Destination: "/x"}}}
	bad := inspectResult{binds: []runtime.BindMount{{Source: global, Destination: "/g"}}}
	failed := inspectResult{err: runtime.ErrMountsUnverifiable}

	t.Run("clean binds", func(t *testing.T) {
		rt := newInspectingRuntime(clean)
		if err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(context.Background(), ref, "a"); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(rt.stopped) != 0 {
			t.Fatalf("stopped a clean agent")
		}
	})
	t.Run("refused bind stops at once", func(t *testing.T) {
		rt := newInspectingRuntime(bad)
		err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(context.Background(), ref, "a")
		if !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Fatalf("err = %v", err)
		}
		if rt.calls != 1 || len(rt.stopped) != 1 || rt.stopped[0] != ref {
			t.Fatalf("calls=%d stopped=%v", rt.calls, rt.stopped)
		}
	})
	t.Run("one inspect failure is retried", func(t *testing.T) {
		rt := newInspectingRuntime(failed, clean)
		if err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(context.Background(), ref, "a"); err != nil {
			t.Fatalf("err = %v", err)
		}
		if rt.calls != 2 || len(rt.stopped) != 0 {
			t.Fatalf("calls=%d stopped=%v", rt.calls, rt.stopped)
		}
	})
	t.Run("two inspect failures stop", func(t *testing.T) {
		rt := newInspectingRuntime(failed, failed)
		err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(context.Background(), ref, "a")
		if !errors.Is(err, brokercontrol.ErrControlPathMount) {
			t.Fatalf("err = %v", err)
		}
		if len(rt.stopped) != 1 {
			t.Fatalf("stopped=%v", rt.stopped)
		}
	})
	t.Run("runtime without inspection", func(t *testing.T) {
		if err := (&AgentManager{Runtime: &runtime.MockRuntime{}}).verifyCreatedMounts(context.Background(), ref, "a"); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

// unrecordedRuntime is a mock runtime whose containers report a bind but
// carry no record of the binds the broker requested.
type unrecordedRuntime struct {
	*runtime.MockRuntime
	stopped []runtime.RunRef
}

func (r *unrecordedRuntime) InspectBindMounts(context.Context, string) (runtime.InspectedBinds, error) {
	return runtime.InspectedBinds{Reported: []runtime.BindMount{{Source: "/tmp", Destination: "/x"}}, ReportedKnown: true}, nil
}

// TestStart_VerifiesCreatedContainerMounts checks that AgentManager.Start
// runs the post-create verification: a container whose binds cannot be
// verified is stopped and the start fails.
func TestStart_VerifiesCreatedContainerMounts(t *testing.T) {
	_, projectScionDir := newResumePhaseTestFixture(t, nil)
	rt := &unrecordedRuntime{}
	rt.MockRuntime = &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc:  func(context.Context, runtime.RunConfig) (string, error) { return "mock-id", nil },
		StopFunc: func(_ context.Context, ref runtime.RunRef) error {
			rt.stopped = append(rt.stopped, ref)
			return nil
		},
	}
	_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      true,
	})
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("Start err = %v, want ErrControlPathMount", err)
	}
	if len(rt.stopped) != 1 || rt.stopped[0].ID != "mock-id" {
		t.Fatalf("stopped = %v, want the created container", rt.stopped)
	}
}

// TestVerifyCreatedMounts_StopFailureIsReported: when the bounded stop of a
// refused container fails, the error says the container may still be
// running rather than that it was stopped.
func TestVerifyCreatedMounts_StopFailureIsReported(t *testing.T) {
	global, _ := controlPathHome(t)
	rt := newInspectingRuntime(inspectResult{binds: []runtime.BindMount{{Source: global, Destination: "/g"}}})
	rt.StopFunc = func(context.Context, runtime.RunRef) error { return errors.New("daemon not responding") }
	err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(context.Background(), runtime.RunRef{ID: "c1"}, "a")
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be stopped") || !strings.Contains(msg, "daemon not responding") || strings.Contains(msg, "agent \"a\" stopped") {
		t.Fatalf("error does not report the failed stop truthfully: %s", msg)
	}
}

// TestVerifyCreatedMounts_StopOutlivesCancelledRequest: a refused container
// is stopped with a live, bounded context even when the request context is
// already cancelled.
func TestVerifyCreatedMounts_StopOutlivesCancelledRequest(t *testing.T) {
	global, _ := controlPathHome(t)
	rt := newInspectingRuntime(inspectResult{binds: []runtime.BindMount{{Source: global, Destination: "/g"}}})
	var stopErr error
	var hasDeadline bool
	rt.StopFunc = func(ctx context.Context, ref runtime.RunRef) error {
		stopErr = ctx.Err()
		_, hasDeadline = ctx.Deadline()
		rt.stopped = append(rt.stopped, ref)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&AgentManager{Runtime: rt}).verifyCreatedMounts(ctx, runtime.RunRef{ID: "c1"}, "a")
	if !errors.Is(err, brokercontrol.ErrControlPathMount) {
		t.Fatalf("err = %v, want ErrControlPathMount", err)
	}
	if len(rt.stopped) != 1 {
		t.Fatalf("stopped = %v, want one stop", rt.stopped)
	}
	if stopErr != nil {
		t.Fatalf("Stop got a done context (%v); it must not inherit the request's cancellation", stopErr)
	}
	if !hasDeadline {
		t.Fatal("Stop got a context without a deadline; the stop must stay bounded")
	}
}
