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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// fakeSubstrateControlClient is a minimal, package-local
// ateapipb.ControlClient fake covering exactly the calls SubstrateRuntime's
// Run/List/Delete make. It embeds the interface so any call this test never
// exercises panics loudly (nil pointer dereference) rather than silently
// doing nothing, instead of duplicating pkg/runtime's own more complete
// fake — that one lives in a _test.go file, so it isn't importable from
// here, and this test only needs a small slice of the interface.
type fakeSubstrateControlClient struct {
	ateapipb.ControlClient

	mu                sync.Mutex
	actors            map[string]*ateapipb.Actor // key: "<atespace>/<name>"
	deleteActorCalls  []*ateapipb.DeleteActorRequest
	deleteEgressCalls []*ateapipb.DeleteActorEgressPolicyRequest

	// forceListOrder, when non-nil, makes ListActors return exactly this
	// slice in exactly this order instead of ranging over the (unordered)
	// actors map — for a test that must exercise ListActors returning its
	// actors in a specific, adversarial order deterministically, rather
	// than relying on Go's randomized map iteration to happen to produce
	// it on some fraction of runs.
	forceListOrder []*ateapipb.Actor
}

func newFakeSubstrateControlClient() *fakeSubstrateControlClient {
	return &fakeSubstrateControlClient{actors: make(map[string]*ateapipb.Actor)}
}

// putActor directly registers an actor as if some earlier CreateActor call
// had created it, without going through Run — used to simulate an actor
// this SubstrateRuntime instance never itself started (e.g. a broker
// restart), so no in-memory agent record exists for it.
func (f *fakeSubstrateControlClient) putActor(atespace, name, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors[atespace+"/"+name] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: uid},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
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
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: in.GetActor().GetMetadata().GetAtespace(),
			Name:     in.GetActor().GetMetadata().GetName(),
			Uid:      "uid-" + in.GetActor().GetMetadata().GetName(),
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	f.actors[actor.GetMetadata().GetAtespace()+"/"+actor.GetMetadata().GetName()] = actor
	return actor, nil
}

func (f *fakeSubstrateControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
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
	if f.forceListOrder != nil {
		return &ateapipb.ListActorsResponse{Actors: f.forceListOrder}, nil
	}
	var actors []*ateapipb.Actor
	for _, a := range f.actors {
		actors = append(actors, a)
	}
	return &ateapipb.ListActorsResponse{Actors: actors}, nil
}

func (f *fakeSubstrateControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteActorCalls = append(f.deleteActorCalls, in)
	delete(f.actors, in.GetActor().GetAtespace()+"/"+in.GetActor().GetName())
	return &ateapipb.Actor{}, nil
}

func (f *fakeSubstrateControlClient) DeleteActorEgressPolicy(ctx context.Context, in *ateapipb.DeleteActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteEgressCalls = append(f.deleteEgressCalls, in)
	return &ateapipb.EgressPolicy{}, nil
}

// newFakeSubstrateActorServer stands in for sciontool substrate-serve,
// reached through a substrate.RouterClient exactly like the real router —
// just enough of the bootstrap handshake (healthz + bootstrap) for Run to
// complete, for the test case that needs a real in-memory agent record
// (which only Run itself creates).
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

