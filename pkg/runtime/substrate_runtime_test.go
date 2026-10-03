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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	sciontoolsubstrate "github.com/GoogleCloudPlatform/scion/pkg/sciontool/substrate"
	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// -----------------------------------------------------------------------
// callRecorder: shared call-order tracker for both the fake ControlClient
// and the fake actor HTTP server.
// -----------------------------------------------------------------------

type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callRecorder) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
}

func (c *callRecorder) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// -----------------------------------------------------------------------
// fakeControlClient: interface-backed fake for ateapipb.ControlClient.
// -----------------------------------------------------------------------

const fakeActorUID = "actor-uid-1"

type fakeControlClient struct {
	rec *callRecorder

	createAtespace          func(*ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error)
	getActorTemplate        func(*ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error)
	createActorTemplate     func(*ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error)
	createActor             func(*ateapipb.CreateActorRequest) (*ateapipb.Actor, error)
	createActorEgressPolicy func(*ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error)
	deleteActorEgressPolicy func(*ateapipb.DeleteActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error)
	resumeActor             func(*ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error)
	getActor                func(*ateapipb.GetActorRequest) (*ateapipb.Actor, error)
	deleteActor             func(*ateapipb.DeleteActorRequest) (*ateapipb.Actor, error)
	listActors              func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error)

	// createdActors backs the DEFAULT createActor closure's AlreadyExists
	// behavior below (see newFakeControlClient) — keyed by "atespace/name",
	// guarded by mu. A test that overrides createActor entirely bypasses
	// this; it exists so a test that does NOT override createActor still
	// gets realistic "second call for the same name fails" behavior rather
	// than a silent overwrite, matching real ateapi.
	mu            sync.Mutex
	createdActors map[string]*ateapipb.Actor
}

// newFakeControlClient returns a fake wired for the Run happy path: a
// fresh template (GetActorTemplate NotFound, then CreateActorTemplate
// returns a ready one), CreateActor succeeds, the actor is immediately
// RUNNING with a worker assignment, and every other call succeeds.
// Individual tests override only the field(s) they care about.
func newFakeControlClient(rec *callRecorder) *fakeControlClient {
	f := &fakeControlClient{
		rec:           rec,
		createdActors: make(map[string]*ateapipb.Actor),
		createAtespace: func(*ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
			return &ateapipb.Atespace{}, nil
		},
		getActorTemplate: func(*ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
			return nil, status.Error(codes.NotFound, "not found")
		},
		createActorTemplate: func(req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
			tmpl := req.GetActorTemplate()
			tmpl.Status = &ateapipb.ActorTemplateStatus{
				GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{
					GoldenTag: &ateapipb.ObjectRef{Atespace: "ate-golden", Name: "golden"},
				},
			}
			return tmpl, nil
		},
		createActorEgressPolicy: func(*ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
			return &ateapipb.EgressPolicy{}, nil
		},
		deleteActorEgressPolicy: func(*ateapipb.DeleteActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
			return &ateapipb.EgressPolicy{}, nil
		},
		resumeActor: func(*ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
			return &ateapipb.ResumeActorResponse{}, nil
		},
		getActor: func(req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
			return &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{
					Atespace: req.GetActor().GetAtespace(),
					Name:     req.GetActor().GetName(),
					Uid:      fakeActorUID,
				},
				Status: &ateapipb.ActorStatus{
					State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
					WorkerAssignment: &ateapipb.WorkerAssignment{
						WorkerPod:       "worker-pod-1",
						WorkerNamespace: "ate-workers",
					},
				},
			}, nil
		},
		deleteActor: func(*ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
			return &ateapipb.Actor{}, nil
		},
		listActors: func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
			return &ateapipb.ListActorsResponse{}, nil
		},
	}
	f.createActor = func(req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
		key := req.GetActor().GetMetadata().GetAtespace() + "/" + req.GetActor().GetMetadata().GetName()
		f.mu.Lock()
		defer f.mu.Unlock()
		// A real ateapi CreateActor refuses a second call for the same
		// atespace/name with AlreadyExists, rather than overwriting —
		// SubstrateRuntime.Run's restart path depends on exactly that
		// error to detect "this actor already exists" (see
		// substrate_runtime.go's codes.AlreadyExists handling). Tracking
		// state here, rather than always succeeding, means a test that
		// does not override createActor still exercises that behavior
		// realistically on a second Run for the same atespace/name.
		if _, exists := f.createdActors[key]; exists {
			return nil, status.Error(codes.AlreadyExists, "actor already exists")
		}
		actor := &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{
				Atespace: req.GetActor().GetMetadata().GetAtespace(),
				Name:     req.GetActor().GetMetadata().GetName(),
				Uid:      fakeActorUID,
			},
		}
		f.createdActors[key] = actor
		return actor, nil
	}
	return f
}

