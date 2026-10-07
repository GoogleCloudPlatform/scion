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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Run-scoped Cloud Run Stop, Delete and Run (ptone/scion#2550 P4).
//
// statefulInstances is a cloudrun.InstancesAPI double that keeps instances
// by resource name, bumps an etag on every write, and enforces the etag a
// Stop, Start or Delete carries the way the API documents it (ABORTED on a
// mismatch), so the run check and the etag precondition can be exercised
// together.

type statefulInstances struct {
	instances map[string]*runpb.Instance
	etagSeq   int

	// beforeWrite, if set, runs before each Stop, Start or Delete is
	// applied (after the runtime's read), to model a concurrent change.
	beforeWrite func(s *statefulInstances, name string)

	// failWrite, if set, is returned by every Stop, Start or Delete
	// without changing the instance (a refusal that is not a change).
	failWrite error

	gets, creates, starts, stops, deletes int
	lastEtag                              string
}

func newStatefulInstances() *statefulInstances {
	return &statefulInstances{instances: map[string]*runpb.Instance{}}
}

func (s *statefulInstances) nextEtag() string {
	s.etagSeq++
	return fmt.Sprintf("etag-%d", s.etagSeq)
}

// put stores an instance labelled with runID (none if "") under name.
func (s *statefulInstances) put(name, runID string) {
	labels := map[string]string{"agent_id": "agent-1"}
	if runID != "" {
		labels[sanitizeGCPLabelKey(api.LabelRunID)] = sanitizeGCPLabelValue(runID)
	}
	s.instances[name] = &runpb.Instance{Name: name, Labels: labels, Etag: s.nextEtag()}
}

func (s *statefulInstances) runOf(name string) (string, bool) {
	inst, ok := s.instances[name]
	if !ok {
		return "", false
	}
	return inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)], true
}

func (s *statefulInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	s.gets++
	inst, ok := s.instances[req.Name]
	if !ok {
		return nil, status.Error(codes.NotFound, "instance does not exist")
	}
	return proto.Clone(inst).(*runpb.Instance), nil
}

func (s *statefulInstances) CreateInstance(_ context.Context, req *runpb.CreateInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	s.creates++
	name := req.Parent + "/instances/" + req.InstanceId
	if _, ok := s.instances[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "instance exists")
	}
	inst := proto.Clone(req.Instance).(*runpb.Instance)
	inst.Name = name
	inst.Etag = s.nextEtag()
	s.instances[name] = inst
	return &fakeInstanceOperation{}, nil
}

// write applies a Stop, Start or Delete with its etag precondition.
func (s *statefulInstances) write(name, etag string, apply func()) (cloudrun.InstanceOperation, error) {
	if s.beforeWrite != nil {
		s.beforeWrite(s, name)
	}
	s.lastEtag = etag
	if s.failWrite != nil {
		return nil, s.failWrite
	}
	inst, ok := s.instances[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "instance does not exist")
	}
	if etag != "" && etag != inst.Etag {
		return nil, status.Error(codes.Aborted, "etag mismatch")
	}
	apply()
	return &fakeInstanceOperation{}, nil
}

func (s *statefulInstances) StartInstance(_ context.Context, req *runpb.StartInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	s.starts++
	return s.write(req.Name, req.Etag, func() { s.instances[req.Name].Etag = s.nextEtag() })
}

func (s *statefulInstances) StopInstance(_ context.Context, req *runpb.StopInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	s.stops++
	return s.write(req.Name, req.Etag, func() { s.instances[req.Name].Etag = s.nextEtag() })
}

func (s *statefulInstances) DeleteInstance(_ context.Context, req *runpb.DeleteInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	s.deletes++
	return s.write(req.Name, req.Etag, func() { delete(s.instances, req.Name) })
}

func (s *statefulInstances) ListInstances(_ context.Context, _ *runpb.ListInstancesRequest, _ ...gax.CallOption) cloudrun.InstanceIterator {
	var out []*runpb.Instance
	for _, inst := range s.instances {
		out = append(out, proto.Clone(inst).(*runpb.Instance))
	}
	return &fakeInstanceIterator{instances: out}
}

func (s *statefulInstances) Close() error { return nil }