// TestSubstrateAgentManagerDelete_RecordExists proves `scion delete` removes
// the Substrate actor: AgentManager.Delete (pkg/agent/manager.go) resolves
// the caller's agent slug to a container via Runtime.List, matching on
// AgentInfo.Name. SubstrateRuntime.List must report the bare agent slug as
// Name, not the project-prefixed containerName(project, agent): reporting
// the prefixed form would make the match never succeed and Delete silently
// return success without calling Runtime.Stop/Runtime.Delete at all.
// Hardening AgentManager.Delete's silent-no-op shape in general is future
// work; this drives the real path (a real *SubstrateRuntime and a real
// agent.Manager, not a reimplementation of either).
//
// This case starts the agent for real (through Run), so an in-memory agent
// record exists — the common case, with the record carrying the correct
// "scion.name" label (see substrate_runtime.go's List doc comment for why
// Name, not the label, is what List reports the bare agent slug as).
func TestSubstrateAgentManagerDelete_RecordExists(t *testing.T) {
	fc := newFakeSubstrateControlClient()
	actorServer := newFakeSubstrateActorServer()
	defer actorServer.Close()

	rt := scionruntime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(actorServer.URL), nil, config.V1SubstrateConfig{
		SnapshotStorage:   "gs://bucket/prefix/",
		SandboxConfigName: "gvisor-default",
	})

	const agentSlug = "sb-smoke-2"
	cfg := scionruntime.RunConfig{
		Name:         "myproj--" + agentSlug, // containerName("myproj", agentSlug)
		ProjectID:    "550e8400-e29b-41d4-a716-446655440002",
		Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
		UnixUsername: "scion",
		NoAuth:       true, // skip needing a real api.Harness for buildSubstrateStartCmd
		Labels:       map[string]string{"scion.name": agentSlug, "scion.agent": "true"},
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	mgr := NewManager(rt)
	defer mgr.Close()

	if _, err := mgr.Delete(context.Background(), agentSlug, false, "", false); err != nil {
		t.Fatalf("Delete(%q) error = %v", agentSlug, err)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) == 0 {
		t.Fatal("DeleteActor was never called — Delete() silently no-opped instead of finding the actor")
	}
	got := fc.deleteActorCalls[len(fc.deleteActorCalls)-1].GetActor()
	if got.GetName() != cfg.Name {
		t.Errorf("DeleteActor actor name = %q, want %q", got.GetName(), cfg.Name)
	}
	if !strings.HasPrefix(got.GetAtespace(), "scion-") {
		t.Errorf("DeleteActor atespace = %q, want a \"scion-\"-prefixed atespace", got.GetAtespace())
	}
	if len(fc.deleteEgressCalls) == 0 {
		t.Fatal("DeleteActorEgressPolicy was never called — Delete() silently no-opped instead of finding the actor")
	}
	gotEgress := fc.deleteEgressCalls[len(fc.deleteEgressCalls)-1].GetActor()
	if gotEgress.GetName() != cfg.Name || gotEgress.GetAtespace() != got.GetAtespace() {
		t.Errorf("DeleteActorEgressPolicy actor = %s/%s, want %s/%s", gotEgress.GetAtespace(), gotEgress.GetName(), got.GetAtespace(), cfg.Name)
	}
}

// TestSubstrateAgentManagerDelete_NoRecord is the documented, deliberate
// no-op case: an actor with no in-memory agent record at all, simulating a
// broker restart (substrate-runtime.md §10 explicitly accepts losing
// records across a restart). The actor here is injected directly into the
// fake client, never through this SubstrateRuntime's own Run, so there is
// genuinely no substrateAgentRecords entry for it.
//
// A record-less actor is never resolvable by its bare agent slug — see
// pkg/runtime/substrate_runtime.go's List doc comment for why — so Delete
// by slug always no-ops for it: zero DeleteActor/DeleteActorEgressPolicy
// calls, and the actor itself is left running.
func TestSubstrateAgentManagerDelete_NoRecord(t *testing.T) {
	fc := newFakeSubstrateControlClient()
	const (
		atespace  = "scion-someproj"
		agentSlug = "sb-smoke-3"
		actorName = "myproj--" + agentSlug
	)
	fc.putActor(atespace, actorName, "uid-orphan")

	rt := scionruntime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient("http://unused"), nil, config.V1SubstrateConfig{})

	mgr := NewManager(rt)
	defer mgr.Close()

	if _, err := mgr.Delete(context.Background(), agentSlug, false, "", false); err != nil {
		t.Fatalf("Delete(%q) error = %v", agentSlug, err)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.deleteActorCalls) != 0 {
		t.Errorf("DeleteActor called %d times, want 0 (a record-less actor is never resolvable by slug)", len(fc.deleteActorCalls))
	}
	if len(fc.deleteEgressCalls) != 0 {
		t.Errorf("DeleteActorEgressPolicy called %d times, want 0", len(fc.deleteEgressCalls))
	}
	if _, ok := fc.actors[atespace+"/"+actorName]; !ok {
		t.Error("the record-less actor was removed — it must be untouched (documented no-op)")
	}
}