func (f *fakeControlClient) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.rec.record("GetActor")
	return f.getActor(in)
}
func (f *fakeControlClient) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.rec.record("CreateActor")
	return f.createActor(in)
}
func (f *fakeControlClient) UpdateActor(ctx context.Context, in *ateapipb.UpdateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.rec.record("UpdateActor")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	f.rec.record("SuspendActor")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) PauseActor(ctx context.Context, in *ateapipb.PauseActorRequest, opts ...grpc.CallOption) (*ateapipb.PauseActorResponse, error) {
	f.rec.record("PauseActor")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	f.rec.record("ResumeActor")
	return f.resumeActor(in)
}
func (f *fakeControlClient) RevertActor(ctx context.Context, in *ateapipb.RevertActorRequest, opts ...grpc.CallOption) (*ateapipb.RevertActorResponse, error) {
	f.rec.record("RevertActor")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.rec.record("DeleteActor")
	return f.deleteActor(in)
}
func (f *fakeControlClient) GetActorEgressPolicy(ctx context.Context, in *ateapipb.GetActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.rec.record("GetActorEgressPolicy")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.rec.record("CreateActorEgressPolicy")
	return f.createActorEgressPolicy(in)
}
func (f *fakeControlClient) UpdateActorEgressPolicy(ctx context.Context, in *ateapipb.UpdateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.rec.record("UpdateActorEgressPolicy")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteActorEgressPolicy(ctx context.Context, in *ateapipb.DeleteActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.rec.record("DeleteActorEgressPolicy")
	return f.deleteActorEgressPolicy(in)
}
func (f *fakeControlClient) MintActorJWT(ctx context.Context, in *ateapipb.MintActorJWTRequest, opts ...grpc.CallOption) (*ateapipb.MintActorJWTResponse, error) {
	f.rec.record("MintActorJWT")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) MintActorCertificate(ctx context.Context, in *ateapipb.MintActorCertificateRequest, opts ...grpc.CallOption) (*ateapipb.MintActorCertificateResponse, error) {
	f.rec.record("MintActorCertificate")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) CreateTag(ctx context.Context, in *ateapipb.CreateTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	f.rec.record("CreateTag")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) GetTag(ctx context.Context, in *ateapipb.GetTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	f.rec.record("GetTag")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ListTags(ctx context.Context, in *ateapipb.ListTagsRequest, opts ...grpc.CallOption) (*ateapipb.ListTagsResponse, error) {
	f.rec.record("ListTags")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) UpdateTag(ctx context.Context, in *ateapipb.UpdateTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	f.rec.record("UpdateTag")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteTag(ctx context.Context, in *ateapipb.DeleteTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	f.rec.record("DeleteTag")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	f.rec.record("ListWorkers")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) GetWorker(ctx context.Context, in *ateapipb.GetWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.rec.record("GetWorker")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) CreateWorker(ctx context.Context, in *ateapipb.CreateWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.rec.record("CreateWorker")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) UpdateWorker(ctx context.Context, in *ateapipb.UpdateWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.rec.record("UpdateWorker")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteWorker(ctx context.Context, in *ateapipb.DeleteWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.rec.record("DeleteWorker")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DrainWorker(ctx context.Context, in *ateapipb.DrainWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.rec.record("DrainWorker")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ListWorkerActorAssignments(ctx context.Context, in *ateapipb.ListWorkerActorAssignmentsRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkerActorAssignmentsResponse, error) {
	f.rec.record("ListWorkerActorAssignments")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	f.rec.record("ListActors")
	return f.listActors(in)
}
func (f *fakeControlClient) CreateAtespace(ctx context.Context, in *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.rec.record("CreateAtespace")
	return f.createAtespace(in)
}
func (f *fakeControlClient) GetAtespace(ctx context.Context, in *ateapipb.GetAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.rec.record("GetAtespace")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) ListAtespaces(ctx context.Context, in *ateapipb.ListAtespacesRequest, opts ...grpc.CallOption) (*ateapipb.ListAtespacesResponse, error) {
	f.rec.record("ListAtespaces")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteAtespace(ctx context.Context, in *ateapipb.DeleteAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.rec.record("DeleteAtespace")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) CreateActorTemplate(ctx context.Context, in *ateapipb.CreateActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	f.rec.record("CreateActorTemplate")
	return f.createActorTemplate(in)
}
func (f *fakeControlClient) GetActorTemplate(ctx context.Context, in *ateapipb.GetActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	f.rec.record("GetActorTemplate")
	return f.getActorTemplate(in)
}
func (f *fakeControlClient) ListActorTemplates(ctx context.Context, in *ateapipb.ListActorTemplatesRequest, opts ...grpc.CallOption) (*ateapipb.ListActorTemplatesResponse, error) {
	f.rec.record("ListActorTemplates")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}
func (f *fakeControlClient) DeleteActorTemplate(ctx context.Context, in *ateapipb.DeleteActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	f.rec.record("DeleteActorTemplate")
	return nil, status.Error(codes.Unimplemented, "not used in this fake")
}

// -----------------------------------------------------------------------
// fakeActorServer: httptest-backed stand-in for sciontool substrate-serve,
// reached through a substrate.RouterClient exactly like the real router.
// -----------------------------------------------------------------------

type fakeActorServer struct {
	rec *callRecorder

	mu              sync.Mutex
	healthzState    string
	bootstrapStatus int
	bootstrapBody   string // written verbatim after the status line, if non-empty
	lastBootstrap   *bootstrapRequest
	execStatus      int
	execResp        execResponse
	lastExec        *execRequest
}

func newFakeActorServer(rec *callRecorder) *fakeActorServer {
	return &fakeActorServer{rec: rec, healthzState: healthzAwaitingBootstrap}
}

func (s *fakeActorServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(substrateHealthzPath, func(w http.ResponseWriter, r *http.Request) {
		s.rec.record("healthz")
		s.mu.Lock()
		state := s.healthzState
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(healthzResponse{State: state})
	})
	mux.HandleFunc(substrateBootstrapPath, func(w http.ResponseWriter, r *http.Request) {
		s.rec.record("bootstrap")
		var req bootstrapRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.lastBootstrap = &req
		code := s.bootstrapStatus
		respBody := s.bootstrapBody
		s.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		if respBody != "" {
			_, _ = w.Write([]byte(respBody))
		}
	})
	mux.HandleFunc(substrateExecPath, func(w http.ResponseWriter, r *http.Request) {
		s.rec.record("exec")
		var req execRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.lastExec = &req
		code := s.execStatus
		resp := s.execResp
		s.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

// -----------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------

func testSubstrateRunConfig() RunConfig {
	return RunConfig{
		Name:         "test-agent",
		ProjectID:    "550e8400-e29b-41d4-a716-446655440000",
		Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
		UnixUsername: "scion",
		Harness:      &mockHarness{command: []string{"claude", "--dangerously-skip-permissions"}},
		Env:          []string{"SCION_AGENT_ID=agent-1", "SCION_HUB_ENDPOINT=https://hub.example.com"},
		// Matches the SCION_HUB_ENDPOINT entry in Env above: this fixture
		// has no agent/template override in play, so the trusted and final
		// hub hosts agree, and substrateEgressHostnames's mismatch Warn
		// (see its own doc comment) stays silent for every test that uses
		// this shared config instead of exercising the hub-endpoint-
		// mismatch path incidentally.
		TrustedHubEndpoint: "https://hub.example.com",
		Labels:             map[string]string{"scion.agent_id": "agent-1"},
	}
}

// newTestSubstrateHarness resets the process-wide agent-state registry
// (substrateControlTokens/substrateAgentRecords — process-wide package vars,
// see their doc comment) so each test starts clean and can't leak state
// into, or pick up state left by, any other test that also uses this
// helper.
func newTestSubstrateHarness(t *testing.T, rec *callRecorder) (*SubstrateRuntime, *fakeControlClient, *fakeActorServer, func()) {
	t.Helper()
	resetSubstrateAgentStateForTest(t)

	fc := newFakeControlClient(rec)
	fa := newFakeActorServer(rec)
	server := httptest.NewServer(fa.handler())
	rt := NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(server.URL), nil, config.V1SubstrateConfig{
		SnapshotStorage:   "gs://bucket/prefix/",
		SandboxConfigName: "gvisor-default",
	})
	return rt, fc, fa, server.Close
}

// resetSubstrateAgentStateForTest clears the process-wide
// substrateControlTokens/substrateAgentRecords maps for the duration of a
// test, restoring their previous contents afterward — the same save/restore
// pattern as resetSubstrateRuntimeRegistryForTest, for the same reason
// (these are now shared by every SubstrateRuntime instance, so tests must
// not leak state through them).
func resetSubstrateAgentStateForTest(t *testing.T) {
	t.Helper()
	substrateAgentStateMu.Lock()
	oldTokens := substrateControlTokens
	oldRecords := substrateAgentRecords
	substrateControlTokens = make(map[string]string)
	substrateAgentRecords = make(map[string]*substrateAgentRecord)
	substrateAgentStateMu.Unlock()
	t.Cleanup(func() {
		substrateAgentStateMu.Lock()
		substrateControlTokens = oldTokens
		substrateAgentRecords = oldRecords
		substrateAgentStateMu.Unlock()
	})
}

// TestEnsureActorTemplate_FailedGoldenSnapshotIsCleanedUp is the regression
// test for a failed golden-snapshot build leaving a sticky, broken
// template behind: templateName is content-addressed (substrateTemplateName
// hashes the image/config/resources shape), so without cleanup, every
// future call for the same shape would find the SAME broken template via
// GetActorTemplate and never attempt CreateActorTemplate again.
// ensureActorTemplate must call DeleteActorTemplate (best-effort) before
// returning the golden-snapshot-failed error.
func TestEnsureActorTemplate_FailedGoldenSnapshotIsCleanedUp(t *testing.T) {
	rec := &callRecorder{}
	fc := newFakeControlClient(rec)

	const atespace, templateName = "scion-proj", "tmpl-abc123"
	var getCalls int
	var getMu sync.Mutex
	fc.getActorTemplate = func(*ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
		// Not ready yet (no golden tag), and no error message either, on
		// the FIRST call only — this is the initial GetActorTemplate
		// ensureActorTemplate makes before entering its poll loop. The
		// poll loop's own GetActorTemplate call is distinguished by the
		// call count below.
		getMu.Lock()
		defer getMu.Unlock()
		getCalls++
		if getCalls == 1 {
			return &ateapipb.ActorTemplate{Status: &ateapipb.ActorTemplateStatus{}}, nil
		}
		// The poll: the golden snapshot build failed.
		return &ateapipb.ActorTemplate{
			Status: &ateapipb.ActorTemplateStatus{
				GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{
					ErrorMessage: "simulated golden snapshot build failure",
				},
			},
		}, nil
	}

	err := ensureActorTemplate(context.Background(), fc, atespace, templateName,
		&ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: templateName}},
		time.Second, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "golden snapshot failed") {
		t.Fatalf("ensureActorTemplate() error = %v, want a golden-snapshot-failed error", err)
	}

	calls := rec.list()
	found := false
	for _, c := range calls {
		if c == "DeleteActorTemplate" {
			found = true
		}
	}
	if !found {
		t.Errorf("call trace = %v, want it to include a DeleteActorTemplate cleanup call", calls)
	}
}

// -----------------------------------------------------------------------
// Run: happy path
// -----------------------------------------------------------------------

func TestSubstrateRun_HappyPath(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantID := substrateAtespaceName(testSubstrateRunConfig().ProjectID) + "/test-agent"
	if id != wantID {
		t.Errorf("Run() id = %q, want %q", id, wantID)
	}

	gotCalls := rec.list()
	wantOrder := []string{
		"CreateAtespace",
		"GetActorTemplate",
		"CreateActorTemplate",
		"CreateActor",
		"CreateActorEgressPolicy",
		"ResumeActor",
		"GetActor",
		"healthz",
		"bootstrap",
	}
	if strings.Join(gotCalls, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("call order = %v, want %v", gotCalls, wantOrder)
	}

	if fa.lastBootstrap == nil {
		t.Fatal("bootstrap was not called")
	}
	if fa.lastBootstrap.StartCmd == "" {
		t.Error("bootstrap start_cmd is empty")
	}
	if !strings.Contains(fa.lastBootstrap.StartCmd, "tmux new-session") {
		t.Errorf("bootstrap start_cmd = %q, want a tmux new-session invocation", fa.lastBootstrap.StartCmd)
	}
	if fa.lastBootstrap.ControlToken == "" {
		t.Error("bootstrap control_token is empty")
	}
	if fa.lastBootstrap.Env["SCION_RUNTIME"] != "substrate" {
		t.Errorf("bootstrap env SCION_RUNTIME = %q, want substrate", fa.lastBootstrap.Env["SCION_RUNTIME"])
	}
	if fa.lastBootstrap.Env["SCION_AGENT_ID"] != "agent-1" {
		t.Errorf("bootstrap env did not carry cfg.Env through: %v", fa.lastBootstrap.Env)
	}

	substrateAgentStateMu.Lock()
	_, hasToken := substrateControlTokens[id]
	substrateAgentStateMu.Unlock()
	if !hasToken {
		t.Error("control token was not cached for the returned id")
	}
}

// TestSubstrateRun_AlreadyExistsMapsToContainerNameInUse is the regression
// test for Run's codes.AlreadyExists handling (substrate_runtime.go, the
// CreateActor error branch): a second Run for the same atespace/actor name
// — the shape a restart against a pre-existing, record-less actor takes,
// since the broker skips its own pre-create existence check in that case
// (restartAgent, pkg/runtimebroker/handlers.go) and relies entirely on this
// mapping — must surface an error agent.isContainerNameInUseError's
// substring match recognizes ("container name" and "already in use"), not
// a generic wrapped gRPC error, and must never create a second egress
// policy for an actor that was never actually created.
func TestSubstrateRun_AlreadyExistsMapsToContainerNameInUse(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	cfg := testSubstrateRunConfig()
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	callsAfterFirstRun := len(rec.list())

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("second Run() for the same atespace/actor name: expected an error, got nil")
	}
	lowerMsg := strings.ToLower(err.Error())
	if !strings.Contains(lowerMsg, "container name") || !strings.Contains(lowerMsg, "already in use") {
		t.Errorf("second Run() error = %q, want it to match agent.isContainerNameInUseError's pattern (\"container name\" and \"already in use\")", err.Error())
	}

	// The second Run's own contribution to the shared call trace (the fake
	// ateapi client and bootstrap recorder are reused across both calls)
	// must stop at CreateActor and go no further — no
	// CreateActorEgressPolicy, ResumeActor, GetActor, healthz, or bootstrap
	// for an actor that was never actually (re)created. The atespace and
	// template steps ahead of CreateActor still run (ensureAtespace treats
	// AlreadyExists as success; the fake's GetActorTemplate always reports
	// NotFound, so CreateActorTemplate runs again too) — this asserts only
	// that nothing AFTER CreateActor ran.
	secondRunCalls := rec.list()[callsAfterFirstRun:]
	if len(secondRunCalls) == 0 || secondRunCalls[len(secondRunCalls)-1] != "CreateActor" {
		t.Errorf("second Run() call trace = %v, want it to end at CreateActor (no egress policy or bootstrap for an actor that was never created)", secondRunCalls)
	}
}

// -----------------------------------------------------------------------
// substrateAtespaceName: collision resistance
// -----------------------------------------------------------------------

// TestSubstrateAtespaceName_SharedPrefixDoesNotCollide proves two project
// IDs that share the first 12 characters — the exact shape the prior
// "scion-"+projectID[:12] scheme collided on — produce different
// atespaces now that the full ID is hashed. A shared atespace would let
// two unrelated projects' record-less-actor counts, and whatever else the
// atespace boundary is meant to isolate, leak into each other.
func TestSubstrateAtespaceName_SharedPrefixDoesNotCollide(t *testing.T) {
	const shared12 = "550e8400-e29"
	idA := shared12 + "b-41d4-a716-446655440000"
	idB := shared12 + "b-99999-ffffffffffff-zzzz"

	if idA[:12] != idB[:12] {
		t.Fatalf("test bug: idA and idB do not actually share a 12-char prefix (%q vs %q)", idA[:12], idB[:12])
	}

	gotA := substrateAtespaceName(idA)
	gotB := substrateAtespaceName(idB)
	if gotA == gotB {
		t.Fatalf("substrateAtespaceName(%q) == substrateAtespaceName(%q) == %q, want distinct atespaces for distinct project IDs sharing a 12-char prefix", idA, idB, gotA)
	}
}

// TestSubstrateAtespaceName_Deterministic proves the same project ID always
// hashes to the same atespace (Run/RecordlessActors/List must all agree on
// one project's atespace across calls and restarts) and that the result is
// a valid k8s-short-name: lowercase hex plus the fixed "scion-" prefix,
// starting and ending with an alphanumeric character, comfortably under
// the 63-character limit.
func TestSubstrateAtespaceName_Deterministic(t *testing.T) {
	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	got1 := substrateAtespaceName(projectID)
	got2 := substrateAtespaceName(projectID)
	if got1 != got2 {
		t.Fatalf("substrateAtespaceName(%q) is not deterministic: %q vs %q", projectID, got1, got2)
	}
	if !strings.HasPrefix(got1, "scion-") {
		t.Errorf("substrateAtespaceName(%q) = %q, want the \"scion-\" prefix", projectID, got1)
	}
	if len(got1) > 63 {
		t.Errorf("substrateAtespaceName(%q) = %q, length %d exceeds the 63-char k8s-short-name limit", projectID, got1, len(got1))
	}
	for _, r := range got1 {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			t.Errorf("substrateAtespaceName(%q) = %q contains invalid k8s-short-name character %q", projectID, got1, r)
		}
	}
}

// -----------------------------------------------------------------------
// Run: non-digest image error
// -----------------------------------------------------------------------

func TestSubstrateRun_NonDigestImageError(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	cfg := testSubstrateRunConfig()
	cfg.Image = "us-docker.pkg.dev/proj/repo/scion-agent:latest"

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() expected an error for a non-digest-pinned image, got nil")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error = %v, want it to mention the digest requirement", err)
	}

	// Must fail before touching the control plane at all (step 2 precedes
	// CreateAtespace's caller-visible effect... actually atespace is step 1;
	// the image check is step 2, so CreateAtespace WILL have been called).
	// What must NOT happen is anything past the image check.
	for _, c := range rec.list() {
		if c == "CreateActor" || c == "CreateActorTemplate" {
			t.Errorf("Run() called %s before failing the digest check", c)
		}
	}
}

// TestSubstrateRun_EmptyProjectIDRejected proves Run refuses an empty
// ProjectID outright rather than silently hashing it into one fixed
// atespace shared by every such caller (substrateAtespaceName has no other
// input to distinguish them). Must fail before touching the control plane
// at all — no CreateAtespace call.
func TestSubstrateRun_EmptyProjectIDRejected(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	cfg := testSubstrateRunConfig()
	cfg.ProjectID = ""

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() expected an error for an empty ProjectID, got nil")
	}
	if !strings.Contains(err.Error(), "ProjectID") {
		t.Errorf("error = %v, want it to mention ProjectID", err)
	}
	if len(rec.list()) != 0 {
		t.Errorf("Run() made control-plane calls %v before rejecting the empty ProjectID, want none", rec.list())
	}
}

// -----------------------------------------------------------------------
// Run: cleanup on failure at each step after CreateActor
// -----------------------------------------------------------------------

func TestSubstrateRun_CleanupOnFailure(t *testing.T) {
	// bootstrapHijackSentinel proves the 409 path's error is genuinely
	// secret-free, not just free of the specific strings the other
	// assertions happen to check: the 409 case below puts this in cfg.Env
	// and asserts it is ABSENT from the error, guarding against a future
	// change that wraps errBootstrapHijacked with cfg-derived context and
	// forgets to route it through r.redact.
	const bootstrapHijackSentinel = "FAKE-KEY-SENTINEL-bootstrap-hijack-not-a-real-credential"

	cases := []struct {
		name       string
		inject     func(fc *fakeControlClient, fa *fakeActorServer)
		setup      func(rt *SubstrateRuntime) // optional; runs after newTestSubstrateHarness, before Run
		cfg        func(cfg RunConfig) RunConfig
		wantErr    string
		wantAbsent string // optional; asserts this string does NOT appear in the error
	}{
		{
			name: "CreateActorEgressPolicy fails",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fc.createActorEgressPolicy = func(*ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
					return nil, status.Error(codes.Internal, "egress boom")
				}
			},
			wantErr: "egress boom",
		},
		{
			name: "ResumeActor fails",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fc.resumeActor = func(*ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
					return nil, status.Error(codes.Internal, "resume boom")
				}
			},
			wantErr: "resume boom",
		},
		{
			name: "actor crashes while starting",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fc.getActor = func(req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
					return &ateapipb.Actor{
						Metadata: &ateapipb.ResourceMetadata{Atespace: req.GetActor().GetAtespace(), Name: req.GetActor().GetName(), Uid: fakeActorUID},
						Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
					}, nil
				}
			},
			wantErr: "crashed",
		},
		{
			name: "healthz never reaches awaiting-bootstrap (timeout)",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fa.healthzState = healthzRunning
			},
			setup: func(rt *SubstrateRuntime) {
				// Real time.Now()-based deadline (waitForHealthz doesn't use
				// r.now), so keep this short enough to actually finish.
				rt.healthzTimeout = 20 * time.Millisecond
			},
			wantErr: "did not reach healthz state",
		},
		{
			name: "buildBootstrapFiles read error",
			cfg: func(cfg RunConfig) RunConfig {
				cfg.ResolvedAuth = &api.ResolvedAuth{
					Files: []api.FileMapping{
						{SourcePath: "/nonexistent/does-not-exist", ContainerPath: "~/.creds/token"},
					},
				}
				return cfg
			},
			wantErr: "read auth file",
		},
		{
			// This is also the broker-side proof that the privilege drop
			// failing closed actually fails the agent: substrate-serve's
			// synchronous privilege-drop precondition (pkg/sciontool/
			// substrate.PrivilegeDropChecker) fails a bootstrap by returning exactly
			// this shape of response — a non-2xx from POST /bootstrap — and
			// the broker cannot tell that failure apart from any other
			// bootstrap failure. postBootstrap already treats every non-2xx
			// as an error (substrate_bootstrap.go), so this case (and the
			// cleanup/DeleteActor assertion below, common to every case in
			// this table) is what "Run() returns an error and the actor is
			// deleted" actually reduces to on the broker side.
			name: "bootstrap fails",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fa.bootstrapStatus = http.StatusInternalServerError
			},
			wantErr: "bootstrap",
		},
		{
			name: "bootstrap hijacked (409)",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fa.bootstrapStatus = http.StatusConflict
			},
			cfg: func(cfg RunConfig) RunConfig {
				cfg.Env = append(cfg.Env, "BOOTSTRAP_HIJACK_SENTINEL_KEY="+bootstrapHijackSentinel)
				return cfg
			},
			wantErr:    "bootstrapped by another caller",
			wantAbsent: bootstrapHijackSentinel,
		},
		{
			// A 422 whose body doesn't match the wire contract (no stable
			// code, unparseable by parseBootstrapPathError) must still fail
			// with a sane, bounded, content-free error — never silently
			// succeed or panic — via bootstrapPathRejectedError's fallback
			// branch (code == "" && path == "").
			name: "bootstrap 422 with unparseable body falls back to the raw body",
			inject: func(fc *fakeControlClient, fa *fakeActorServer) {
				fa.bootstrapStatus = http.StatusUnprocessableEntity
				fa.bootstrapBody = "garbage"
			},
			wantErr: "(422): garbage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &callRecorder{}
			rt, fc, fa, closeServer := newTestSubstrateHarness(t, rec)
			defer closeServer()
			if tc.inject != nil {
				tc.inject(fc, fa)
			}
			if tc.setup != nil {
				tc.setup(rt)
			}

			runCfg := testSubstrateRunConfig()
			if tc.cfg != nil {
				runCfg = tc.cfg(runCfg)
			}

			_, err := rt.Run(context.Background(), runCfg)
			if err == nil {
				t.Fatal("Run() expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantAbsent != "" && strings.Contains(err.Error(), tc.wantAbsent) {
				t.Errorf("CREDENTIAL LEAK: error = %v, want it to NOT contain %q", err, tc.wantAbsent)
			}

			calls := rec.list()
			if !containsCall(calls, "DeleteActor") {
				t.Errorf("Run() failure did not clean up with DeleteActor; calls = %v", calls)
			}
			if !containsCall(calls, "DeleteActorEgressPolicy") {
				t.Errorf("Run() failure did not clean up with DeleteActorEgressPolicy; calls = %v", calls)
			}
		})
	}
}

