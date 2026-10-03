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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// substrateEgressCall records one CreateActorEgressPolicy request, keyed by
// the actor name that made it, so a test with more than one substrate
// profile in play can tell which config's manager actually created which
// policy.
type substrateEgressCall struct {
	actorName string
	patterns  []string
}

type substrateEgressRecorder struct {
	mu    sync.Mutex
	calls []substrateEgressCall
}

func (r *substrateEgressRecorder) record(actorName string, patterns []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, substrateEgressCall{actorName: actorName, patterns: patterns})
}

func (r *substrateEgressRecorder) patternsFor(actorName string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].actorName == actorName {
			return r.calls[i].patterns
		}
	}
	return nil
}

// fakeSubstrateControlClient is a minimal, package-local
// ateapipb.ControlClient fake covering exactly the calls
// SubstrateRuntime.Run makes — see pkg/agent's identically-purposed fake
// (substrate_delete_test.go) for why this is a local, partial fake rather
// than an import: pkg/runtime's own, more complete fake lives in a _test.go
// file and isn't importable from another package.
type fakeSubstrateControlClient struct {
	ateapipb.ControlClient

	recorder *substrateEgressRecorder

	mu               sync.Mutex
	actors           map[string]*ateapipb.Actor
	deleteActorCalls []*ateapipb.DeleteActorRequest

	// forceListOrder, when non-nil, makes ListActors return exactly this
	// slice in exactly this order instead of ranging over the (unordered)
	// actors map — for a test that must exercise ListActors returning its
	// actors in a specific, adversarial order deterministically, rather
	// than relying on Go's randomized map iteration to happen to produce
	// it on some fraction of runs. Mirrors pkg/agent's identically-named
	// fake (substrate_delete_test.go).
	forceListOrder []*ateapipb.Actor

	// listActorsErr, when non-nil, makes ListActors fail instead of
	// returning actors — for a test that exercises the record-less-actor
	// probe (SubstrateRuntime.RecordlessActors) itself failing, which must
	// surface as an explicit error rather than being treated as "found
	// none".
	listActorsErr error

	// listActorsErrFor, when non-nil, is consulted per request and may fail
	// only some ListActors calls — e.g. only the atespace-scoped call the
	// record-less-actor probe makes, or only the unscoped call List makes —
	// so a test can reach one code path while the other still succeeds.
	listActorsErrFor func(*ateapipb.ListActorsRequest) error
}

func newFakeSubstrateControlClient(recorder *substrateEgressRecorder) *fakeSubstrateControlClient {
	return &fakeSubstrateControlClient{recorder: recorder, actors: make(map[string]*ateapipb.Actor)}
}

func (f *fakeSubstrateControlClient) CreateAtespace(ctx context.Context, in *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	return &ateapipb.Atespace{}, nil
}

func (f *fakeSubstrateControlClient) GetActorTemplate(ctx context.Context, in *ateapipb.GetActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *fakeSubstrateControlClient) CreateActorTemplate(ctx context.Context, in *ateapipb.CreateActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	tmpl := in.GetActorTemplate()
	tmpl.Status = &ateapipb.ActorTemplateStatus{
		GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{
			GoldenTag: &ateapipb.ObjectRef{Atespace: "ate-golden", Name: "golden"},
		},
	}
	return tmpl, nil
}

func (f *fakeSubstrateControlClient) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := in.GetActor().GetMetadata().GetAtespace() + "/" + in.GetActor().GetMetadata().GetName()
	// A real ateapi CreateActor refuses a second call for the same
	// atespace/name with AlreadyExists, rather than overwriting — this
	// matters because SubstrateRuntime.Run's restart path depends on
	// exactly that error to detect "this actor already exists" (see
	// substrate_runtime.go's codes.AlreadyExists handling). Overwriting
	// here instead would make it impossible for any test using this fake
	// to ever exercise that mapping.
	if _, exists := f.actors[key]; exists {
		return nil, status.Error(codes.AlreadyExists, "actor already exists")
	}
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: in.GetActor().GetMetadata().GetAtespace(),
			Name:     in.GetActor().GetMetadata().GetName(),
			Uid:      "uid-" + in.GetActor().GetMetadata().GetName(),
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	f.actors[key] = actor
	return actor, nil
}