func newStatefulCloudRunRuntime(t *testing.T, s *statefulInstances) *CloudRunRuntime {
	t.Helper()
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{})
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return s, nil }
	return rt
}

const (
	crInstanceID = "agent-dev-0123456789"
	crName       = "projects/test-project/locations/us-central1/instances/" + crInstanceID
)

type crOp struct {
	name   string
	call   func(rt *CloudRunRuntime, ref RunRef) error
	writes func(s *statefulInstances) int
}

var crOps = []crOp{
	{"Stop", func(rt *CloudRunRuntime, ref RunRef) error { return rt.Stop(context.Background(), ref) },
		func(s *statefulInstances) int { return s.stops }},
	{"Delete", func(rt *CloudRunRuntime, ref RunRef) error { return rt.Delete(context.Background(), ref) },
		func(s *statefulInstances) int { return s.deletes }},
}

// A stop or delete for run A after run B took the instance ID leaves run
// B's instance untouched and reports ErrRunMismatch (which the broker maps
// to its run-mismatch answer), issuing no Stop or Delete call.
func TestCloudRunRunScoped_OtherRunUntouched(t *testing.T) {
	for _, op := range crOps {
		t.Run(op.name, func(t *testing.T) {
			s := newStatefulInstances()
			s.put(crName, "run-b")
			rt := newStatefulCloudRunRuntime(t, s)

			err := op.call(rt, RunRef{ID: crInstanceID, RunID: "run-a"})
			if !errors.Is(err, ErrRunMismatch) {
				t.Fatalf("%s = %v, want ErrRunMismatch", op.name, err)
			}
			if n := op.writes(s); n != 0 {
				t.Errorf("%sInstance called %d times, want 0", op.name, n)
			}
			if run, ok := s.runOf(crName); !ok || run != "run-b" {
				t.Errorf("run B's instance: present=%v run=%q, want present run-b", ok, run)
			}
		})
	}
}

// The run's own instance, and a legacy instance with no run label, are
// stopped or deleted, with the read's etag as the precondition. The run ID
// is compared after GCP label sanitising, as Run stored it ("Run.A" is
// stored as "run_a").
func TestCloudRunRunScoped_OwnAndLegacyInstance(t *testing.T) {
	cases := []struct{ label, runID string }{
		{"run-a", "run-a"},
		{"", "run-a"},
		{"Run.A", "Run.A"},
	}
	for _, op := range crOps {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/label=%q", op.name, tc.label), func(t *testing.T) {
				s := newStatefulInstances()
				s.put(crName, tc.label)
				etag := s.instances[crName].Etag
				rt := newStatefulCloudRunRuntime(t, s)

				if err := op.call(rt, RunRef{ID: crInstanceID, RunID: tc.runID}); err != nil {
					t.Fatalf("%s: %v", op.name, err)
				}
				if n := op.writes(s); n != 1 {
					t.Fatalf("%sInstance called %d times, want 1", op.name, n)
				}
				if s.lastEtag != etag {
					t.Errorf("%sInstance etag = %q, want the read's %q", op.name, s.lastEtag, etag)
				}
			})
		}
	}
}

// A run-scoped stop or delete of a missing instance gives the API's
// NotFound, as the name-based call does.
func TestCloudRunRunScoped_MissingInstanceNotFound(t *testing.T) {
	for _, op := range crOps {
		t.Run(op.name, func(t *testing.T) {
			s := newStatefulInstances()
			rt := newStatefulCloudRunRuntime(t, s)

			err := op.call(rt, RunRef{ID: crInstanceID, RunID: "run-a"})
			var se interface{ GRPCStatus() *status.Status }
			if !errors.As(err, &se) || se.GRPCStatus().Code() != codes.NotFound {
				t.Fatalf("%s = %v, want a gRPC NotFound", op.name, err)
			}
			if n := op.writes(s); n != 0 {
				t.Errorf("%sInstance called %d times, want 0", op.name, n)
			}
		})
	}
}