// TestSubstrateRun_BootstrapPathRejectedSurfacesCodeAndPathNoContent proves
// the broker-side contract for a rejected bootstrap path: when substrate-serve answers 422 for a
// bootstrap file whose Path failed validation, Run's returned error names
// both the stable code and the offending path (parsed by
// parseBootstrapPathError out of the 422 response body), and never leaks
// file content — even though this run's own ResolvedSecret carries a
// sentinel, proving the client-side error-construction path itself
// introduces no leak of its own.
func TestSubstrateRun_BootstrapPathRejectedSurfacesCodeAndPathNoContent(t *testing.T) {
	const contentSentinel = "FAKE-SENTINEL-secret-content-not-a-real-credential"
	const rejectedPath = "/home/scion/.config/nested/secret.json"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	fa.bootstrapStatus = http.StatusUnprocessableEntity
	// Golden, byte-exact fixture: mirrors pkg/sciontool/substrate's
	// TestBootstrap_SymlinkedFileRejectionSurfacesAs422WithStableCode body
	// exactly, including strconv.Quote on the path and the trailing "\n"
	// http.Error's Fprintln appends — this is the wire contract
	// parseBootstrapPathError depends on. A format change on either side
	// must update both tests.
	fa.bootstrapBody = "bootstrap_path_symlink: bootstrap file " + strconv.Quote(rejectedPath) + " rejected: path traverses a symlink\n"

	cfg := testSubstrateRunConfig()
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "S", Type: "file", Target: "~/.config/nested/secret.json", Value: contentSentinel},
	}

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap_path_symlink") {
		t.Errorf("error = %v, want it to contain the stable code %q", err, "bootstrap_path_symlink")
	}
	if !strings.Contains(err.Error(), rejectedPath) {
		t.Errorf("error = %v, want it to contain the rejected path %q", err, rejectedPath)
	}
	if strings.Contains(err.Error(), contentSentinel) {
		t.Errorf("CREDENTIAL LEAK: error = %v, want it to NOT contain the file content", err)
	}

	// Contains alone doesn't prove the body actually parsed: the fallback
	// branch in postBootstrap (parseBootstrapPathError's ok == false) stuffs
	// the raw body into detail, and the raw body also contains the code and
	// path substrings too — so Contains(err, code) and Contains(err, path)
	// hold either way. errors.As into the concrete type and checking its
	// code/path fields is what actually pins the parse. That has to be
	// checked against postBootstrap's own return value, not Run()'s: Run's
	// redact (substrate_runtime.go's r.redact) deliberately flattens every
	// returned error to a plain string (so it can strip a secret value out
	// of the text), which erases bootstrapPathRejectedError's concrete type
	// along the way. postBootstrap is what actually calls
	// parseBootstrapPathError on this same fixture body, so exercising it
	// directly against the harness's own router/fake server still proves
	// the wire body parses, and fails (rather than stays green) if the
	// parser regresses — see TestParseBootstrapPathError for the parser's
	// own dedicated string-level coverage.
	postErr := postBootstrap(context.Background(), rt.router, "scion-atespace", "test-agent", "nonce", bootstrapRequest{
		Files:        []bootstrapFile{{Path: rejectedPath, ContentB64: "eA=="}},
		StartCmd:     "true",
		ControlToken: "tok",
	}, nil)
	var pathErr *bootstrapPathRejectedError
	if !errors.As(postErr, &pathErr) {
		t.Fatalf("errors.As(postBootstrap's err, *bootstrapPathRejectedError) = false, want true; err = %v", postErr)
	}
	if pathErr.code != "bootstrap_path_symlink" {
		t.Errorf("pathErr.code = %q, want %q", pathErr.code, "bootstrap_path_symlink")
	}
	if pathErr.path != rejectedPath {
		t.Errorf("pathErr.path = %q, want %q", pathErr.path, rejectedPath)
	}

	calls := rec.list()
	if !containsCall(calls, "DeleteActor") {
		t.Errorf("Run() failure did not clean up with DeleteActor; calls = %v", calls)
	}
}

// TestSubstrateRun_BootstrapPathRejectedLogsCodeAndPath proves the other half
// of the fallback-and-logging contract: when the 422 body DOES parse, Run's
// explicit runtimeLog.Error call (substrate_runtime.go, right before
// cleanup()) carries the parsed code and path as attributes — elsewhere
// this is exercised only indirectly (via the returned error); here it is
// asserted against the log line itself.
func TestSubstrateRun_BootstrapPathRejectedLogsCodeAndPath(t *testing.T) {
	const rejectedPath = "/home/scion/.config/nested/secret.json"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	fa.bootstrapStatus = http.StatusUnprocessableEntity
	fa.bootstrapBody = "bootstrap_path_symlink: bootstrap file " + strconv.Quote(rejectedPath) + " rejected: path traverses a symlink\n"

	// runtimeLog is slog.Default() with a "subsystem" attr (see common.go),
	// which by default bridges to the stdlib "log" package — the same
	// capture pattern common_test.go's TestRunSimpleCommand_NoSecretsInDebugLog
	// uses.
	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})

	_, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err == nil {
		t.Fatal("Run() expected an error, got nil")
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "bootstrap_path_symlink") {
		t.Errorf("log output missing the code %q; log:\n%s", "bootstrap_path_symlink", logOutput)
	}
	if !strings.Contains(logOutput, rejectedPath) {
		t.Errorf("log output missing the path %q; log:\n%s", rejectedPath, logOutput)
	}
}

// TestSubstrateRun_BootstrapPathRejectedLogsQuotePathWithNewline proves the
// claim in substrate_runtime.go's comment above the runtimeLog.Error call: a
// newline (or other control byte) embedded in pathErr.path is quoted by
// slog's own attribute encoding, so the structured log call needs no
// separate %q the way bootstrapPathRejectedError.Error()'s plain
// fmt.Sprintf does. Without that quoting, a forged newline in the path could
// split this log line into two or forge an extra one, the same class of bug
// server_test.go's TestBootstrap_RejectedPathLogLineIsSingleLineEvenWithEmbeddedNewline
// covers on the serve side.
func TestSubstrateRun_BootstrapPathRejectedLogsQuotePathWithNewline(t *testing.T) {
	const rejectedPath = "/home/scion/.config/nested\nFAKE LOG LINE INJECTED\nsecret.json"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	fa.bootstrapStatus = http.StatusUnprocessableEntity
	fa.bootstrapBody = "bootstrap_path_symlink: bootstrap file " + strconv.Quote(rejectedPath) + " rejected: path traverses a symlink\n"

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})

	_, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err == nil {
		t.Fatal("Run() expected an error, got nil")
	}

	logOutput := buf.String()
	lines := strings.Split(strings.TrimRight(logOutput, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("log output had %d lines, want exactly 1 (the embedded newline split it):\n%s", len(lines), logOutput)
	}
	if strings.Contains(logOutput, "FAKE LOG LINE INJECTED\n") {
		t.Errorf("log output contains a forged line from the embedded newline: %q", logOutput)
	}
	if !strings.Contains(logOutput, "bootstrap_path_symlink") {
		t.Errorf("log output missing the code %q; log:\n%s", "bootstrap_path_symlink", logOutput)
	}
	if !strings.Contains(lines[0], `path="/home/scion/.config/nested\nFAKE LOG LINE INJECTED\nsecret.json"`) {
		t.Errorf("log line = %q, want the path attribute quoted with the embedded newline escaped", lines[0])
	}
}

// TestParseBootstrapPathError proves the client-side parse of
// pkg/sciontool/substrate's bootstrapPathError wire text round-trips a path
// containing characters strconv.Quote must escape (a space and a literal
// quote), and that a body which doesn't match the expected shape reports
// ok=false rather than a wrong split.
func TestParseBootstrapPathError(t *testing.T) {
	tricky := `/home/scion/weird "quoted" path/file`
	body := `bootstrap_path_invalid: bootstrap file ` + strconv.Quote(tricky) + ` rejected: a path component exists and is not a directory`

	code, path, detail, ok := parseBootstrapPathError(body)
	if !ok {
		t.Fatalf("parseBootstrapPathError(%q): ok = false, want true", body)
	}
	if code != "bootstrap_path_invalid" {
		t.Errorf("code = %q, want %q", code, "bootstrap_path_invalid")
	}
	if path != tricky {
		t.Errorf("path = %q, want %q", path, tricky)
	}
	if detail != "a path component exists and is not a directory" {
		t.Errorf("detail = %q, want %q", detail, "a path component exists and is not a directory")
	}

	if _, _, _, ok := parseBootstrapPathError("failed to write bootstrap files"); ok {
		t.Error("parseBootstrapPathError on a generic 500 body: ok = true, want false")
	}

	// A path that itself contains the literal tail delimiter
	// (" rejected: ") must not make the parser split inside the quoted path.
	// A naive strings.Index search would find this occurrence first, hand
	// strconv.Unquote a truncated fragment, and return ok = false.
	tailInPath := `/home/scion/not rejected: yet/file`
	bodyWithTailInPath := `bootstrap_path_invalid: bootstrap file ` + strconv.Quote(tailInPath) + ` rejected: a path component exists and is not a directory`
	code2, path2, detail2, ok2 := parseBootstrapPathError(bodyWithTailInPath)
	if !ok2 {
		t.Fatalf("parseBootstrapPathError(%q): ok = false, want true (path contains %q)", bodyWithTailInPath, bootstrapPathErrorTail)
	}
	if path2 != tailInPath {
		t.Errorf("path = %q, want %q", path2, tailInPath)
	}
	if code2 != "bootstrap_path_invalid" {
		t.Errorf("code = %q, want %q", code2, "bootstrap_path_invalid")
	}
	if detail2 != "a path component exists and is not a directory" {
		t.Errorf("detail = %q, want %q", detail2, "a path component exists and is not a directory")
	}
}