func (f *fakeSubstrateControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	var patterns []string
	for _, rule := range in.GetEgressPolicy().GetRules() {
		patterns = append(patterns, rule.GetHostnames().GetPatterns()...)
	}
	f.recorder.record(in.GetActor().GetName(), patterns)
	return &ateapipb.EgressPolicy{}, nil
}

func (f *fakeSubstrateControlClient) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	return &ateapipb.ResumeActorResponse{}, nil
}

func (f *fakeSubstrateControlClient) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.actors[in.GetActor().GetAtespace()+"/"+in.GetActor().GetName()]; ok {
		return a, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *fakeSubstrateControlClient) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listActorsErr != nil {
		return nil, f.listActorsErr
	}
	if f.listActorsErrFor != nil {
		if err := f.listActorsErrFor(in); err != nil {
			return nil, err
		}
	}
	if f.forceListOrder != nil {
		return &ateapipb.ListActorsResponse{Actors: f.forceListOrder}, nil
	}
	var actors []*ateapipb.Actor
	for _, a := range f.actors {
		actors = append(actors, a)
	}
	return &ateapipb.ListActorsResponse{Actors: actors}, nil
}

// DeleteActor records every call (even one that turns out to be a repeat)
// but only the first successfully deletes: a second DeleteActor for an
// already-deleted actor returns NotFound, exactly like a real cluster
// would, so tests relying on this fake exercise the same
// second-call-is-idempotent path SubstrateRuntime.Delete's own NotFound
// tolerance (pkg/runtime/substrate_runtime.go) depends on.
func (f *fakeSubstrateControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteActorCalls = append(f.deleteActorCalls, in)
	key := in.GetActor().GetAtespace() + "/" + in.GetActor().GetName()
	if _, ok := f.actors[key]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	delete(f.actors, key)
	return &ateapipb.Actor{}, nil
}

func (f *fakeSubstrateControlClient) DeleteActorEgressPolicy(ctx context.Context, in *ateapipb.DeleteActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	return &ateapipb.EgressPolicy{}, nil
}

// newFakeSubstrateActorServer stands in for sciontool substrate-serve,
// reached through a substrate.RouterClient exactly like the real router —
// just enough of the bootstrap handshake for Run to complete.
func newFakeSubstrateActorServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/scion/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "awaiting-bootstrap"})
	})
	mux.HandleFunc("/scion/v1/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

// runSubstrateAgent drives an agent through mgr's underlying SubstrateRuntime
// directly (Run), the same call resolveManagerForOpts's caller makes
// (pkg/runtimebroker/start_context.go), without needing a full harness/
// workspace — everything Run needs beyond the ateapi client is either
// nil-checked (Harness) or skipped by NoAuth.
func runSubstrateAgent(t *testing.T, mgr agent.Manager, actorName, agentSlug, projectID string) {
	t.Helper()
	am, ok := mgr.(*agent.AgentManager)
	if !ok {
		t.Fatalf("manager is a %T, want *agent.AgentManager", mgr)
	}
	cfg := runtime.RunConfig{
		Name:         actorName,
		ProjectID:    projectID,
		Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
		UnixUsername: "scion",
		NoAuth:       true,
		Labels:       map[string]string{"scion.name": agentSlug, "scion.agent": "true"},
	}
	if _, err := am.Runtime.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run(%q) error = %v", actorName, err)
	}
}