// TestSubstrateAgentManagerDelete_RecordlessAmbiguousSlugDeletesNothing is
// the invariant a record-less List entry must never violate: a slug lookup
// must never resolve to an actor whose ownership can't be verified. Two
// different projects' actors, both record-less, both named "<project>--dev"
// — an unscoped Delete("dev") must find neither, not pick one arbitrarily.
// A naive implementation keyed only on the last-seen match would have
// whichever actor ListActors happened to return second end up as List's
// sole "scion.name"="dev" entry (map iteration order is undefined), so
// Delete would delete THAT ONE — a different project's actor — instead of
// doing nothing. Both actor orderings are exercised as subtests;
// running the whole test under a high -count is an additional check that
// the outcome truly doesn't depend on map iteration order.
func TestSubstrateAgentManagerDelete_RecordlessAmbiguousSlugDeletesNothing(t *testing.T) {
	const (
		atespaceA = "scion-aaaaaaaaaaaa"
		actorA    = "projA--dev"
		atespaceB = "scion-bbbbbbbbbbbb"
		actorB    = "projB--dev"
	)

	for _, order := range []string{"A then B", "B then A"} {
		t.Run(order, func(t *testing.T) {
			fc := newFakeSubstrateControlClient()
			if order == "A then B" {
				fc.putActor(atespaceA, actorA, "uid-a")
				fc.putActor(atespaceB, actorB, "uid-b")
			} else {
				fc.putActor(atespaceB, actorB, "uid-b")
				fc.putActor(atespaceA, actorA, "uid-a")
			}

			rt := scionruntime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient("http://unused"), nil, config.V1SubstrateConfig{})
			mgr := NewManager(rt)
			defer mgr.Close()

			if _, err := mgr.Delete(context.Background(), "dev", false, "", false); err != nil {
				t.Fatalf(`Delete("dev") error = %v`, err)
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()
			if len(fc.deleteActorCalls) != 0 {
				t.Fatalf(`Delete("dev") called DeleteActor %v, want no calls at all (ambiguous slug, neither actor's ownership is verified)`, fc.deleteActorCalls)
			}
			if len(fc.deleteEgressCalls) != 0 {
				t.Fatalf(`Delete("dev") called DeleteActorEgressPolicy %v, want no calls at all`, fc.deleteEgressCalls)
			}
			// Both actors must still be present — genuinely untouched, not
			// merely "not the target of a recorded call".
			if _, ok := fc.actors[atespaceA+"/"+actorA]; !ok {
				t.Error("projA's actor was removed from the fake's own actor store")
			}
			if _, ok := fc.actors[atespaceB+"/"+actorB]; !ok {
				t.Error("projB's actor was removed from the fake's own actor store")
			}
		})
	}
}

// TestSubstrateAgentManagerDelete_RecordExistsAndRecordlessSameSlug
// confirms a record-EXISTS actor's real slug resolves correctly even when
// an unrelated, different-project, record-less actor's actor name happens
// to end the same way. Delete("dev") must remove only the record-having
// actor; the record-less other-project actor — never a candidate for
// "scion.name"="dev" at all, since a record-less actor always reports its
// full actor name — must be untouched. Run for both ListActors return
// orders, forced deterministically rather than left to Go's randomized map
// iteration, since this is exactly the kind of bug that only shows up for
// one order.
func TestSubstrateAgentManagerDelete_RecordExistsAndRecordlessSameSlug(t *testing.T) {
	const (
		otherAtespace = "scion-bbbbbbbbbbbb"
		otherActor    = "projB--dev"
	)

	for _, orderName := range []string{"record-having first", "record-less first"} {
		t.Run(orderName, func(t *testing.T) {
			fc := newFakeSubstrateControlClient()
			actorServer := newFakeSubstrateActorServer()
			defer actorServer.Close()

			rt := scionruntime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(actorServer.URL), nil, config.V1SubstrateConfig{})

			// The record-less other-project actor, injected directly (never
			// through this runtime's Run, so it genuinely has no record).
			fc.putActor(otherAtespace, otherActor, "uid-other-project")

			// The record-having actor, started for real.
			cfg := scionruntime.RunConfig{
				Name:         "projA--dev",
				ProjectID:    "550e8400-e29b-41d4-a716-446655440020",
				Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
				UnixUsername: "scion",
				NoAuth:       true,
				Labels:       map[string]string{"scion.name": "dev", "scion.agent": "true"},
			}
			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			fc.mu.Lock()
			var recordHaving, recordLess *ateapipb.Actor
			for key, a := range fc.actors {
				if key == otherAtespace+"/"+otherActor {
					recordLess = a
				} else {
					recordHaving = a
				}
			}
			if orderName == "record-having first" {
				fc.forceListOrder = []*ateapipb.Actor{recordHaving, recordLess}
			} else {
				fc.forceListOrder = []*ateapipb.Actor{recordLess, recordHaving}
			}
			fc.mu.Unlock()

			mgr := NewManager(rt)
			defer mgr.Close()

			if _, err := mgr.Delete(context.Background(), "dev", false, "", false); err != nil {
				t.Fatalf(`Delete("dev") error = %v`, err)
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()
			// AgentManager.Delete calls Runtime.Stop then Runtime.Delete, and
			// for substrate Stop is Delete (same underlying call) — so 2
			// DeleteActor calls for one mgr.Delete() is expected. What
			// matters is that EVERY one of them names the record-having
			// actor, never the record-less other-project one.
			if len(fc.deleteActorCalls) == 0 {
				t.Fatal(`Delete("dev") called DeleteActor 0 times, want at least 1`)
			}
			for _, call := range fc.deleteActorCalls {
				got := call.GetActor()
				if got.GetName() != cfg.Name {
					t.Errorf("DeleteActor actor = %s/%s, want the record-having %q, not the record-less other-project actor", got.GetAtespace(), got.GetName(), cfg.Name)
				}
			}
			if _, ok := fc.actors[otherAtespace+"/"+otherActor]; !ok {
				t.Error("the record-less other-project actor was removed — it must be untouched")
			}
		})
	}
}