func containsCall(calls []string, name string) bool {
	for _, c := range calls {
		if c == name {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------
// Template name stability
// -----------------------------------------------------------------------

func TestSubstrateTemplateName_Stable(t *testing.T) {
	resources := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}}
	image := "repo/image@sha256:" + strings.Repeat("b", 64)
	baseCfg := config.V1SubstrateConfig{
		SandboxClass:      "gvisor",
		SandboxConfigName: "gvisor-default",
		SnapshotStorage:   "gs://bucket/prefix/",
	}

	n1 := substrateTemplateName(image, baseCfg, resources)
	n2 := substrateTemplateName(image, baseCfg, resources)
	if n1 != n2 {
		t.Errorf("substrateTemplateName() not stable: %q != %q", n1, n2)
	}
	if !strings.HasPrefix(n1, "scion-") {
		t.Errorf("substrateTemplateName() = %q, want scion- prefix", n1)
	}

	microVMCfg := baseCfg
	microVMCfg.SandboxClass = "microvm"
	n3 := substrateTemplateName(image, microVMCfg, resources)
	if n1 == n3 {
		t.Error("substrateTemplateName() did not change with sandbox class")
	}

	n4 := substrateTemplateName(image, baseCfg, &api.ResourceSpec{Limits: api.ResourceList{CPU: "4"}})
	if n1 == n4 {
		t.Error("substrateTemplateName() did not change with resources")
	}

	n5 := substrateTemplateName(image, baseCfg, nil)
	n6 := substrateTemplateName(image, baseCfg, config.BuiltinDefaultResources())
	if n5 != n6 {
		t.Error("substrateTemplateName() with nil resources did not match the resolved default resources (buildActorTemplate substitutes BuiltinDefaultResources for nil)")
	}

	configNameCfg := baseCfg
	configNameCfg.SandboxConfigName = "other-config"
	n7 := substrateTemplateName(image, configNameCfg, resources)
	if n1 == n7 {
		t.Error("substrateTemplateName() did not change with sandbox_config_name")
	}

	workerSelectorCfg := baseCfg
	workerSelectorCfg.WorkerSelector = map[string]string{"pool": "scion-agents"}
	n8 := substrateTemplateName(image, workerSelectorCfg, resources)
	if n1 == n8 {
		t.Error("substrateTemplateName() did not change with worker_selector")
	}

	snapshotCfg := baseCfg
	snapshotCfg.SnapshotStorage = "gs://other-bucket/prefix/"
	n9 := substrateTemplateName(image, snapshotCfg, resources)
	if n1 == n9 {
		t.Error("substrateTemplateName() did not change with snapshot_storage")
	}
}

// TestBuildActorTemplate_SecurityContextGrantsExactlyRequiredCapabilities
// confirms the container's SecurityContext carries exactly
// substratecaps.Required — no more, no less, and in particular never
// silently falls back to fewer capabilities than checkPrivilegeDropFeasible
// (cmd/sciontool/commands) verifies. "ALL" is rejected by Substrate and
// drop is applied before add, so this must name each capability explicitly
// rather than lean on a default set.
func TestBuildActorTemplate_SecurityContextGrantsExactlyRequiredCapabilities(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("b", 64)
	tmpl := buildActorTemplate("scion-test", "scion-abc123", image, config.V1SubstrateConfig{}, nil)

	if len(tmpl.GetContainers()) != 1 {
		t.Fatalf("Containers = %d, want 1", len(tmpl.GetContainers()))
	}
	sc := tmpl.GetContainers()[0].GetSecurityContext()
	if sc == nil {
		t.Fatalf("Container.SecurityContext is nil, want Capabilities.Add = %v", substratecaps.Names())
	}
	caps := sc.GetCapabilities()
	if caps == nil {
		t.Fatalf("SecurityContext.Capabilities is nil, want Add = %v", substratecaps.Names())
	}
	want := substratecaps.Names()
	if !slices.Equal(caps.GetAdd(), want) {
		t.Errorf("Capabilities.Add = %v, want %v (substratecaps.Required)", caps.GetAdd(), want)
	}
	if len(caps.GetDrop()) != 0 {
		t.Errorf("Capabilities.Drop = %v, want empty — this template only ever adds", caps.GetDrop())
	}
}

// TestSubstrateTemplateName_ChangesWithImageDigest confirms the image is
// part of the template's content-address: ensureActorTemplate's reuse
// decision (same templateName -> assume the existing golden template is
// still current) must not reuse a template built from a different image, or
// an actor started from it would run stale image content under a name that
// claims to match the current one. TestSubstrateTemplateName_Stable holds
// the image fixed across all of its cases and never varies it, so this is
// the only place that exercises this input.
func TestSubstrateTemplateName_ChangesWithImageDigest(t *testing.T) {
	cfg := config.V1SubstrateConfig{SandboxClass: "gvisor"}
	image1 := "repo/image@sha256:" + strings.Repeat("a", 64)
	image2 := "repo/image@sha256:" + strings.Repeat("b", 64)

	n1 := substrateTemplateName(image1, cfg, nil)
	n2 := substrateTemplateName(image2, cfg, nil)
	if n1 == n2 {
		t.Error("substrateTemplateName() did not change with the image digest — an existing golden template would be silently reused with stale image content")
	}
}

// TestSubstrateTemplateName_ChangesWithCapabilitySet confirms the
// capabilities buildActorTemplate grants are part of the template's
// content-address: an existing golden template built before a capability
// change must not be silently reused after one, since it would still be
// running with the old (missing) capabilities.
func TestSubstrateTemplateName_ChangesWithCapabilitySet(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("b", 64)
	cfg := config.V1SubstrateConfig{SandboxClass: "gvisor"}

	before := substrateTemplateName(image, cfg, nil)

	original := substrateContainerCapabilitiesAdd
	substrateContainerCapabilitiesAdd = append([]string{}, original...)
	t.Cleanup(func() { substrateContainerCapabilitiesAdd = original })

	// Same capability set: hash must not change from this alone.
	same := substrateTemplateName(image, cfg, nil)
	if before != same {
		t.Errorf("substrateTemplateName() changed with no actual capability-set change: %q != %q", before, same)
	}

	substrateContainerCapabilitiesAdd = []string{"SETUID", "SETGID", "CHOWN", "FOWNER"}
	after := substrateTemplateName(image, cfg, nil)
	if before == after {
		t.Error("substrateTemplateName() did not change when the capability set changed — an existing golden template would be silently reused without the new capability")
	}
}

// TestBuildBootstrapEnv_SetsHostUIDGIDForPrivilegeDrop confirms the
// bootstrap env carries SCION_HOST_UID/GID — without them, `sciontool
// init`'s setupHostUser takes its "not configured, skip user setup" branch
// and leaves the whole process tree at UID 0 even once the container has
// the SETUID/SETGID capabilities to actually perform the drop. Substrate's
// workspace is never bind-mounted from the invoking broker's own
// filesystem (unlike Docker/Podman), so there is no host UID to
// synchronize with — 1000 is the actor image's own baked-in "scion" user
// (image-build/scion-base/Dockerfile: `useradd -u 1000 scion`), used
// unconditionally.
func TestBuildBootstrapEnv_SetsHostUIDGIDForPrivilegeDrop(t *testing.T) {
	env := buildBootstrapEnv(RunConfig{})
	if got := env["SCION_HOST_UID"]; got != "1000" {
		t.Errorf(`buildBootstrapEnv()["SCION_HOST_UID"] = %q, want "1000"`, got)
	}
	if got := env["SCION_HOST_GID"]; got != "1000" {
		t.Errorf(`buildBootstrapEnv()["SCION_HOST_GID"] = %q, want "1000"`, got)
	}
}

// -----------------------------------------------------------------------
// List: mapping and label filter
// -----------------------------------------------------------------------

func TestSubstrateList_MappingAndLabelFilter(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	// Seed the in-memory record List needs, as Run() would.
	substrateAgentStateMu.Lock()
	substrateAgentRecords["uid-a"] = &substrateAgentRecord{
		Labels:  map[string]string{"scion.agent_id": "agent-a"},
		Project: "proj-a",
	}
	substrateAgentRecords["uid-b"] = &substrateAgentRecord{
		Labels:  map[string]string{"scion.agent_id": "agent-b"},
		Project: "proj-b",
	}
	substrateAgentStateMu.Unlock()

	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{
					Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-a", Uid: "uid-a"},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
				},
				{
					Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-b", Uid: "uid-b"},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
				},
				{
					// No agentRecords entry — should still be listed, with
					// empty synthesised fields.
					Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-c", Uid: "uid-c"},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
				},
			},
		}, nil
	}

	all, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() returned %d agents, want 3", len(all))
	}

	byName := map[string]api.AgentInfo{}
	for _, a := range all {
		byName[a.Name] = a
	}

	if byName["agent-a"].Phase != "running" {
		t.Errorf("agent-a phase = %q, want running", byName["agent-a"].Phase)
	}
	if byName["agent-b"].Phase != "stopped" {
		t.Errorf("agent-b phase = %q, want stopped", byName["agent-b"].Phase)
	}
	if byName["agent-c"].Phase != "error" {
		t.Errorf("agent-c phase = %q, want error", byName["agent-c"].Phase)
	}
	if byName["agent-a"].ContainerID != "scion-proj/agent-a" {
		t.Errorf("agent-a ContainerID = %q, want scion-proj/agent-a", byName["agent-a"].ContainerID)
	}
	if byName["agent-a"].Runtime != "substrate" {
		t.Errorf("agent-a Runtime = %q, want substrate", byName["agent-a"].Runtime)
	}

	// Label filter: only agent-a matches project=proj-a.
	filtered, err := rt.List(context.Background(), map[string]string{"scion.agent_id": "agent-a"})
	if err != nil {
		t.Fatalf("List() with filter error = %v", err)
	}
	if len(filtered) != 1 || filtered[0].Name != "agent-a" {
		t.Errorf("List() with label filter = %v, want only agent-a", filtered)
	}
}

// -----------------------------------------------------------------------
// Exec: error mapping
// -----------------------------------------------------------------------

func TestSubstrateExec_ErrorMapping(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	fa.execResp = execResponse{Stdout: "partial output", Stderr: "command not found", ExitCode: 127}

	_, err := rt.Exec(context.Background(), id, []string{"nope"})
	if err == nil {
		t.Fatal("Exec() expected an error for a non-zero exit code, got nil")
	}
	if !strings.Contains(err.Error(), "command not found") {
		t.Errorf("Exec() error = %v, want it to include stderr", err)
	}
	if !strings.Contains(err.Error(), "127") {
		t.Errorf("Exec() error = %v, want it to include the exit code", err)
	}
}

func TestSubstrateExec_NoCachedToken(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	_, err := rt.Exec(context.Background(), "scion-proj/agent-unknown", []string{"true"})
	if err == nil {
		t.Fatal("Exec() expected an error when no control token is cached, got nil")
	}
}

// TestSubstrateDelete_FallsBackToDeleteActorUIDWhenGetActorFails is the
// regression test for the leaked-record bug: Delete's speculative GetActor
// call (used to resolve the actor's UID so substrateAgentRecords can be
// cleaned up) can fail for reasons that have nothing to do with whether the
// actor itself is deletable — a transient RPC error, or a race with some
// other caller's own lookup. Previously, a failed GetActor left uid empty
// and skipped the substrateAgentRecords cleanup entirely, leaking that
// record for the process's lifetime even though DeleteActor itself
// succeeded. Delete must fall back to the UID DeleteActor's own response
// names.
func TestSubstrateDelete_FallsBackToDeleteActorUIDWhenGetActorFails(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	substrateAgentStateMu.Lock()
	_, hadRecord := substrateAgentRecords[fakeActorUID]
	substrateAgentStateMu.Unlock()
	if !hadRecord {
		t.Fatalf("test setup: no record for uid %q after Run()", fakeActorUID)
	}

	fc.getActor = func(*ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
		return nil, status.Error(codes.Unavailable, "simulated transient GetActor failure")
	}
	fc.deleteActor = func(req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
		// A real ateapi DeleteActor response names the actor it just
		// removed — this is what Delete must fall back to.
		return &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{
				Atespace: req.GetActor().GetAtespace(),
				Name:     req.GetActor().GetName(),
				Uid:      fakeActorUID,
			},
		}, nil
	}

	if err := rt.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	substrateAgentStateMu.Lock()
	_, stillPresent := substrateAgentRecords[fakeActorUID]
	substrateAgentStateMu.Unlock()
	if stillPresent {
		t.Errorf("substrateAgentRecords[%q] still present after Delete() — leaked despite GetActor failing", fakeActorUID)
	}
}

func TestSubstrateExec_Success(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	fa.execResp = execResponse{Stdout: "hello", ExitCode: 0}

	out, err := rt.Exec(context.Background(), id, []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if out != "hello" {
		t.Errorf("Exec() = %q, want hello", out)
	}
	if len(fa.lastExec.Stdin) != 0 {
		t.Errorf("plain Exec() sent a non-empty Stdin field: %q, want none (Exec must be unaffected by ExecWithStdin)", fa.lastExec.Stdin)
	}
}

// TestSubstrateExec_StderrTruncatedInError is the regression test for
// bounding how much of a failed command's stderr doExec embeds directly in
// an error: without truncateForError, a command that fills the control
// server's own 4 MiB per-stream cap would turn one failed Exec call into a
// multi-megabyte error message.
func TestSubstrateExec_StderrTruncatedInError(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	hugeStderr := strings.Repeat("x", maxEmbeddedErrorBytes*2)
	fa.execResp = execResponse{Stderr: hugeStderr, ExitCode: 1}

	_, err := rt.Exec(context.Background(), id, []string{"nope"})
	if err == nil {
		t.Fatal("Exec() expected an error for a non-zero exit code, got nil")
	}
	if len(err.Error()) >= len(hugeStderr) {
		t.Errorf("Exec() error length = %d, want it bounded well under the full %d-byte stderr", len(err.Error()), len(hugeStderr))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Exec() error = %v, want a truncation marker", err.Error())
	}
}

// TestSubstrateExec_RedactsCachedSecretFromError proves Exec's error path
// redacts a secret value the same way Run's own errors do, via the
// substrateExecSecrets cache Run populates alongside the control token:
// a command's stderr that happens to echo back a secret value from the
// agent's own bootstrap env must never reach the caller verbatim.
func TestSubstrateExec_RedactsCachedSecretFromError(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	const secret = "sk-ultra-secret-credential-value"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateExecSecrets[id] = map[string]string{"ANTHROPIC_API_KEY": secret}
	substrateAgentStateMu.Unlock()
	t.Cleanup(func() {
		substrateAgentStateMu.Lock()
		delete(substrateExecSecrets, id)
		substrateAgentStateMu.Unlock()
	})

	fa.execResp = execResponse{Stderr: "failed: key=" + secret, ExitCode: 1}

	_, err := rt.Exec(context.Background(), id, []string{"nope"})
	if err == nil {
		t.Fatal("Exec() expected an error for a non-zero exit code, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Exec() error = %v, leaked the cached secret value", err)
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("Exec() error = %v, want it to name the redacted key", err)
	}
}

// straddleSecret is a cached secret value the straddle tests below place
// across the maxEmbeddedErrorBytes cut. Its leading bytes appear nowhere
// else in the test output, so any surviving prefix is detectable.
const straddleSecret = "sk-STRADDLE-0123456789abcdefghijklmnopqrstuvwxyz"

// cacheStraddleSecret registers straddleSecret as id's cached exec secret.
func cacheStraddleSecret(t *testing.T, id string) {
	t.Helper()
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateExecSecrets[id] = map[string]string{"ANTHROPIC_API_KEY": straddleSecret}
	substrateAgentStateMu.Unlock()
	t.Cleanup(func() {
		substrateAgentStateMu.Lock()
		delete(substrateExecSecrets, id)
		substrateAgentStateMu.Unlock()
	})
}

// assertNoSecretPrefix fails if msg contains any leading fragment of
// straddleSecret long enough to be recognizable.
func assertNoSecretPrefix(t *testing.T, msg string) {
	t.Helper()
	const minFragment = 4
	for n := len(straddleSecret); n >= minFragment; n-- {
		if strings.Contains(msg, straddleSecret[:n]) {
			t.Fatalf("error leaks %d leading bytes of the secret (%q): %s", n, straddleSecret[:n], msg)
		}
	}
}

// TestSubstrateExec_SecretStraddlingStderrCutIsRedacted: a failed command's
// stderr carries a cached secret that begins just before the
// maxEmbeddedErrorBytes cut. Redacting after truncating would leave the
// secret's first bytes in the error, because the cut-off remainder no
// longer matches the secret; doExec must redact first, then truncate.
func TestSubstrateExec_SecretStraddlingStderrCutIsRedacted(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	cacheStraddleSecret(t, id)

	stderr := strings.Repeat("x", maxEmbeddedErrorBytes-10) + straddleSecret + strings.Repeat("y", maxEmbeddedErrorBytes)
	fa.execResp = execResponse{Stderr: stderr, ExitCode: 1}

	_, err := rt.Exec(context.Background(), id, []string{"nope"})
	if err == nil {
		t.Fatal("Exec() expected an error for a non-zero exit code, got nil")
	}
	assertNoSecretPrefix(t, err.Error())
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Exec() error lacks a truncation marker, so the cut was never exercised: %s", err)
	}
}