// A refusal that is not a change (FAILED_PRECONDITION or ABORTED while
// the etag is still the one sent, e.g. a state precondition) is returned
// after one call, not retried as a change.
func TestCloudRunRunScoped_UnchangedEtagRefusalNotRetried(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.Aborted} {
		for _, op := range crOps {
			t.Run(code.String()+"/"+op.name, func(t *testing.T) {
				s := newStatefulInstances()
				s.put(crName, "run-a")
				s.failWrite = status.Error(code, "refused")
				rt := newStatefulCloudRunRuntime(t, s)

				err := op.call(rt, RunRef{ID: crInstanceID, RunID: "run-a"})
				if status.Code(errors.Unwrap(err)) != code {
					t.Fatalf("%s = %v, want the %s refusal", op.name, err, code)
				}
				if n := op.writes(s); n != 1 {
					t.Errorf("%sInstance called %d times, want 1", op.name, n)
				}
			})
		}
	}
}

// An empty run ID behaves as before run IDs existed: no read, no etag, the
// call is made by name whatever run holds the instance.
func TestCloudRunRunScoped_EmptyRunIDByName(t *testing.T) {
	for _, op := range crOps {
		t.Run(op.name, func(t *testing.T) {
			s := newStatefulInstances()
			s.put(crName, "run-b")
			rt := newStatefulCloudRunRuntime(t, s)

			if err := op.call(rt, RunRef{ID: crInstanceID}); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
			if s.gets != 0 || op.writes(s) != 1 || s.lastEtag != "" {
				t.Errorf("gets=%d writes=%d etag=%q, want 0, 1, \"\"", s.gets, op.writes(s), s.lastEtag)
			}
		})
	}
}

// Run B replaces the instance between the run check and the call: the etag
// precondition refuses the call, the re-read finds run B, and run B's
// instance survives with ErrRunMismatch.
func TestCloudRunRunScoped_ReplacedAfterReadRefusedByEtag(t *testing.T) {
	for _, op := range crOps {
		t.Run(op.name, func(t *testing.T) {
			s := newStatefulInstances()
			s.put(crName, "run-a")
			replaced := false
			s.beforeWrite = func(s *statefulInstances, name string) {
				if !replaced {
					replaced = true
					s.put(name, "run-b")
				}
			}
			rt := newStatefulCloudRunRuntime(t, s)

			err := op.call(rt, RunRef{ID: crInstanceID, RunID: "run-a"})
			if !errors.Is(err, ErrRunMismatch) {
				t.Fatalf("%s = %v, want ErrRunMismatch", op.name, err)
			}
			if run, ok := s.runOf(crName); !ok || run != "run-b" {
				t.Errorf("run B's instance: present=%v run=%q, want present run-b", ok, run)
			}
			if s.gets != 2 {
				t.Errorf("GetInstance called %d times, want 2 (read, re-read)", s.gets)
			}
		})
	}
}