// TestSubstrateAgentManagerDelete_SameSlugDifferentProjectsFailsClosed is
// the record-HAVING counterpart to
// TestSubstrateAgentManagerDelete_RecordlessAmbiguousSlugDeletesNothing:
// two agents, both started for real (through Run, so each has a full
// in-memory record with the realistic Project/ProjectID/ProjectPath a
// hub-dispatched start actually produces — see
// pkg/runtimebroker's runSubstrateAgentForProject for the same
// convention), sharing the agent slug "dev" in two different projects.
//
// AgentManager.Delete's own internal Runtime.List call
// (map[string]string{"scion.name": slug}) never carries a project-scoping
// key — see manager.go — so this is exactly the shape the ambiguity guard
// in SubstrateRuntime.List targets. Even though AgentInfo.ProjectPath is
// populated correctly, an unscoped Delete("dev") still cannot tell the two
// apart at this call site: it must make zero DeleteActor calls and leave
// both actors running, for both possible ListActors return orders — a
// no-op, not a wrong-actor delete.
func TestSubstrateAgentManagerDelete_SameSlugDifferentProjectsFailsClosed(t *testing.T) {
	for _, orderName := range []string{"projA first", "projB first"} {
		t.Run(orderName, func(t *testing.T) {
			fc := newFakeSubstrateControlClient()
			actorServer := newFakeSubstrateActorServer()
			defer actorServer.Close()

			rt := scionruntime.NewSubstrateRuntimeForTest(fc, substrate.NewRouterClient(actorServer.URL), nil, config.V1SubstrateConfig{})

			runProjectAgent := func(projectName, projectID, projectPath string) *ateapipb.Actor {
				labels := map[string]string{"scion.name": "dev", "scion.agent": "true"}
				for k, v := range projectkeys.ProjectNameLabels(projectName) {
					labels[k] = v
				}
				for k, v := range projectkeys.ProjectIDLabels(projectID) {
					labels[k] = v
				}
				cfg := scionruntime.RunConfig{
					Name:         projectName + "--dev",
					Project:      projectName,
					ProjectID:    projectID,
					Image:        "us-docker.pkg.dev/proj/repo/scion-agent@sha256:" + strings.Repeat("a", 64),
					UnixUsername: "scion",
					NoAuth:       true,
					Labels:       labels,
					Annotations:  projectkeys.ProjectPathLabels(projectPath),
				}
				if _, err := rt.Run(context.Background(), cfg); err != nil {
					t.Fatalf("Run(%q) error = %v", cfg.Name, err)
				}
				fc.mu.Lock()
				defer fc.mu.Unlock()
				return fc.actors[scionruntime.SubstrateAtespaceNameForTest(projectID)+"/"+cfg.Name]
			}

			actorA := runProjectAgent("projA", "aaaaaaaaaaaa", "/projects/projA")
			actorB := runProjectAgent("projB", "bbbbbbbbbbbb", "/projects/projB")
			if actorA == nil || actorB == nil {
				t.Fatalf("test setup: actorA=%v actorB=%v, want both non-nil", actorA, actorB)
			}

			fc.mu.Lock()
			if orderName == "projA first" {
				fc.forceListOrder = []*ateapipb.Actor{actorA, actorB}
			} else {
				fc.forceListOrder = []*ateapipb.Actor{actorB, actorA}
			}
			fc.mu.Unlock()

			mgr := NewManager(rt)
			defer mgr.Close()

			// Unscoped: exercises AgentManager.Delete's own internal
			// Runtime.List call directly, with an empty projectPath.
			// deleteAgent resolves the target itself (project ID included
			// in the List filter) and calls AgentManager.DeleteTarget,
			// which performs no re-list, so this instead covers Delete's
			// own slug-resolution fallback for other, non-broker-resolved
			// callers.
			if _, err := mgr.Delete(context.Background(), "dev", false, "", false); err != nil {
				t.Fatalf(`Delete("dev") error = %v`, err)
			}

			fc.mu.Lock()
			defer fc.mu.Unlock()
			if len(fc.deleteActorCalls) != 0 {
				t.Fatalf(`Delete("dev") called DeleteActor %v, want zero calls (ambiguous slug across two projects — must fail closed even with ProjectPath set)`, fc.deleteActorCalls)
			}
			if _, ok := fc.actors[scionruntime.SubstrateAtespaceNameForTest("aaaaaaaaaaaa")+"/projA--dev"]; !ok {
				t.Error("projA's actor was removed — it must be untouched")
			}
			if _, ok := fc.actors[scionruntime.SubstrateAtespaceNameForTest("bbbbbbbbbbbb")+"/projB--dev"]; !ok {
				t.Error("projB's actor was removed — it must be untouched")
			}
		})
	}
}