// TestSubstrateExec_SecretStraddlingResponseBodyCutIsRedacted is the same
// check for a non-2xx exec response, whose body doExec embeds in its error.
func TestSubstrateExec_SecretStraddlingResponseBodyCutIsRedacted(t *testing.T) {
	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	cacheStraddleSecret(t, id)

	// The fake server writes execResp as JSON, so place the secret by its
	// offset within that encoded body.
	resp := execResponse{Stderr: "PAD" + straddleSecret + strings.Repeat("y", maxEmbeddedErrorBytes)}
	encoded, mErr := json.Marshal(resp)
	if mErr != nil {
		t.Fatal(mErr)
	}
	offset := strings.Index(string(encoded), straddleSecret)
	resp.Stderr = "PAD" + strings.Repeat("x", maxEmbeddedErrorBytes-10-offset) + straddleSecret + strings.Repeat("y", maxEmbeddedErrorBytes)
	encoded, _ = json.Marshal(resp)
	if got := strings.Index(string(encoded), straddleSecret); got != maxEmbeddedErrorBytes-10 {
		t.Fatalf("fixture places the secret at body offset %d, want %d", got, maxEmbeddedErrorBytes-10)
	}
	fa.execStatus = http.StatusInternalServerError
	fa.execResp = resp

	_, err := rt.Exec(context.Background(), id, []string{"nope"})
	if err == nil {
		t.Fatal("Exec() expected an error for a non-2xx response, got nil")
	}
	assertNoSecretPrefix(t, err.Error())
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Exec() error lacks a truncation marker, so the cut was never exercised: %s", err)
	}
}

// TestSubstrateRun_SecretStraddlingBootstrapBodyCutIsRedacted: a failed
// bootstrap's response body carries one of the agent's own env values
// beginning just before the maxEmbeddedErrorBytes cut. postBootstrap must
// redact the full body before truncating it; truncating first (or reading
// only up to the cut) leaves the secret's leading bytes in Run's error,
// because the cut-off remainder no longer matches the value Run's final
// redaction pass searches for. Both the generic non-2xx path and the 422
// path whose body does not parse (which embeds the body as detail) are
// covered.
func TestSubstrateRun_SecretStraddlingBootstrapBodyCutIsRedacted(t *testing.T) {
	body := strings.Repeat("x", maxEmbeddedErrorBytes-10) + straddleSecret + strings.Repeat("y", maxEmbeddedErrorBytes)
	for name, status := range map[string]int{
		"generic non-2xx": http.StatusInternalServerError,
		"unparseable 422": http.StatusUnprocessableEntity,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &callRecorder{}
			rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
			defer closeServer()
			fa.bootstrapStatus = status
			fa.bootstrapBody = body

			cfg := testSubstrateRunConfig()
			cfg.Env = append(cfg.Env, "ANTHROPIC_API_KEY="+straddleSecret)
			_, err := rt.Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("Run() expected an error for a failed bootstrap, got nil")
			}
			assertNoSecretPrefix(t, err.Error())
			if !strings.Contains(err.Error(), "truncated") {
				t.Errorf("Run() error lacks a truncation marker, so the cut was never exercised: %s", err)
			}
		})
	}
}

// TestTruncateForError_NeverSplitsARune: a multi-byte character spanning
// the maxEmbeddedErrorBytes boundary is dropped whole rather than cut in
// half, so the result stays valid UTF-8.
func TestTruncateForError_NeverSplitsARune(t *testing.T) {
	s := strings.Repeat("x", maxEmbeddedErrorBytes-1) + strings.Repeat("é", 10)
	got := truncateForError(s)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForError produced invalid UTF-8: %q", got[maxEmbeddedErrorBytes-8:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", maxEmbeddedErrorBytes-1)+"...[truncated ") {
		t.Errorf("truncateForError = %q..., want the cut just before the split rune", got[maxEmbeddedErrorBytes-8:])
	}
	if want := "[truncated 20 bytes]"; !strings.HasSuffix(got, want) {
		t.Errorf("truncateForError suffix = %q, want %q", got[maxEmbeddedErrorBytes-1:], want)
	}
}

// -----------------------------------------------------------------------
// ExecWithStdin: secret delivered via stdin, never via argv
// -----------------------------------------------------------------------

func TestSubstrateExecWithStdin_DeliversViaStdinFieldNotArgv(t *testing.T) {
	const secret = "S3CR3T-1894-STDIN-NOT-ARGV"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	fa.execResp = execResponse{Stdout: secret, ExitCode: 0, StdinSupported: true}

	// fakeActorServer's exec handler returns whatever fa.execResp is set to
	// regardless of what it received, so asserting the returned Stdout
	// equals secret here would only prove this test's own fixture echoes
	// back what it was told to — not that stdin was actually delivered. The
	// real round trip through a real control-server handler and a real
	// subprocess is proven server-side in
	// pkg/sciontool/substrate/server_test.go's
	// TestExec_StdinRoundTripsThroughRealCommand (see the comment further
	// down this file for why that test can't also run from here). What this
	// test proves is what the client actually put on the wire, checked
	// below via fa.lastExec.
	_, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, strings.NewReader(secret))
	if err != nil {
		t.Fatalf("ExecWithStdin() error = %v", err)
	}
	if fa.lastExec == nil {
		t.Fatal("server never received an exec request")
	}
	if string(fa.lastExec.Stdin) != secret {
		t.Errorf("server-side ExecRequest.Stdin = %q, want %q", fa.lastExec.Stdin, secret)
	}
	for _, a := range fa.lastExec.Argv {
		if strings.Contains(a, secret) {
			t.Errorf("secret leaked into ExecRequest.Argv: %q", fa.lastExec.Argv)
		}
	}
}

func TestSubstrateExecWithStdin_MissingStdinSupportedIsError(t *testing.T) {
	const secret = "S3CR3T-VERSION-SKEW"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	// Simulate an older control server: it ran the command and returned a
	// clean exit, but doesn't know about ExecRequest.Stdin at all, so it
	// never set StdinSupported. The client must not treat this as success.
	fa.execResp = execResponse{ExitCode: 0}

	_, err := rt.ExecWithStdin(context.Background(), id, []string{"sh", "-c", "cat > realfile"}, strings.NewReader(secret))
	if err == nil {
		t.Fatal("ExecWithStdin() expected an error when the server doesn't confirm stdin_supported, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("ExecWithStdin() error leaked the stdin content: %v", err)
	}
	// The version-skew rejection happens on the pre-exec capability probe,
	// before the real command is ever attempted: exactly one exec call
	// reaches the server, and it is the probe's own no-op argv, never the
	// caller's real command.
	if got := len(rec.list()); got != 1 {
		t.Fatalf("exec calls to the server = %d, want exactly 1 (the probe only, real command must never run)", got)
	}
	if fa.lastExec == nil || len(fa.lastExec.Argv) != 1 || fa.lastExec.Argv[0] != "true" {
		var argv []string
		if fa.lastExec != nil {
			argv = fa.lastExec.Argv
		}
		t.Errorf("the one exec call's argv = %v, want the probe's [\"true\"] — the real command must never reach the server", argv)
	}
}

func TestSubstrateExecWithStdin_OversizeRejectedWithoutEchoing(t *testing.T) {
	const secret = "S3CR3T-OVERSIZE-MARKER"

	rec := &callRecorder{}
	rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id := "scion-proj/agent-a"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-123"
	substrateAgentStateMu.Unlock()

	// Exactly one byte over the cap — not some comfortably-over size — so
	// this pins the boundary itself rather than merely "oversize inputs are
	// rejected somewhere."
	filler := maxExecStdinBytes + 1 - len(secret)
	over := strings.NewReader(secret + strings.Repeat("A", filler))

	_, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, over)
	if err == nil {
		t.Fatal("ExecWithStdin() expected an error for oversize stdin, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("ExecWithStdin() error leaked the oversize stdin content: %v", err)
	}
	if fa.lastExec != nil {
		t.Error("oversize stdin reached the control server; it should have been rejected client-side first")
	}
}

// TestSubstrateExecWithStdin_ProbeStopsBeforeRealCommandOnOldServer simulates
// a control server old enough to predate ExecRequest.Stdin, using the real
// wire types (pkg/sciontool/substrate.ExecRequest/ExecResponse) so the
// decode step this test relies on is the genuine shared contract, not the
// client's own mirrored copy of it. The handler here stands in for the old
// server's own (unmodified) code: it decodes an ExecRequest exactly as an
// old server would (silently keeping the fields it knows and dropping the
// one it doesn't), runs nothing for real, and never sets StdinSupported —
// exactly what an old server's response looks like. The point under test is
// entirely client-side: that the pre-exec probe is what stops here, before
// the real, destructive command is ever sent.
func TestSubstrateExecWithStdin_ProbeStopsBeforeRealCommandOnOldServer(t *testing.T) {
	const secret = "S3CR3T-MUST-NOT-REACH-REAL-COMMAND"

	var mu sync.Mutex
	var execCalls []sciontoolsubstrate.ExecRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/scion/v1/exec", func(w http.ResponseWriter, r *http.Request) {
		var req sciontoolsubstrate.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		execCalls = append(execCalls, req)
		mu.Unlock()
		// An old server: understands Argv/User/TimeoutS, has no idea Stdin
		// exists, runs the command with nothing attached to its stdin, and
		// its response has no stdin_supported field at all.
		_ = json.NewEncoder(w).Encode(sciontoolsubstrate.ExecResponse{ExitCode: 0})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	resetSubstrateAgentStateForTest(t)
	rt := NewSubstrateRuntimeForTest(nil, substrate.NewRouterClient(server.URL), nil, config.V1SubstrateConfig{})
	id := "scion-proj/agent-old-server"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = "tok-old-server"
	substrateAgentStateMu.Unlock()

	_, err := rt.ExecWithStdin(context.Background(), id, []string{"sh", "-c", "cat > realfile"}, strings.NewReader(secret))
	if err == nil {
		t.Fatal("ExecWithStdin() expected an error against a server that never confirms stdin support, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("ExecWithStdin() error leaked the stdin content: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(execCalls) != 1 {
		t.Fatalf("exec requests reaching the server = %d, want exactly 1 (the probe only)", len(execCalls))
	}
	if got := execCalls[0].Argv; len(got) != 1 || got[0] != "true" {
		t.Errorf("the only request's argv = %v, want the probe's [\"true\"] — the real command must never be sent", got)
	}
}

// fakeScionUserIsCurrentIdentity installs a *user.User stand-in, via
// pkg/sciontool/substrate's exported SetExecUserLookupForTest seam, naming
// "scion" but carrying THIS test process's own real uid/gid rather than a
// real "scion" account's. The real control server's exec path
// (pkg/sciontool/substrate's runExec, via execUserCredential) skips its own
// credential drop whenever the resolved target's uid/gid already match the
// caller's own euid/egid (see execUserCredential's doc comment for why:
// Go's exec implementation calls setgroups() for any non-nil Credential,
// which needs CAP_SETGID even to set an unchanged group list); with this
// stub in place that's true for any caller targeting "scion" (which is all
// of these tests, matching SubstrateRuntime.ExecUser()'s hard-coded value),
// so this runs for real on any user rather than needing real root/CAP_SETUID
// or a genuine "scion" account on the test machine. The stand-in's home is
// a fresh temporary directory, since runExec runs the child there and
// refuses a missing one; the test user's own home need not exist. Exec
// refuses a target that resolves to uid 0, so this skips when run as root.
// Restores the seam in t.Cleanup.
func fakeScionUserIsCurrentIdentity(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: exec refuses a target user that resolves to uid 0")
	}
	fakeUser := &user.User{
		Uid:      strconv.Itoa(os.Geteuid()),
		Gid:      strconv.Itoa(os.Getegid()),
		Username: "scion",
		HomeDir:  t.TempDir(),
	}
	restore := sciontoolsubstrate.SetExecUserLookupForTest(func(username string) (*user.User, error) {
		return fakeUser, nil
	})
	t.Cleanup(restore)
}

// newRealSubstrateServeHarness starts a real pkg/sciontool/substrate.Server
// behind httptest, bootstraps it for real over HTTP, and returns a
// SubstrateRuntime pointed at it plus the agent id to use — no fake stands
// in for either the client or the server. fakeScionUserIsCurrentIdentity
// makes the server's own real exec path skip its credential drop regardless
// of which user is actually running the test, so this runs for real on any
// user rather than being skipped. SetPrivateRootTmpDirForTest redirects the
// server's private-scratch-directory bootstrap step at a throwaway
// directory this test process does own (the real default,
// "/run/scion/tmp", requires root); a missing enforced-hooks directory is
// already a no-op in production code, so nothing else needs redirecting.
func newRealSubstrateServeHarness(t *testing.T) (*SubstrateRuntime, string) {
	t.Helper()
	fakeScionUserIsCurrentIdentity(t)

	restoreTmpDir := sciontoolsubstrate.SetPrivateRootTmpDirForTest(t.TempDir())
	t.Cleanup(restoreTmpDir)

	srv := sciontoolsubstrate.NewServer(
		sciontoolsubstrate.WithChownOwner(-1, -1),
		sciontoolsubstrate.WithInitRunner(func(argv []string, forwardTermSignal bool) int { return 0 }),
	)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)

	const controlToken = "real-harness-control-token"
	bootstrapBody, err := json.Marshal(sciontoolsubstrate.BootstrapRequest{
		StartCmd:     "true",
		ControlToken: controlToken,
	})
	if err != nil {
		t.Fatalf("marshal bootstrap request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/scion/v1/bootstrap", bytes.NewReader(bootstrapBody))
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer any-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bootstrap request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200", resp.StatusCode)
	}

	resetSubstrateAgentStateForTest(t)
	rt := NewSubstrateRuntimeForTest(nil, substrate.NewRouterClient(server.URL), nil, config.V1SubstrateConfig{})
	id := "scion-proj/agent-real-harness"
	substrateAgentStateMu.Lock()
	substrateControlTokens[id] = controlToken
	substrateAgentStateMu.Unlock()

	return rt, id
}

// TestSubstrateExecWithStdin_EndToEndRealControlServer wires a real
// SubstrateRuntime client to a real pkg/sciontool/substrate.Server handler
// over httptest — no fakes on either side of the exec path — and proves the
// full round trip: the client's probe and its real exec both reach a real
// handler backed by a real subprocess, and the secret sent as stdin comes
// back out through that subprocess's real stdout. This is the only test
// that exercises the probe's happy path (a real "true" invocation) against
// a real server, not a fake that would accept any argv it was handed. Runs
// on any process user, not just "scion"; see newRealSubstrateServeHarness.
func TestSubstrateExecWithStdin_EndToEndRealControlServer(t *testing.T) {
	const secret = "S3CR3T-END-TO-END-REAL-SERVER"

	rt, id := newRealSubstrateServeHarness(t)

	out, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, strings.NewReader(secret))
	if err != nil {
		t.Fatalf("ExecWithStdin() error = %v", err)
	}
	if out != secret {
		t.Errorf("ExecWithStdin() = %q, want %q from a real cat subprocess echoing real stdin", out, secret)
	}
}