// An etag change that is not a new run (the same run's instance changed)
// is retried after a re-read and then succeeds; a change on every attempt
// gives up after cloudRunRunCheckAttempts with a plain error.
func TestCloudRunRunScoped_EtagRetryBounded(t *testing.T) {
	t.Run("same run changed once", func(t *testing.T) {
		s := newStatefulInstances()
		s.put(crName, "run-a")
		bumped := false
		s.beforeWrite = func(s *statefulInstances, name string) {
			if !bumped {
				bumped = true
				s.instances[name].Etag = s.nextEtag()
			}
		}
		rt := newStatefulCloudRunRuntime(t, s)
		if err := rt.Delete(context.Background(), RunRef{ID: crInstanceID, RunID: "run-a"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok := s.instances[crName]; ok {
			t.Error("instance still present after a retried delete")
		}
	})
	t.Run("changes every time", func(t *testing.T) {
		s := newStatefulInstances()
		s.put(crName, "run-a")
		s.beforeWrite = func(s *statefulInstances, name string) { s.instances[name].Etag = s.nextEtag() }
		rt := newStatefulCloudRunRuntime(t, s)
		err := rt.Delete(context.Background(), RunRef{ID: crInstanceID, RunID: "run-a"})
		if err == nil || errors.Is(err, ErrRunMismatch) {
			t.Fatalf("Delete = %v, want a plain error", err)
		}
		if s.deletes != cloudRunRunCheckAttempts {
			t.Errorf("DeleteInstance called %d times, want %d", s.deletes, cloudRunRunCheckAttempts)
		}
	})
}

// runCfgForRun is runConfigForTest labelled with runID.
func runCfgForRun(runID string) RunConfig {
	cfg := runConfigForTest()
	cfg.Labels = map[string]string{"agent_id": "agent-1", api.LabelRunID: runID}
	return cfg
}

var crAgentName = "projects/test-project/locations/us-central1/instances/" + cloudRunInstanceID("agent-1")

// D1: Run reuses an existing instance only when it is this run's or a
// legacy unlabelled one; another run's instance is refused with
// ErrRunConflict (409 at the broker) and is not started.
func TestCloudRunRun_ReuseIsRunChecked(t *testing.T) {
	t.Run("same run reused", func(t *testing.T) {
		s := newStatefulInstances()
		s.put(crAgentName, "run-b")
		etag := s.instances[crAgentName].Etag
		rt := newStatefulCloudRunRuntime(t, s)
		if _, err := rt.Run(context.Background(), runCfgForRun("run-b")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if s.starts != 1 || s.creates != 0 || s.lastEtag != etag {
			t.Errorf("starts=%d creates=%d etag=%q, want 1, 0, %q", s.starts, s.creates, s.lastEtag, etag)
		}
	})
	t.Run("legacy instance reused unstamped", func(t *testing.T) {
		s := newStatefulInstances()
		s.put(crAgentName, "")
		rt := newStatefulCloudRunRuntime(t, s)
		if _, err := rt.Run(context.Background(), runCfgForRun("run-b")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if s.starts != 1 || s.creates != 0 {
			t.Errorf("starts=%d creates=%d, want 1, 0", s.starts, s.creates)
		}
		agents, err := rt.List(context.Background(), nil)
		if err != nil || len(agents) != 1 {
			t.Fatalf("List = %v, %v; want one entry", agents, err)
		}
		if agents[0].RunID != "" {
			t.Errorf("reused legacy instance reports run %q, want \"\" (legacy)", agents[0].RunID)
		}
	})
	t.Run("other run refused", func(t *testing.T) {
		s := newStatefulInstances()
		s.put(crAgentName, "run-a")
		rt := newStatefulCloudRunRuntime(t, s)
		_, err := rt.Run(context.Background(), runCfgForRun("run-b"))
		if !errors.Is(err, ErrRunConflict) {
			t.Fatalf("Run = %v, want ErrRunConflict", err)
		}
		if s.starts != 0 || s.creates != 0 {
			t.Errorf("starts=%d creates=%d, want 0, 0", s.starts, s.creates)
		}
		if run, _ := s.runOf(crAgentName); run != "run-a" {
			t.Errorf("run A's instance now has run %q, want run-a untouched", run)
		}
	})
}

// Stop then start of the same agent still works: the stop leaves run A's
// instance, Start's pre-clean (pkg/agent/run.go) deletes every listed
// non-running entry with its run, and Run for run B then creates a fresh
// instance labelled run B. Cloud Run's List sets no Phase, so the listed
// instance always counts as not running.
func TestCloudRunRun_StopThenStartNewRun(t *testing.T) {
	s := newStatefulInstances()
	rt := newStatefulCloudRunRuntime(t, s)
	ctx := context.Background()

	idA, err := rt.Run(ctx, runCfgForRun("run-a"))
	if err != nil {
		t.Fatalf("Run A: %v", err)
	}
	if err := rt.Stop(ctx, RunRef{ID: idA, RunID: "run-a"}); err != nil {
		t.Fatalf("Stop A: %v", err)
	}

	// Start's pre-clean, as pkg/agent/run.go does it.
	agents, err := rt.List(ctx, nil)
	if err != nil || len(agents) != 1 {
		t.Fatalf("List = %v, %v; want one entry", agents, err)
	}
	if agents[0].Phase == "running" {
		t.Fatalf("listed phase %q: pre-clean would skip the instance", agents[0].Phase)
	}
	if err := rt.Delete(ctx, RunRef{ID: AgentOperationID(agents[0]), RunID: agents[0].RunID}); err != nil {
		t.Fatalf("pre-clean Delete: %v", err)
	}

	if _, err := rt.Run(ctx, runCfgForRun("run-b")); err != nil {
		t.Fatalf("Run B: %v", err)
	}
	if run, ok := s.runOf(crAgentName); !ok || run != "run-b" {
		t.Errorf("instance after restart: present=%v run=%q, want run-b", ok, run)
	}
}
