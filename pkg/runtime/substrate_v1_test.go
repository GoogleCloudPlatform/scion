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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	ateapipb "github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// wireCall is one RPC the generated v1 stub sent: its full gRPC method and
// a copy of its request message.
type wireCall struct {
	method string
	req    proto.Message
}

// wireRecorder backs a real generated ateapipb.ControlClient with a unary
// interceptor that records every call (full method path and request) and
// answers from canned replies, without any network. It proves what the
// re-vendored stubs actually put on the wire.
type wireRecorder struct {
	mu      sync.Mutex
	calls   []wireCall
	replies map[string]func(req proto.Message) (proto.Message, error)
}

const controlService = "/substrate.ateapi.v1.Control/"

func newWireRecorder() *wireRecorder {
	running := func(req proto.Message) (proto.Message, error) {
		ref := req.(*ateapipb.GetActorRequest).GetActor()
		return &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName(), Uid: fakeActorUID},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
		}, nil
	}
	return &wireRecorder{replies: map[string]func(proto.Message) (proto.Message, error){
		"GetActor": running,
		"GetActorTemplate": func(proto.Message) (proto.Message, error) {
			return nil, status.Error(codes.NotFound, "no template")
		},
		"CreateActorTemplate": func(req proto.Message) (proto.Message, error) {
			tmpl := proto.Clone(req.(*ateapipb.CreateActorTemplateRequest).GetActorTemplate()).(*ateapipb.ActorTemplate)
			tmpl.Status = &ateapipb.ActorTemplateStatus{GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{
				GoldenTag: &ateapipb.ObjectRef{Atespace: "ate-golden", Name: "golden"},
			}}
			return tmpl, nil
		},
		"CreateActor": func(req proto.Message) (proto.Message, error) {
			a := proto.Clone(req.(*ateapipb.CreateActorRequest).GetActor()).(*ateapipb.Actor)
			a.Metadata.Uid = fakeActorUID
			return a, nil
		},
		"ResumeActor": func(proto.Message) (proto.Message, error) {
			return &ateapipb.ResumeActorResponse{
				Actor:   &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				Resumed: true,
			}, nil
		},
	}}
}

func (w *wireRecorder) client(t *testing.T) ateapipb.ControlClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///ateapi.invalid",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			w.mu.Lock()
			w.calls = append(w.calls, wireCall{method: method, req: proto.Clone(req.(proto.Message))})
			answer := w.replies[strings.TrimPrefix(method, controlService)]
			w.mu.Unlock()
			if answer == nil {
				return nil // empty reply
			}
			resp, err := answer(req.(proto.Message))
			if err != nil {
				return err
			}
			proto.Merge(reply.(proto.Message), resp)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ateapipb.NewControlClient(conn)
}

// last returns the request of the most recent call to rpc.
func (w *wireRecorder) last(t *testing.T, rpc string) proto.Message {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.calls) - 1; i >= 0; i-- {
		if w.calls[i].method == controlService+rpc {
			return w.calls[i].req
		}
	}
	t.Fatalf("no %s call recorded; calls: %v", rpc, w.calls)
	return nil
}

func newWireRuntime(t *testing.T, w *wireRecorder, sc config.V1SubstrateConfig) (*SubstrateRuntime, *fakeActorServer) {
	t.Helper()
	resetSubstrateAgentStateForTest(t)
	fa := newFakeActorServer(&callRecorder{})
	server := httptest.NewServer(fa.handler())
	t.Cleanup(server.Close)
	return NewSubstrateRuntimeForTest(w.client(t), substrate.NewRouterClient(server.URL), nil, sc), fa
}