// TestSubstrateExecWithStdin_ExactCapAcceptedByRealControlServer sends
// exactly maxExecStdinBytes through the real server harness, so the server's
// own request-body LimitReader — not just the client's own cap check — is
// what accepts it. Proves the derived cap is not merely internally
// consistent on the client side but actually usable end to end. Runs on any
// process user, not just "scion"; see newRealSubstrateServeHarness.
func TestSubstrateExecWithStdin_ExactCapAcceptedByRealControlServer(t *testing.T) {
	rt, id := newRealSubstrateServeHarness(t)

	payload := bytes.Repeat([]byte("A"), maxExecStdinBytes)
	out, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("ExecWithStdin() at exactly the cap: error = %v", err)
	}
	if len(out) != maxExecStdinBytes {
		t.Errorf("ExecWithStdin() at the cap returned %d bytes, want %d", len(out), maxExecStdinBytes)
	}
}

// TestExecRequestAtCap_MarshalledSizeWithinServerLimit pins the arithmetic
// the derived cap depends on: a realistic ExecWithStdin call (resetAuth's
// own write-then-rename argv, plus stdin at exactly maxExecStdinBytes),
// marshalled exactly as doExec marshals it, must fit within the server's own
// MaxExecBodyBytes. Without this, a change to ExecEnvelopeAllowanceBytes or
// to the 3/4 factor could silently break the derivation with nothing to
// catch it — the end-to-end tests above only prove today's numbers work,
// not that the formula stays correct if either constant changes independently.
func TestExecRequestAtCap_MarshalledSizeWithinServerLimit(t *testing.T) {
	resetAuthArgv := []string{"sh", "-c",
		"TOKEN_DIR=\"$(getent passwd scion 2>/dev/null | cut -d: -f6 || echo /home/scion)/.scion\" && " +
			"mkdir -p \"$TOKEN_DIR\" && " +
			"cat > \"$TOKEN_DIR/scion-token.tmp\" && " +
			"mv \"$TOKEN_DIR/scion-token.tmp\" \"$TOKEN_DIR/scion-token\"",
	}

	body, err := json.Marshal(execRequest{
		Argv:     resetAuthArgv,
		User:     "scion",
		TimeoutS: 60,
		Stdin:    make([]byte, maxExecStdinBytes),
	})
	if err != nil {
		t.Fatalf("marshal execRequest at the cap: %v", err)
	}
	if len(body) > sciontoolsubstrate.MaxExecBodyBytes {
		t.Errorf("marshalled ExecRequest at the cap is %d bytes, want <= MaxExecBodyBytes (%d) — the derived cap no longer fits the server's limit",
			len(body), sciontoolsubstrate.MaxExecBodyBytes)
	}
}

// TestSubstrateExecWithStdin_ProbeErrorAttribution pins that the capability
// probe names version skew ONLY when the control server itself answered but
// never confirmed StdinSupported — every other way the probe can fail
// (a transport error, an authorization rejection) is passed through
// unchanged, since neither has anything to do with an old sciontool image.
func TestSubstrateExecWithStdin_ProbeErrorAttribution(t *testing.T) {
	const skewPhrase = "may be running an image older"

	t.Run("transport error is not named as version skew", func(t *testing.T) {
		rec := &callRecorder{}
		rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
		id := "scion-proj/agent-a"
		substrateAgentStateMu.Lock()
		substrateControlTokens[id] = "tok-123"
		substrateAgentStateMu.Unlock()
		closeServer() // the router now points at a closed listener.

		_, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, strings.NewReader("x"))
		if err == nil {
			t.Fatal("expected an error against a closed server, got nil")
		}
		if strings.Contains(err.Error(), skewPhrase) {
			t.Errorf("transport error was misreported as version skew: %v", err)
		}
	})

	t.Run("an authorization error is not named as version skew", func(t *testing.T) {
		rec := &callRecorder{}
		rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
		defer closeServer()
		id := "scion-proj/agent-a"
		substrateAgentStateMu.Lock()
		substrateControlTokens[id] = "tok-123"
		substrateAgentStateMu.Unlock()
		fa.execStatus = http.StatusUnauthorized

		_, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, strings.NewReader("x"))
		if err == nil {
			t.Fatal("expected an error for a 401 response, got nil")
		}
		if strings.Contains(err.Error(), skewPhrase) {
			t.Errorf("authorization error was misreported as version skew: %v", err)
		}
	})

	t.Run("a missing stdin_supported confirmation is named as version skew", func(t *testing.T) {
		rec := &callRecorder{}
		rt, _, fa, closeServer := newTestSubstrateHarness(t, rec)
		defer closeServer()
		id := "scion-proj/agent-a"
		substrateAgentStateMu.Lock()
		substrateControlTokens[id] = "tok-123"
		substrateAgentStateMu.Unlock()
		fa.execResp = execResponse{ExitCode: 0} // no StdinSupported.

		_, err := rt.ExecWithStdin(context.Background(), id, []string{"cat"}, strings.NewReader("x"))
		if err == nil {
			t.Fatal("expected an error when stdin support is never confirmed, got nil")
		}
		if !strings.Contains(err.Error(), skewPhrase) {
			t.Errorf("expected the version-skew explanation for an unconfirmed StdinSupported, got: %v", err)
		}
	})
}

// -----------------------------------------------------------------------
// Redaction: no secret env values leak into a Run error
// -----------------------------------------------------------------------

func TestSubstrateRun_ErrorDoesNotLeakEnvValues(t *testing.T) {
	const (
		envSentinel          = "FAKE-KEY-SENTINEL-cfg-env-not-a-real-credential"
		resolvedAuthSentinel = "FAKE-KEY-SENTINEL-resolved-auth-not-a-real-credential"
		authFileSentinel     = "FAKE-KEY-SENTINEL-resolved-auth-file-not-a-real-credential"
		envSecretSentinel    = "FAKE-KEY-SENTINEL-env-secret-not-a-real-credential"
		fileSecretSentinel   = "FAKE-KEY-SENTINEL-file-secret-not-a-real-credential"
		// collidingSentinel is the value of a cfg.Env entry AND a file-type
		// ResolvedSecret that share the same name ("COLLIDING_KEY"). With a
		// bare map key, both would be stored under the same key, so adding
		// the second would silently drop the first from the redaction set
		// while its value stayed in the request.
		collidingEnvSentinel    = "FAKE-KEY-SENTINEL-colliding-env-not-a-real-credential"
		collidingSecretSentinel = "FAKE-KEY-SENTINEL-colliding-secret-not-a-real-credential"
	)

	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	authFilePath := filepath.Join(t.TempDir(), "auth-file")
	if err := os.WriteFile(authFilePath, []byte(authFileSentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fail a step whose error text, in a naive implementation, might
	// interpolate the whole request — resumeActor's error text stands in
	// for a hypothetical verbose upstream error that echoes back
	// everything the runtime sent it, regardless of source.
	fc.resumeActor = func(*ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
		return nil, status.Error(codes.Internal, "resume boom, env dump: "+
			"CFG_ENV_KEY="+envSentinel+" "+
			"RESOLVED_AUTH_KEY="+resolvedAuthSentinel+" "+
			"RESOLVED_AUTH_FILE="+authFileSentinel+" "+
			"ENV_SECRET_KEY="+envSecretSentinel+" "+
			"FILE_SECRET_KEY="+fileSecretSentinel+" "+
			"COLLIDING_KEY(env)="+collidingEnvSentinel+" "+
			"COLLIDING_KEY(secret)="+collidingSecretSentinel)
	}

	cfg := testSubstrateRunConfig()
	cfg.Env = append(cfg.Env,
		"CFG_ENV_KEY="+envSentinel,
		"COLLIDING_KEY="+collidingEnvSentinel,
	)
	cfg.ResolvedAuth = &api.ResolvedAuth{
		EnvVars: map[string]string{"RESOLVED_AUTH_KEY": resolvedAuthSentinel},
		Files:   []api.FileMapping{{SourcePath: authFilePath, ContainerPath: "~/.creds/auth-file"}},
	}
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "ENV_SECRET_KEY", Type: "environment", Target: "ENV_SECRET_KEY", Value: envSecretSentinel},
		{Name: "FILE_SECRET_KEY", Type: "file", Target: "~/.creds/file-secret", Value: fileSecretSentinel},
		// Same Name as the cfg.Env entry above, deliberately.
		{Name: "COLLIDING_KEY", Type: "file", Target: "~/.creds/colliding", Value: collidingSecretSentinel},
	}

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() expected an error, got nil")
	}
	got := err.Error()

	for name, sentinel := range map[string]string{
		"cfg.Env":                    envSentinel,
		"ResolvedAuth.EnvVars":       resolvedAuthSentinel,
		"ResolvedAuth.Files content": authFileSentinel,
		"ResolvedSecrets (env)":      envSecretSentinel,
		"ResolvedSecrets (file)":     fileSecretSentinel,
		"colliding cfg.Env":          collidingEnvSentinel,
		"colliding ResolvedSecret":   collidingSecretSentinel,
	} {
		if strings.Contains(got, sentinel) {
			t.Errorf("CREDENTIAL LEAK: Run() error contains the %s secret value.\nerror: %s", name, got)
		}
	}
	for _, key := range []string{"CFG_ENV_KEY", "RESOLVED_AUTH_KEY", "ENV_SECRET_KEY", "FILE_SECRET_KEY", "COLLIDING_KEY"} {
		if !strings.Contains(got, key) {
			t.Errorf("redacted error does not name the redacted key %q.\nerror: %s", key, got)
		}
	}
	if !strings.Contains(got, "resume boom") {
		t.Errorf("error is no longer useful: diagnostic text was also removed.\nerror: %s", got)
	}
}

// -----------------------------------------------------------------------
// List: pagination and cross-tenant atespace filtering
// -----------------------------------------------------------------------

func TestSubstrateList_Pagination(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	var pageTokensSeen []string
	fc.listActors = func(req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		pageTokensSeen = append(pageTokensSeen, req.GetPageToken())
		switch req.GetPageToken() {
		case "":
			return &ateapipb.ListActorsResponse{
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-a", Uid: "uid-a"},
						Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				},
				NextPageToken: "page-2",
			}, nil
		case "page-2":
			return &ateapipb.ListActorsResponse{
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-b", Uid: "uid-b"},
						Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				},
				NextPageToken: "",
			}, nil
		default:
			t.Fatalf("unexpected page token %q", req.GetPageToken())
			return nil, nil
		}
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("List() returned %d agents, want 2 (one per page); got %v", len(agents), agents)
	}
	if len(pageTokensSeen) != 2 || pageTokensSeen[0] != "" || pageTokensSeen[1] != "page-2" {
		t.Errorf("ListActors called with page tokens %v, want [\"\", \"page-2\"]", pageTokensSeen)
	}
}

// TestSubstrateList_RepeatedPageTokenIsAnError proves List detects a
// server that returns the same next_page_token twice in a row (a sign of a
// broken or malicious server, since a real paging cursor always advances)
// and fails explicitly rather than looping on it forever.
func TestSubstrateList_RepeatedPageTokenIsAnError(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	calls := 0
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		calls++
		return &ateapipb.ListActorsResponse{NextPageToken: "stuck"}, nil
	}

	agents, err := rt.List(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "repeated page token") {
		t.Fatalf("List() err = %v, want a repeated-page-token error", err)
	}
	if agents != nil {
		t.Errorf("List() agents = %v, want nil alongside the error", agents)
	}
	if calls != 2 {
		t.Errorf("ListActors calls = %d, want 2 (stop at the first repeated token)", calls)
	}
}

// TestSubstrateList_EndlessPagingHitsPageCap proves a server that returns a
// fresh next_page_token on every call — never repeating, never going empty
// — is cut off by maxActorListPages with an explicit error, rather than
// spinning until ctx expires.
func TestSubstrateList_EndlessPagingHitsPageCap(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	// maxActorListPages is a package var (not a const) precisely so this
	// test can lower it, rather than paging through the real production
	// cap's worth of fake responses.
	origCap := maxActorListPages
	maxActorListPages = 5
	t.Cleanup(func() { maxActorListPages = origCap })

	calls := 0
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		calls++
		if calls > maxActorListPages+1 {
			return nil, fmt.Errorf("List exceeded the page cap (call %d)", calls)
		}
		return &ateapipb.ListActorsResponse{NextPageToken: fmt.Sprintf("p%d", calls)}, nil
	}

	agents, err := rt.List(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("List() err = %v, want a page-cap error", err)
	}
	if agents != nil {
		t.Errorf("List() agents = %v, want nil alongside the error", agents)
	}
	if calls != maxActorListPages {
		t.Errorf("ListActors calls = %d, want exactly %d", calls, maxActorListPages)
	}
}