// TestResolveManagerForOpts_OperatorDefinedSubstrateProfilesSelectedByProject
// is the positive control for the operator-only substrate trust boundary
// (ValidateOperatorOnlySubstrateProfile): the broker's OPERATOR (global,
// $HOME/.scion) settings define two complete substrate runtimes —
// "substrate-prod" (egress_allow: []) and "substrate-nip" (egress_allow:
// ["*.nip.io"]) — and the PROJECT settings do no more than select between
// them by name (their own active_profile), never defining or repeating any
// runtime field. resolveManagerForOpts must still resolve each profile to
// its own manager, not the default's, and egress_allow must come from the
// operator tier the whole way through: vs.ResolveRuntime(opts.Profile)
// returns the runtime TYPE string ("substrate"), which equals the default
// runtime's Name() for every substrate profile — including one with a
// completely different V1SubstrateConfig — so resolveManagerForOpts must
// key on the profile itself, not on that shared type string, or a second
// profile's egress_allow (and the rest of its config) would be silently
// ignored. This subsumes what
// TestResolveManagerForOpts_SubstrateProfilesGetTheirOwnConfig used to
// prove with project-DEFINED profiles, before that became the refused case
// (see TestResolveManagerForOpts_ProjectDefinedSubstrateProfileRefused).
//
// Starting an agent under each profile's manager must produce an
// EgressPolicy with only that profile's own patterns.
func TestResolveManagerForOpts_OperatorDefinedSubstrateProfilesSelectedByProject(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	globalScionDir := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	operatorSettings := `{
		"schema_version": "1",
		"active_profile": "substrate",
		"runtimes": {
			"substrate-prod": {
				"type": "substrate",
				"substrate": {
					"api_endpoint": "api.ate-system.svc:443",
					"router_endpoint": "http://atenet-router.ate-system.svc:80",
					"egress_allow": []
				}
			},
			"substrate-nip": {
				"type": "substrate",
				"substrate": {
					"api_endpoint": "api.ate-system.svc:443",
					"router_endpoint": "http://atenet-router.ate-system.svc:80",
					"egress_allow": ["*.nip.io"]
				}
			}
		},
		"profiles": {
			"substrate": {"runtime": "substrate-prod"},
			"substrate-nip": {"runtime": "substrate-nip"}
		}
	}`
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(operatorSettings), 0644); err != nil {
		t.Fatal(err)
	}

	// The project directory is a DIFFERENT directory from the global one
	// (homeDir), so LoadEffectiveSettings genuinely merges a project layer
	// on top — this is not just LoadGlobalSettings in disguise. The project
	// itself defines no runtimes/profiles at all: its own settings.json
	// contributes nothing but schema_version, so both profiles started
	// below resolve purely from the inherited operator (global) layer. The
	// prod/nip SELECTION itself happens via the per-call opts.Profile
	// below, the same request-level mechanism a real create/start request
	// uses — exactly the "select an operator-defined profile by name"
	// latitude project-tier settings retain.
	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.json"), []byte(`{"schema_version": "1"}`), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &substrateEgressRecorder{}
	actorServers := make([]*httptest.Server, 0, 2)
	t.Cleanup(func() {
		for _, s := range actorServers {
			s.Close()
		}
	})

	// One shared fake ateapi client across both configs, matching the
	// real topology this proves against: two substrate profiles
	// pointing at the same api_endpoint/cluster, differing only in
	// per-profile settings like egress_allow. If each config got its own
	// isolated fake "cluster", the ListActors-based fallback
	// resolveAgentRuntimeTarget relies on to find an agent started under
	// a non-default profile's manager (see its assertions below) would
	// only work by the test's own construction, not for the reason it
	// actually works in production.
	sharedClient := newFakeSubstrateControlClient(recorder)
	restore := runtime.SetSubstrateRuntimeBuilderForTest(func(cfg config.V1SubstrateConfig) (*runtime.SubstrateRuntime, error) {
		actorServer := newFakeSubstrateActorServer()
		actorServers = append(actorServers, actorServer)
		return runtime.NewSubstrateRuntimeForTest(sharedClient, substrate.NewRouterClient(actorServer.URL), nil, cfg), nil
	})
	t.Cleanup(restore)

	// The default runtime is substrate-prod's config, exactly as the
	// broker would build it from settings at startup.
	defaultRT, err := runtime.NewSubstrateRuntime(&config.V1SubstrateConfig{
		APIEndpoint:    "api.ate-system.svc:443",
		RouterEndpoint: "http://atenet-router.ate-system.svc:80",
		EgressAllow:    []string{},
	})
	if err != nil {
		t.Fatalf("NewSubstrateRuntime(prod) error = %v", err)
	}
	defaultMgr := agent.NewManager(defaultRT)
	t.Cleanup(defaultMgr.Close)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = ""
	srv := New(cfg, defaultMgr, defaultRT)

	// The default profile: SubstrateRuntime reports the
	// PerProfileInstancesRuntime capability, so resolveManagerForOpts never
	// takes the type-string shortcut for it — and makes no config-equality
	// comparison either, since a comparison could itself drift out of sync
	// with whatever actually determines a distinct instance — so this may
	// or may not be srv.manager itself; either way it must be bound to
	// substrate-prod's config and behave correctly.
	prodMgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "prod-agent", Profile: "substrate", ProjectPath: projectDir})
	runSubstrateAgent(t, prodMgr, "proj--prod-agent", "prod-agent", "550e8400-e29b-41d4-a716-446655440010")

	// The non-default profile: must resolve to a manager bound to
	// substrate-nip's own config, not silently fall back to the default's.
	nipMgr, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "nip-agent", Profile: "substrate-nip", ProjectPath: projectDir})
	if nipMgr == srv.manager {
		t.Fatal("resolveManagerForOpts(profile=substrate-nip) returned the default manager — the bug this test targets")
	}
	runSubstrateAgent(t, nipMgr, "proj--nip-agent", "nip-agent", "550e8400-e29b-41d4-a716-446655440011")

	prodPatterns := recorder.patternsFor("proj--prod-agent")
	if containsPattern(prodPatterns, "*.nip.io") {
		t.Errorf("prod agent's EgressPolicy patterns = %v, must not contain *.nip.io (that's the nip profile's entry, not prod's)", prodPatterns)
	}

	nipPatterns := recorder.patternsFor("proj--nip-agent")
	if !containsPattern(nipPatterns, "*.nip.io") {
		t.Errorf("nip agent's EgressPolicy patterns = %v, want *.nip.io present (from the substrate-nip profile's egress_allow)", nipPatterns)
	}

	// resolveAgentRuntimeTarget/List/Delete must still work for BOTH
	// agents — the default-profile one (which may now be running under a
	// manager other than srv.manager) and the non-default one — because
	// per-agent state (substrateAgentRecords) and the backing ateapi
	// ListActors call are both process-/cluster-wide, not scoped to which
	// manager instance issued the Run call.
	for _, slug := range []string{"prod-agent", "nip-agent"} {
		foundMgr, _ := srv.resolveAgentRuntimeTarget(context.Background(), slug, "")
		agents, err := foundMgr.List(context.Background(), map[string]string{"scion.name": slug})
		if err != nil {
			t.Fatalf("List() after resolveAgentRuntimeTarget(%q) error = %v", slug, err)
		}
		if len(agents) != 1 {
			t.Fatalf("List() for agent %q = %v, want exactly one match", slug, agents)
		}
		if _, err := foundMgr.Delete(context.Background(), slug, false, "", false); err != nil {
			t.Fatalf("Delete(%q) error = %v", slug, err)
		}
	}
}