// TestSubstrateV1Wire_CallSites drives every runtime call site through the
// real re-vendored stubs and pins the v1 wire: the service path
// /substrate.ateapi.v1.Control/<RPC>, and each request shape the v1.0.0
// API requires.
func TestSubstrateV1Wire_CallSites(t *testing.T) {
	w := newWireRecorder()
	rt, _ := newWireRuntime(t, w, config.V1SubstrateConfig{WorkerSelector: map[string]string{"pool": "gvisor"}})
	ctx := context.Background()

	cfg := testSubstrateRunConfig()
	cfg.Resources = &api.ResourceSpec{Limits: api.ResourceList{CPU: "1500m", Memory: "2Gi"}}
	id, err := rt.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	w.mu.Lock()
	for _, c := range w.calls {
		if c.method == controlService+"GetActor" {
			t.Errorf("Run() called GetActor despite ResumeActor already returning ACTOR_STATE_RUNNING")
		}
	}
	w.mu.Unlock()
	if _, err := rt.List(ctx, nil); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if _, _, err := rt.RecordlessActors(ctx, cfg.ProjectID); err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if err := rt.Delete(ctx, RunRef{ID: id}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	w.mu.Lock()
	for _, c := range w.calls {
		if !strings.HasPrefix(c.method, controlService) {
			t.Errorf("RPC %q is not on the v1 service path %s*", c.method, controlService)
		}
	}
	w.mu.Unlock()

	atespace, _, _ := splitSubstrateID(id)
	tests := []struct {
		rpc   string
		check func(t *testing.T, req proto.Message)
	}{
		{"CreateActorTemplate", func(t *testing.T, req proto.Message) {
			tmpl := req.(*ateapipb.CreateActorTemplateRequest).GetActorTemplate()
			lim := tmpl.GetResources().GetLimits()
			if lim.GetCpuMillicores() != 1500 || lim.GetMemoryBytes() != 2<<30 {
				t.Errorf("template resources.limits = %v, want cpu_millicores=1500 memory_bytes=2Gi", lim)
			}
			sc := tmpl.GetSnapshotConfig()
			if sc.GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY ||
				sc.GetGoldenSnapshotConfig().GetMode() != ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED {
				t.Errorf("template snapshot_config = %v, want MEMORY fidelity with golden snapshots enabled", sc)
			}
			if got := tmpl.GetWorkerSelector().GetMatchLabels()["pool"]; got != "gvisor" {
				t.Errorf("template worker_selector = %v, want the configured selector (the sandbox class comes from the WorkerPool)", tmpl.GetWorkerSelector())
			}
			probe := tmpl.GetContainers()[0].GetWakeupProbe()
			if probe.GetHttpGet().GetPath() != substrateHealthzPath || probe.GetHttpGet().GetPort() != 80 {
				t.Errorf("wakeup probe = %v, want GET %s :80", probe, substrateHealthzPath)
			}
		}},
		{"CreateActorEgressPolicy", func(t *testing.T, req proto.Message) {
			r := req.(*ateapipb.CreateActorEgressPolicyRequest)
			if r.GetEgressPolicy().GetMetadata().GetName() != "default" || len(r.GetEgressPolicy().GetRules()) == 0 {
				t.Errorf("egress policy = %v, want named default with v1 rules", r.GetEgressPolicy())
			}
			for _, rule := range r.GetEgressPolicy().GetRules() {
				if rule.GetHttp() == nil && rule.GetHttps() == nil && rule.GetTlsPassthrough() == nil {
					t.Errorf("rule %v sets no protocol", rule)
				}
			}
		}},
		{"ListActors", func(t *testing.T, req proto.Message) {
			r := req.(*ateapipb.ListActorsRequest)
			if r.GetAtespace() != atespace || r.GetOptions().GetPageSize() != 1000 {
				t.Errorf("last ListActors = %v, want atespace %s with ListOptions page_size 1000", r, atespace)
			}
		}},
		{"DeleteActorEgressPolicy", func(t *testing.T, req proto.Message) {
			if r := req.(*ateapipb.DeleteActorEgressPolicyRequest); r.GetName() != "default" {
				t.Errorf("DeleteActorEgressPolicy = %v, want name default", r)
			}
		}},
		{"DeleteActor", func(t *testing.T, req proto.Message) {
			if r := req.(*ateapipb.DeleteActorRequest); r.GetOptions().GetUid() != fakeActorUID {
				t.Errorf("DeleteActor = %v, want options.uid %s", r, fakeActorUID)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.rpc, func(t *testing.T) { tc.check(t, w.last(t, tc.rpc)) })
	}
}

func TestBuildActorTemplate_ResourceLimitValidation(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("a", 64)

	// Requests fall back when limits are unset.
	tmpl := mustBuildActorTemplate(t, "scion-x", "scion-req", image, config.V1SubstrateConfig{}, &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"},
	})
	lim := tmpl.GetResources().GetLimits()
	if lim.GetCpuMillicores() != 500 || lim.GetMemoryBytes() != 1<<30 {
		t.Errorf("limits from requests = %v, want 500m / 1Gi", lim)
	}

	// Nil resources use substrate defaults (2 CPU / 4Gi).
	tmpl = mustBuildActorTemplate(t, "scion-x", "scion-def", image, config.V1SubstrateConfig{}, nil)
	lim = tmpl.GetResources().GetLimits()
	if lim.GetCpuMillicores() != 2000 || lim.GetMemoryBytes() != 4<<30 {
		t.Errorf("default limits = %v, want 2000m / 4Gi", lim)
	}

	bad := []struct {
		name string
		spec *api.ResourceSpec
		want string
	}{
		{"invalid cpu", &api.ResourceSpec{Limits: api.ResourceList{CPU: "not-a-cpu", Memory: "1Gi"}}, "invalid CPU"},
		{"invalid memory", &api.ResourceSpec{Limits: api.ResourceList{CPU: "1", Memory: "bogus"}}, "invalid memory"},
		{"zero cpu", &api.ResourceSpec{Limits: api.ResourceList{CPU: "0", Memory: "1Gi"}}, "greater than zero"},
		{"negative memory", &api.ResourceSpec{Limits: api.ResourceList{CPU: "1", Memory: "-1Gi"}}, "greater than zero"},
		{"cpu over max", &api.ResourceSpec{Limits: api.ResourceList{CPU: "1000", Memory: "1Gi"}}, "999999 millicores"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildActorTemplate("scion-x", "scion-bad", image, config.V1SubstrateConfig{}, tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildActorTemplate(%s) = %v, want error containing %q", tc.name, err, tc.want)
			}
		})
	}
}