func TestSubstrateList_SkipsNonScionAtespaces(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "agent-a", Uid: "uid-a"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				{Metadata: &ateapipb.ResourceMetadata{Atespace: "other-tenant", Name: "not-ours", Uid: "uid-x"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
			},
		}, nil
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "agent-a" {
		t.Errorf("List() = %v, want only agent-a (the other actor's atespace doesn't start with \"scion-\")", agents)
	}
}

func TestSubstrateList_SynthesisesNameAndAgentLabelsWithoutRecord(t *testing.T) {
	// No agentRecords entry at all for this actor — e.g. it was created by
	// a different SubstrateRuntime instance, or this instance just
	// restarted. List must still expose "scion.agent" (so it appears in an
	// unfiltered listing) even though — see List's doc comment — it never
	// gets a slug-shaped "scion.name": that stays the actor name.
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	const actorName = "myproj--orphaned-agent" // containerName("myproj", "orphaned-agent")
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: actorName, Uid: "uid-orphan"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
			},
		}, nil
	}

	agents, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List() with scion.agent filter = %v, want exactly the orphaned actor (no record needed)", agents)
	}
	if agents[0].Labels["scion.agent"] != "true" {
		t.Errorf("agent Labels[scion.agent] = %q, want \"true\"", agents[0].Labels["scion.agent"])
	}
	if agents[0].Name != actorName {
		t.Errorf("Name = %q, want the actor name %q (a record-less actor is never given a slug-shaped name)", agents[0].Name, actorName)
	}
}

// TestSubstrateList_NameIsAgentSlugNotActorName is the regression test for
// a known AgentManager.Delete/Stop limitation (silently no-op when
// Runtime.List can't find a matching agent): the actor
// name is containerName(project, agent) = "<project>--<agent>"
// (pkg/agent/run.go), so if AgentInfo.Name echoed the actor name back
// verbatim, it would never match the bare agent slug a caller like
// AgentManager.Delete looks up by — exactly what DockerRuntime.List
// (labels["scion.name"], falling back to the raw container name only when
// that label is absent) avoids.
//
// This is fixed only for a record-having actor: rec.Labels["scion.name"]
// carries the real slug. A record-less actor has no such independently
// verified value to fall back on, so List deliberately does not try to
// recover one from the actor name — see List's doc comment for why — and
// a slug lookup for it finds nothing, by design.
func TestSubstrateList_NameIsAgentSlugNotActorName(t *testing.T) {
	const (
		atespace  = "scion-proj"
		actorName = "myproj--sb-smoke-2" // containerName("myproj", "sb-smoke-2")
		agentSlug = "sb-smoke-2"
	)

	t.Run("record exists", func(t *testing.T) {
		rec := &callRecorder{}
		rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
		defer closeServer()

		const uid = "uid-with-record"
		fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
			return &ateapipb.ListActorsResponse{
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: actorName, Uid: uid},
						Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				},
			}, nil
		}
		substrateAgentStateMu.Lock()
		substrateAgentRecords[uid] = &substrateAgentRecord{
			Labels: map[string]string{"scion.name": agentSlug, "scion.agent": "true"},
		}
		substrateAgentStateMu.Unlock()

		agents, err := rt.List(context.Background(), map[string]string{"scion.name": agentSlug})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("List() with scion.name=%q filter = %v, want exactly the one matching actor", agentSlug, agents)
		}
		if agents[0].Name != agentSlug {
			t.Errorf("Name = %q, want the agent slug %q (not the actor name %q)", agents[0].Name, agentSlug, actorName)
		}
	})

	t.Run("no record (simulated broker restart) never matches by slug", func(t *testing.T) {
		rec := &callRecorder{}
		rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
		defer closeServer()

		fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
			return &ateapipb.ListActorsResponse{
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: actorName, Uid: "uid-no-record"},
						Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				},
			}, nil
		}
		// Deliberately no substrateAgentRecords entry for "uid-no-record".

		agents, err := rt.List(context.Background(), map[string]string{"scion.name": agentSlug})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(agents) != 0 {
			t.Fatalf("List() with scion.name=%q filter = %v, want no matches (a record-less actor is never resolvable by slug)", agentSlug, agents)
		}

		all, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(all) != 1 || all[0].Name != actorName {
			t.Fatalf("List(scion.agent=true) = %v, want the actor present under its actor name %q", all, actorName)
		}
	})
}

// TestSubstrateList_RecordlessActorsNeverMatchedBySlug is the exact
// live-cluster repro: two record-less actors in different projects share
// the same agent name ("dev") but not the same actor name (each is
// project-prefixed). Neither is resolvable by that bare slug — List always
// reports a record-less actor under its full actor name — so a
// caller-supplied "dev" lookup matches neither, rather than an arbitrary
// one of the two depending on ListActors' return order. Run with both
// actor orderings to confirm that explicitly.
func TestSubstrateList_RecordlessActorsNeverMatchedBySlug(t *testing.T) {
	actorA := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-aaaaaaaaaaaa", Name: "projA--dev", Uid: "uid-a"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	actorB := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-bbbbbbbbbbbb", Name: "projB--dev", Uid: "uid-b"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	orderings := map[string][]*ateapipb.Actor{
		"A then B": {actorA, actorB},
		"B then A": {actorB, actorA},
	}
	for name, order := range orderings {
		t.Run(name, func(t *testing.T) {
			rec := &callRecorder{}
			rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
			defer closeServer()

			fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
				return &ateapipb.ListActorsResponse{Actors: order}, nil
			}

			agents, err := rt.List(context.Background(), map[string]string{"scion.name": "dev"})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(agents) != 0 {
				t.Fatalf(`List(scion.name="dev") = %v, want no matches (neither record-less actor is resolvable by its bare agent slug)`, agents)
			}

			all, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(all) != 2 {
				t.Fatalf("List(scion.agent=true) = %v, want both actors present under their own actor names", all)
			}
			for _, a := range all {
				if a.Name == "dev" {
					t.Errorf("agent %+v has Name=\"dev\" — a record-less actor must never report a slug-shaped name", a)
				}
			}
		})
	}
}

// TestSubstrateList_RecordlessActorNotMatchedEvenWhenProjectScoped confirms
// the fail-closed rule holds even for a project-scoped lookup, not only an
// unscoped one: a record-less actor carries no project labels at all, so a
// "scion.project_id" filter can never match it either, regardless of
// whether the filter's project ID actually corresponds to the actor's own
// atespace. This is deliberately less capable than resolving the actor
// correctly when the scoping does match — see List's doc comment for why
// recovering the slug from the actor name cannot be made to fail closed
// against every project-scoped caller.
func TestSubstrateList_RecordlessActorNotMatchedEvenWhenProjectScoped(t *testing.T) {
	const projAID = "aaaaaaaaaaaa"
	atespaceA := substrateAtespaceName(projAID)

	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: atespaceA, Name: "projA--dev", Uid: "uid-a"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
			},
		}, nil
	}

	agents, err := rt.List(context.Background(), map[string]string{"scion.project_id": projAID})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("List(scion.project_id=%q) = %v, want no matches (a record-less actor carries no project label to match, even for its own project)", projAID, agents)
	}
}

// TestSubstrateList_RecordExistsAndRecordlessSameSlug confirms a
// record-having actor's real, independently verified slug is unaffected by
// an unrelated record-less actor in a different project whose actor name
// happens to end the same way. The record-less one is never a candidate
// for "scion.name"="dev" at all (see List's doc comment), so there is
// nothing for the record-having actor's slug to collide with; this pins
// that down for both possible ListActors return orders.
func TestSubstrateList_RecordExistsAndRecordlessSameSlug(t *testing.T) {
	const uid = "uid-with-record"
	actorWithRecord := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-aaaaaaaaaaaa", Name: "projA--dev", Uid: uid},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	actorWithoutRecord := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-bbbbbbbbbbbb", Name: "projB--dev", Uid: "uid-no-record"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	orderings := map[string][]*ateapipb.Actor{
		"record-having first": {actorWithRecord, actorWithoutRecord},
		"record-less first":   {actorWithoutRecord, actorWithRecord},
	}
	for name, order := range orderings {
		t.Run(name, func(t *testing.T) {
			rec := &callRecorder{}
			rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
			defer closeServer()

			fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
				return &ateapipb.ListActorsResponse{Actors: order}, nil
			}
			substrateAgentStateMu.Lock()
			substrateAgentRecords[uid] = &substrateAgentRecord{
				Labels: map[string]string{"scion.name": "dev", "scion.agent": "true"},
			}
			substrateAgentStateMu.Unlock()

			agents, err := rt.List(context.Background(), map[string]string{"scion.name": "dev"})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(agents) != 1 {
				t.Fatalf(`List(scion.name="dev") = %v, want exactly the record-having actor`, agents)
			}
			if agents[0].ContainerID != "scion-aaaaaaaaaaaa/projA--dev" {
				t.Errorf("ContainerID = %q, want the record-having projA actor, not the record-less projB one", agents[0].ContainerID)
			}
		})
	}
}

// TestSubstrateList_SameSlugDifferentProjects_UnscopedReturnsNothing pins
// down the fail-closed ambiguity guard List's doc comment describes: two
// record-having actors sharing the agent slug "dev" across different
// projects (project A and project B, each with its own project name,
// project ID, and project path). An unscoped-by-slug query
// ("scion.name"="dev", no project-scoping key) is exactly the shape
// AgentManager.Delete/Stop and LookupContainerID's own internal
// Runtime.List calls always use (pkg/agent/manager.go,
// pkg/runtimebroker/server.go) — regardless of whether their own caller
// resolved a project. With two matching record-having actors and no way to
// tell which one the caller means, List must return neither, never an
// arbitrary one — for both possible ListActors return orders.
func TestSubstrateList_SameSlugDifferentProjects_UnscopedReturnsNothing(t *testing.T) {
	const (
		uidA = "uid-projA-dev"
		uidB = "uid-projB-dev"
	)
	actorA := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-aaaaaaaaaaaa", Name: "projA--dev", Uid: uidA},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	actorB := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-bbbbbbbbbbbb", Name: "projB--dev", Uid: uidB},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	orderings := map[string][]*ateapipb.Actor{
		"projA first": {actorA, actorB},
		"projB first": {actorB, actorA},
	}
	for name, order := range orderings {
		t.Run(name, func(t *testing.T) {
			rec := &callRecorder{}
			rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
			defer closeServer()

			fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
				return &ateapipb.ListActorsResponse{Actors: order}, nil
			}

			substrateAgentStateMu.Lock()
			substrateAgentRecords[uidA] = &substrateAgentRecord{
				Labels:      map[string]string{"scion.name": "dev", "scion.agent": "true"},
				Project:     "projA",
				ProjectID:   "aaaaaaaaaaaa",
				ProjectPath: "/projects/projA",
			}
			substrateAgentRecords[uidB] = &substrateAgentRecord{
				Labels:      map[string]string{"scion.name": "dev", "scion.agent": "true"},
				Project:     "projB",
				ProjectID:   "bbbbbbbbbbbb",
				ProjectPath: "/projects/projB",
			}
			substrateAgentStateMu.Unlock()
			t.Cleanup(func() {
				substrateAgentStateMu.Lock()
				delete(substrateAgentRecords, uidA)
				delete(substrateAgentRecords, uidB)
				substrateAgentStateMu.Unlock()
			})

			agents, err := rt.List(context.Background(), map[string]string{"scion.name": "dev"})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(agents) != 0 {
				t.Fatalf(`List(scion.name="dev") = %v, want no matches (ambiguous: two record-having actors share this slug across different projects)`, agents)
			}

			// A project-scoped query for the same slug is unaffected by the
			// guard and must still resolve — and now carries ProjectPath.
			scoped, err := rt.List(context.Background(), map[string]string{
				"scion.name":               "dev",
				projectkeys.LabelProjectID: "bbbbbbbbbbbb",
			})
			if err != nil {
				t.Fatalf("List() scoped to projB error = %v", err)
			}
			if len(scoped) != 1 || scoped[0].ContainerID != "scion-bbbbbbbbbbbb/projB--dev" {
				t.Fatalf("List(scion.name=dev, scion.project_id=projB) = %v, want exactly projB's actor", scoped)
			}
			if scoped[0].ProjectPath != "/projects/projB" {
				t.Errorf("ProjectPath = %q, want %q", scoped[0].ProjectPath, "/projects/projB")
			}
		})
	}
}

// -----------------------------------------------------------------------
// NewSubstrateRuntime: process-wide memoization
// -----------------------------------------------------------------------

// resetSubstrateRuntimeRegistryForTest clears the process-wide
// substrateRuntimes registry for the duration of a test, restoring it
// afterward so this test can't leak state into (or pick up state left by)
// any other test.
func resetSubstrateRuntimeRegistryForTest(t *testing.T) {
	t.Helper()
	substrateRuntimesMu.Lock()
	old := substrateRuntimes
	substrateRuntimes = make(map[string]*SubstrateRuntime)
	substrateRuntimesMu.Unlock()
	t.Cleanup(func() {
		substrateRuntimesMu.Lock()
		substrateRuntimes = old
		substrateRuntimesMu.Unlock()
	})
}