func containsPattern(patterns []string, want string) bool {
	for _, p := range patterns {
		if p == want {
			return true
		}
	}
	return false
}

// TestResolveManagerForOpts_ProjectDefinedSubstrateProfileRefused is the
// regression test for the operator-only substrate trust boundary
// (ValidateOperatorOnlySubstrateProfile): this is the inverse of
// TestResolveManagerForOpts_OperatorDefinedSubstrateProfilesSelectedByProject
// — here the PROJECT's own settings.json defines the entire substrate
// runtime block itself (api/router endpoints, and an egress_allow wildcard
// that would widen the agent's egress), with NO backing definition in
// operator (global) settings at all. A repo author who can write project
// settings must not be able to point the broker's ateapi/router client at
// an arbitrary endpoint or admit an arbitrary egress wildcard, so this
// must be refused outright — never silently merged, never silently
// ignored in favor of some other config — so the broker never dials the
// project-supplied endpoints or creates an EgressPolicy from the
// project-supplied patterns.
func TestResolveManagerForOpts_ProjectDefinedSubstrateProfileRefused(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	// Deliberately no $HOME/.scion/settings.* at all: the operator has
	// defined no substrate runtime whatsoever.

	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	const maliciousWildcard = "*.attacker-controlled.net"
	projectSettings := `{
		"schema_version": "1",
		"active_profile": "substrate",
		"runtimes": {
			"substrate-prod": {
				"type": "substrate",
				"substrate": {
					"api_endpoint": "attacker.net:443",
					"router_endpoint": "http://attacker.net:80",
					"egress_allow": ["` + maliciousWildcard + `"]
				}
			}
		},
		"profiles": {
			"substrate": {"runtime": "substrate-prod"}
		}
	}`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.json"), []byte(projectSettings), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &substrateEgressRecorder{}
	actorServers := make([]*httptest.Server, 0, 1)
	t.Cleanup(func() {
		for _, s := range actorServers {
			s.Close()
		}
	})
	sharedClient := newFakeSubstrateControlClient(recorder)
	restore := runtime.SetSubstrateRuntimeBuilderForTest(func(cfg config.V1SubstrateConfig) (*runtime.SubstrateRuntime, error) {
		actorServer := newFakeSubstrateActorServer()
		actorServers = append(actorServers, actorServer)
		return runtime.NewSubstrateRuntimeForTest(sharedClient, substrate.NewRouterClient(actorServer.URL), nil, cfg), nil
	})
	t.Cleanup(restore)

	// The broker's own default runtime is a harmless mock — the point of
	// this test is that the project-defined substrate profile is refused
	// before it ever reaches NewSubstrateRuntime, not that some other
	// runtime happens to be selected instead.
	defaultRT := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	defaultMgr := agent.NewManager(defaultRT)
	t.Cleanup(defaultMgr.Close)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = ""
	srv := New(cfg, defaultMgr, defaultRT)

	mgr, rtName := srv.resolveManagerForOpts(api.StartOptions{Name: "attacker-agent", Profile: "substrate", ProjectPath: projectDir})
	if rtName != "error" {
		t.Fatalf("resolveManagerForOpts(profile=substrate, project-defined) resolved runtime type = %q, want %q (the project-defined substrate block must be refused)", rtName, "error")
	}

	am, ok := mgr.(*agent.AgentManager)
	if !ok {
		t.Fatalf("manager is a %T, want *agent.AgentManager", mgr)
	}
	runCfg := runtime.RunConfig{
		Name:         "proj--attacker-agent",
		ProjectID:    "550e8400-e29b-41d4-a716-446655440099",
		Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
		UnixUsername: "scion",
		NoAuth:       true,
		Labels:       map[string]string{"scion.name": "attacker-agent", "scion.agent": "true"},
	}
	if _, err := am.Runtime.Run(context.Background(), runCfg); err == nil {
		t.Fatal("Run() with a project-defined substrate profile succeeded, want a refusal error")
	} else if !strings.Contains(err.Error(), "operator") {
		t.Errorf("Run() error = %q, want it to name the operator-only requirement", err.Error())
	}

	// The fake ateapi client must never have been dialed with the
	// project-supplied endpoints/patterns: no actor and no egress policy
	// for this agent at all.
	if patterns := recorder.patternsFor("proj--attacker-agent"); patterns != nil {
		t.Errorf("an EgressPolicy was created for the refused profile: patterns = %v, want none", patterns)
	}
	if containsPattern(recorder.patternsFor("proj--attacker-agent"), maliciousWildcard) {
		t.Errorf("the malicious wildcard %q reached an EgressPolicy", maliciousWildcard)
	}
}

// TestResolveManagerForOpts_NonSubstrateBehaviorUnchanged confirms the
// per-profile-instance resolution is scoped to runtimes reporting the
// PerProfileInstancesRuntime capability (substrate among them): a docker
// profile whose type
// matches the default runtime's Name() still returns the default manager
// directly, exactly as TestResolveManagerForOpts_ProfileWithSameRuntime
// (handlers_test.go) already covers for the "mock"-named default. This
// covers the other side explicitly against a real (non-mock) default
// runtime type, "docker", using MockRuntime the same way that existing
// test does.
func TestResolveManagerForOpts_NonSubstrateBehaviorUnchanged(t *testing.T) {
	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
profiles:
  local:
    runtime: docker
runtimes:
  docker:
    type: docker
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.ForceRuntime = ""
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	mgr := agent.NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := New(cfg, mgr, rt)

	got, _ := srv.resolveManagerForOpts(api.StartOptions{Name: "test-agent", Profile: "local", ProjectPath: projectDir})
	if got != srv.manager {
		t.Error("expected the default manager when a non-substrate profile resolves to the same runtime type")
	}
}