func TestNewSubstrateRuntime_MemoizedAcrossCalls(t *testing.T) {
	resetSubstrateRuntimeRegistryForTest(t)

	rec := &callRecorder{}
	fc := newFakeControlClient(rec)
	fa := newFakeActorServer(rec)
	server := httptest.NewServer(fa.handler())
	defer server.Close()

	built := 0
	origBuilder := substrateRuntimeBuilder
	substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
		built++
		return NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(server.URL), nil, cfg), nil
	}
	t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })

	cfg := &config.V1SubstrateConfig{
		APIEndpoint:    "api.ate-system.svc:443",
		RouterEndpoint: server.URL,
	}

	// Simulates the broker's first `scion start`: GetRuntime resolves a
	// fresh substrate runtime and Run()s an agent on it.
	rt1, err := NewSubstrateRuntime(cfg)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime() error = %v", err)
	}
	id, err := rt1.Run(context.Background(), testSubstrateRunConfig())
	if err != nil {
		t.Fatalf("rt1.Run() error = %v", err)
	}

	// newFakeControlClient's default listActors doesn't track what
	// createActor "created" — it's a canned response, not an in-memory
	// store. Point it at the actor Run() just created so List can find it;
	// this only stands in for what a real ateapi ListActors would already
	// return.
	atespace, actorName, err := splitSubstrateID(id)
	if err != nil {
		t.Fatalf("splitSubstrateID(%q) error = %v", id, err)
	}
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{
					Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: actorName, Uid: fakeActorUID},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
				},
			},
		}, nil
	}

	// Simulates a second `scion start`: the broker resolves substrate again
	// (it's an auxiliary runtime, rebuilt on every start that isn't the
	// default profile).
	rt2, err := NewSubstrateRuntime(cfg)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime() (second call) error = %v", err)
	}

	if rt1 != rt2 {
		t.Fatal("NewSubstrateRuntime() returned different instances for the same config — state (control tokens, agent records, the gRPC conn) is not shared")
	}
	if built != 1 {
		t.Errorf("substrateRuntimeBuilder called %d times, want 1 (memoized)", built)
	}

	// The critical behavioural proof: an operation against the "new"
	// resolved instance (rt2, as the broker would use for e.g. `scion
	// message`/`scion delete` on the first agent) must still see the agent
	// rt1 created — this is exactly what broke before memoization.
	agents, err := rt2.List(context.Background(), map[string]string{"scion.name": "test-agent"})
	if err != nil {
		t.Fatalf("rt2.List() error = %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("rt2.List({scion.name: test-agent}) = %v, want to find the agent rt1 created", agents)
	}

	if _, err := rt2.Exec(context.Background(), id, []string{"true"}); err != nil {
		t.Errorf("rt2.Exec() on rt1's agent error = %v, want the control token cached by rt1 to still work", err)
	}
}

func TestNewSubstrateRuntime_DifferentConfigsGetDifferentInstances(t *testing.T) {
	resetSubstrateRuntimeRegistryForTest(t)

	built := 0
	origBuilder := substrateRuntimeBuilder
	substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
		built++
		return NewSubstrateRuntimeForTest(newFakeControlClient(&callRecorder{}), substrate.NewRouterClient("http://unused"), nil, cfg), nil
	}
	t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })

	cfgA := &config.V1SubstrateConfig{APIEndpoint: "a.example:443", RouterEndpoint: "http://a"}
	cfgB := &config.V1SubstrateConfig{APIEndpoint: "b.example:443", RouterEndpoint: "http://b"}

	rtA, err := NewSubstrateRuntime(cfgA)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime(cfgA) error = %v", err)
	}
	rtB, err := NewSubstrateRuntime(cfgB)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime(cfgB) error = %v", err)
	}
	if rtA == rtB {
		t.Error("NewSubstrateRuntime() returned the same instance for two different configs")
	}
	if built != 2 {
		t.Errorf("substrateRuntimeBuilder called %d times, want 2 (one per distinct config)", built)
	}
}

// TestSubstrateAgentState_SharedAcrossConfigChange confirms control tokens
// and agent records are shared process-wide, not per SubstrateRuntime
// instance, because the broker resolves a genuinely new instance whenever
// settings change (or a second substrate profile has different settings) —
// even though List's synthesised scion.name/scion.agent labels mean the
// agent stays visible, Exec (and any label lookup beyond those two) would
// break for every pre-existing agent the moment the config changed, without
// this.
func TestSubstrateAgentState_SharedAcrossConfigChange(t *testing.T) {
	resetSubstrateRuntimeRegistryForTest(t)
	resetSubstrateAgentStateForTest(t)

	rec := &callRecorder{}
	fc := newFakeControlClient(rec)
	fa := newFakeActorServer(rec)
	server := httptest.NewServer(fa.handler())
	defer server.Close()

	origBuilder := substrateRuntimeBuilder
	// Both configs' instances talk to the same underlying fake ateapi/
	// router, exactly as two SubstrateRuntime instances for the same real
	// cluster would in production — they differ only in
	// V1SubstrateConfig's Go value (egress_allow), which is what makes
	// NewSubstrateRuntime treat them as separate connection-registry
	// entries.
	substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
		return NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(server.URL), nil, cfg), nil
	}
	t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })

	cfgA := &config.V1SubstrateConfig{APIEndpoint: "api.example:443", RouterEndpoint: server.URL}
	cfgB := &config.V1SubstrateConfig{APIEndpoint: "api.example:443", RouterEndpoint: server.URL, EgressAllow: []string{"api.example.com"}}

	rtA, err := NewSubstrateRuntime(cfgA)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime(cfgA) error = %v", err)
	}

	runCfg := testSubstrateRunConfig()
	runCfg.Project = "proj-a"
	id, err := rtA.Run(context.Background(), runCfg)
	if err != nil {
		t.Fatalf("rtA.Run() error = %v", err)
	}

	atespace, actorName, err := splitSubstrateID(id)
	if err != nil {
		t.Fatalf("splitSubstrateID(%q) error = %v", id, err)
	}
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{
					Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: actorName, Uid: fakeActorUID},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
				},
			},
		}, nil
	}

	// Simulates the broker resolving substrate again after an
	// egress_allow edit (or a second profile with different settings) —
	// a genuinely different SubstrateRuntime instance.
	rtB, err := NewSubstrateRuntime(cfgB)
	if err != nil {
		t.Fatalf("NewSubstrateRuntime(cfgB) error = %v", err)
	}
	if rtA == rtB {
		t.Fatal("NewSubstrateRuntime(cfgB) returned the same instance as cfgA — this test requires distinct instances to prove state is shared, not per-instance")
	}

	if _, err := rtB.Exec(context.Background(), id, []string{"true"}); err != nil {
		t.Errorf("rtB.Exec() on rtA's agent error = %v, want the control token rtA cached to still work from rtB", err)
	}

	agents, err := rtB.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("rtB.List() error = %v", err)
	}
	if len(agents) != 1 || agents[0].Project != "proj-a" {
		t.Errorf("rtB.List() = %v, want the project label from rtA's record (\"proj-a\")", agents)
	}
}

// TestGetRuntime_Substrate_SettingsBased_Memoized:
// TestNewSubstrateRuntime_MemoizedAcrossCalls calls NewSubstrateRuntime
// directly, which wouldn't catch a future factory.go change that bypasses
// it (e.g. calling newSubstrateRuntimeFromConfig directly). This drives the
// same assertion through GetRuntime/
// config.LoadEffectiveSettings, the actual path pkg/runtimebroker exercises,
// with substrateRuntimeBuilder stubbed so it needs no real cluster/network.
func TestGetRuntime_Substrate_SettingsBased_Memoized(t *testing.T) {
	resetSubstrateRuntimeRegistryForTest(t)

	built := 0
	origBuilder := substrateRuntimeBuilder
	substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
		built++
		return NewSubstrateRuntimeForTest(newFakeControlClient(&callRecorder{}), substrate.NewRouterClient("http://unused"), nil, cfg), nil
	}
	t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })

	t.Setenv("PATH", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
		"schema_version": "1",
		"active_profile": "substrate",
		"runtimes": {
			"substrate-prod": {
				"type": "substrate",
				"substrate": {
					"api_endpoint": "api.ate-system.svc:443",
					"router_endpoint": "http://atenet-router.ate-system.svc:80"
				}
			}
		},
		"profiles": {
			"substrate": {
				"runtime": "substrate-prod"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.json"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	r1 := GetRuntime("", "")
	rt1, ok := r1.(*SubstrateRuntime)
	if !ok {
		t.Fatalf("GetRuntime() = %T, want *SubstrateRuntime", r1)
	}

	r2 := GetRuntime("", "")
	rt2, ok := r2.(*SubstrateRuntime)
	if !ok {
		t.Fatalf("GetRuntime() (second call) = %T, want *SubstrateRuntime", r2)
	}

	if rt1 != rt2 {
		t.Error("GetRuntime() returned different SubstrateRuntime instances for the same settings-resolved config — memoization is bypassed somewhere on the factory path")
	}
	if built != 1 {
		t.Errorf("substrateRuntimeBuilder called %d times via GetRuntime, want 1 (memoized)", built)
	}
}

// TestWaitForHealthz_ContextCancellationReturnsPromptly proves waitForHealthz
// stops polling as soon as the context is cancelled — before the call and
// mid-loop — instead of sleeping and retrying until the timeout deadline. The
// returned error must satisfy errors.Is(context.Canceled), and the injected
// wait (the production-cancellable sleepWithContext seam) must run at most
// once.
func TestWaitForHealthz_ContextCancellationReturnsPromptly(t *testing.T) {
	rec := &callRecorder{}
	fa := newFakeActorServer(rec)
	// Never reports the wanted state, so only cancellation (not success) can
	// end the loop.
	fa.healthzState = healthzRunning
	server := httptest.NewServer(fa.handler())
	defer server.Close()
	router := substrate.NewRouterClient(server.URL)

	t.Run("cancelled before the first attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sleeps := 0
		sleep := func(context.Context, time.Duration) error { sleeps++; return nil }

		err := waitForHealthz(ctx, router, "atespace", "actor", healthzAwaitingBootstrap, 2*time.Second, sleep)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if sleeps > 1 {
			t.Fatalf("sleep ran %d times, want at most once on an already-cancelled context", sleeps)
		}
	})

	t.Run("cancelled during the wait between attempts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sleeps := 0
		// The wait reports context.Canceled WITHOUT cancelling ctx itself, so
		// the only way waitForHealthz can return a cancellation error is the
		// backoff site's sleep-error return — not the top-of-loop ctx.Err()
		// check, which stays nil here. This pins that return specifically.
		sleep := func(context.Context, time.Duration) error {
			sleeps++
			return context.Canceled
		}

		err := waitForHealthz(ctx, router, "atespace", "actor", healthzAwaitingBootstrap, 2*time.Second, sleep)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if sleeps != 1 {
			t.Fatalf("sleep ran %d times, want exactly once (return immediately once the wait is cancelled)", sleeps)
		}
	})
}

// TestWaitForHealthz_SucceedsAfterRetries is the positive control: with a live
// context, waitForHealthz keeps polling across waits and returns nil once the
// server reports the wanted state, so the cancellation guard above has not
// made the normal retry path give up early.
func TestWaitForHealthz_SucceedsAfterRetries(t *testing.T) {
	rec := &callRecorder{}
	fa := newFakeActorServer(rec)
	fa.healthzState = healthzRunning // not yet the wanted state
	server := httptest.NewServer(fa.handler())
	defer server.Close()
	router := substrate.NewRouterClient(server.URL)

	sleeps := 0
	sleep := func(context.Context, time.Duration) error {
		sleeps++
		if sleeps == 2 {
			fa.mu.Lock()
			fa.healthzState = healthzAwaitingBootstrap
			fa.mu.Unlock()
		}
		return nil
	}

	err := waitForHealthz(context.Background(), router, "atespace", "actor", healthzAwaitingBootstrap, time.Minute, sleep)
	if err != nil {
		t.Fatalf("waitForHealthz returned %v, want nil once the state is reached after retries", err)
	}
	if sleeps < 2 {
		t.Fatalf("sleep ran %d times, want >=2 (success is reached only after retries)", sleeps)
	}
}

// TestSleepWithContext exercises the production sleep seam directly (the
// waitForHealthz tests above inject a fake sleep, so they never run this code).
// A cancelled context must abort a long wait promptly rather than sleep out the
// full duration, and a live context must sleep the (short) duration and return
// nil.
func TestSleepWithContext(t *testing.T) {
	t.Run("cancelled context returns promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled before the call

		start := time.Now()
		err := sleepWithContext(ctx, time.Hour)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleepWithContext returned %v, want errors.Is(context.Canceled)", err)
		}
		if elapsed > time.Second {
			t.Fatalf("sleepWithContext took %s, want it to abort within 1s rather than wait the full hour", elapsed)
		}
	})

	t.Run("cancelled mid-wait returns promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err := sleepWithContext(ctx, time.Hour)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleepWithContext returned %v, want errors.Is(context.Canceled)", err)
		}
		if elapsed > time.Second {
			t.Fatalf("sleepWithContext took %s, want it to abort within 1s of cancellation", elapsed)
		}
	})

	t.Run("live context sleeps the duration and returns nil", func(t *testing.T) {
		const d = 5 * time.Millisecond
		start := time.Now()
		err := sleepWithContext(context.Background(), d)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("sleepWithContext returned %v, want nil for a live context", err)
		}
		if elapsed < d {
			t.Fatalf("sleepWithContext returned after %s, want it to wait at least the requested %s", elapsed, d)
		}
	})
}

// TestSubstrateRun_RejectsEmptyPerAgent pins the Run-time guard behind
// SupportsEmptyPerAgentWorkspace=false (design #2703): substrate never
// mounts RunConfig.Workspace, so an empty-per-agent start that reaches the
// broker before its first heartbeat corrects the static capabilities is
// refused before any control-plane call. Other modes still run.
func TestSubstrateRun_RejectsEmptyPerAgent(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	cfg := testSubstrateRunConfig()
	cfg.Env = append(append([]string(nil), cfg.Env...), "SCION_WORKSPACE_MODE=empty-per-agent")
	if _, err := rt.Run(context.Background(), cfg); !errors.Is(err, errEmptyPerAgentSubstrate) {
		t.Fatalf("Run(empty-per-agent) error = %v, want errEmptyPerAgentSubstrate", err)
	}
	if calls := rec.list(); len(calls) != 0 {
		t.Fatalf("Run(empty-per-agent) made control-plane calls %v, want none", calls)
	}

	cfg = testSubstrateRunConfig()
	cfg.Env = append(append([]string(nil), cfg.Env...), "SCION_WORKSPACE_MODE=shared-plain")
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run(shared-plain) error = %v, want nil", err)
	}
}
