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

//go:build !no_sqlite

package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

func preflightCandidate(list func(context.Context, map[string]string) ([]api.AgentInfo, error)) brokerhost.Candidate {
	return brokerhost.Candidate{
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
		Identity: &brokeridentity.Identity{InstanceKey: "docker-a", RuntimeBrokerID: "rb-a"},
		Runtime:  &runtime.MockRuntime{ListFunc: list},
	}
}

// TestFlatOwnershipPreflight: unlabeled agent objects on the scope, or a
// scope that cannot be read, refuse activation; objects labelled for this or
// another instance do not.
func TestFlatOwnershipPreflight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	labelled := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{Name: "mine", ContainerID: "cid-m", Labels: map[string]string{api.LabelRuntimeBrokerID: "rb-a",
				"scion.project_id": "proj-1", "agent_id": "agent-m", "scion.name": "mine", api.LabelRunID: "run-m"}},
			{Name: "theirs", Labels: map[string]string{api.LabelRuntimeBrokerID: "rb-b"}},
		}, nil
	}
	require.NoError(t, flatOwnershipPreflight(ctx, preflightCandidate(labelled)))

	unlabeled := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{Name: "old-agent", Labels: map[string]string{"scion.project_id": "proj-1"}},
		}, nil
	}
	err := flatOwnershipPreflight(ctx, preflightCandidate(unlabeled))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proj-1/old-agent")

	failing := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, errors.New("permission denied")
	}
	err = flatOwnershipPreflight(ctx, preflightCandidate(failing))
	require.Error(t, err, "a scope that cannot be read refuses (no inference of absence)")
	assert.Contains(t, err.Error(), "cannot read the execution scope")
}

// TestFlatOwnershipPreflight_RefusesBeforeActivation: through the host, an
// unlabeled object refuses the instance in pass 1, with the reason code, and
// the Activator is never called.
func TestFlatOwnershipPreflight_RefusesBeforeActivation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	act := &recordingFlatActivator{}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: t.TempDir(),
		Instances: []config.V1RuntimeBrokerInstanceConfig{{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}},
		Mode:      brokerhost.ModeRemote,
		NewRuntime: func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{Name: "legacy-agent"}}, nil
			}}, nil
		},
		ProbeScope: fakeScopeProber("daemon-1"),
		Activator:  act,
		BuildServer: func(brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			t.Fatal("a refused instance is never built")
			return nil, nil
		},
		OwnershipPreflight: flatOwnershipPreflight,
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	st := h.Status()[0]
	assert.Equal(t, brokerhost.StateRefused, st.State)
	assert.Equal(t, "ownership_unresolved", st.Reason)
	assert.Contains(t, st.Error, "legacy-agent")
	assert.Empty(t, act.activated, "no Hub activation for an instance with unresolved ownership")
}

// TestFlatOwnershipPreflight_ReconstructsOwnObjects: this instance's
// labelled objects with complete metadata re-create their records; an own
// object with incomplete labels is unresolved.
func TestFlatOwnershipPreflight_ReconstructsOwnObjects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	complete := api.AgentInfo{Name: "worker", ContainerID: "cid-1", Labels: map[string]string{
		api.LabelRuntimeBrokerID: "rb-a", "scion.project_id": "proj-1", "agent_id": "agent-1", "scion.name": "worker", api.LabelRunID: "run-1"}}
	require.NoError(t, flatOwnershipPreflight(ctx, preflightCandidate(func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{complete}, nil
	})))
	dir, err := runtimebroker.DefaultStateDir("rb-a")
	require.NoError(t, err)
	rec, ok, err := runtimebroker.NewOwnershipStore(dir, "rb-a").Get("proj-1", "agent-1")
	require.NoError(t, err)
	require.True(t, ok, "record reconstructed from complete labels")
	assert.True(t, rec.OwnsUID("cid-1"))

	incomplete := api.AgentInfo{Name: "half", ContainerID: "cid-2", Labels: map[string]string{api.LabelRuntimeBrokerID: "rb-a"}}
	err = flatOwnershipPreflight(ctx, preflightCandidate(func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{incomplete}, nil
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "half")
}

// TestFlatOwnership_MultiInstanceConflictingClaimsBothRefuse: two
// configured instances whose records claim the same agent (and slug) both
// receive that key as conflicting, through the production preflight, key
// reader and server configuration; each refuses only that key. A labelled
// object of one instance whose key the other's record claims conflicts the
// same way (the preflight reconstructs the labelled object's record first).
func TestFlatOwnership_MultiInstanceConflictingClaimsBothRefuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir := t.TempDir()
	insts := []config.V1RuntimeBrokerInstanceConfig{
		{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
		{Key: "docker-b", Name: "b", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}},
	}
	scope := func(key string) brokeridentity.ExecutionScope {
		return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-" + key}}
	}
	ids := map[string]*brokeridentity.Identity{}
	for _, in := range insts {
		id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, in.Key), in.Key, "docker", scope(in.Key), nil)
		require.NoError(t, err)
		ids[in.Key] = id
	}
	store := func(key string) *runtimebroker.OwnershipStore {
		dir, err := runtimebroker.DefaultStateDir(ids[key].RuntimeBrokerID)
		require.NoError(t, err)
		return runtimebroker.NewOwnershipStore(dir, ids[key].RuntimeBrokerID)
	}
	// docker-a records the shared agent and one of its own; docker-b's
	// runtime has a labelled object for the shared agent.
	require.NoError(t, store("docker-a").BeginRun("proj-1", "agent-shared", "shared", "run-a"))
	require.NoError(t, store("docker-a").BeginRun("proj-1", "agent-a", "a-only", "run-a2"))
	objects := map[string][]api.AgentInfo{
		"docker-b": {{Name: "shared", ContainerID: "cid-b", Labels: map[string]string{api.LabelRuntimeBrokerID: ids["docker-b"].RuntimeBrokerID,
			"scion.project_id": "proj-1", "agent_id": "agent-shared", "scion.name": "shared", api.LabelRunID: "run-b"}}},
	}

	sh := flatServerShared{cfg: &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{Host: "127.0.0.1", Port: 9800}},
		mode: brokerhost.ModeRemote}
	conflicting := map[string]map[string]bool{}
	act := &recordingFlatActivator{}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: insts,
		Mode:      brokerhost.ModeRemote,
		NewRuntime: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return objects[in.Key], nil
			}}, nil
		},
		ProbeScope: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return scope(in.Key), nil
		},
		Activator: act,
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			c := flatInstanceServerConfig(sh, ic)
			conflicting[ic.Instance.Key] = c.FlatInstance.ConflictingOwnershipKeys
			return nil, errors.New("not serving in this test")
		},
		OwnershipPreflight: flatOwnershipPreflight,
		OwnershipKeys:      flatOwnershipKeys,
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	want := map[string]bool{
		runtimebroker.OwnershipAgentKey("proj-1", "agent-shared"): true,
		runtimebroker.OwnershipSlugKey("proj-1", "shared"):        true,
	}
	assert.Equal(t, want, conflicting["docker-a"], "docker-a refuses the shared key")
	assert.Equal(t, want, conflicting["docker-b"], "docker-b refuses the shared key")

	for _, key := range []string{"docker-a", "docker-b"} {
		s := store(key)
		s.SetConflicting(conflicting[key])
		_, _, err := s.Get("proj-1", "agent-shared")
		assert.ErrorIs(t, err, runtimebroker.ErrOwnershipConflict, key)
	}
	a := store("docker-a")
	a.SetConflicting(conflicting["docker-a"])
	_, ok, err := a.Get("proj-1", "agent-a")
	require.NoError(t, err)
	assert.True(t, ok, "docker-a's unrelated agent is unaffected")
}

// TestFlatOwnershipPreflight_DeniedVersusAbsent: a scope that cannot be
// read refuses and leaves the records untouched; a complete read marks a
// recorded main object it does not show as absent, keeps a listed one and
// every child object recorded, and finishes an older empty provisioning
// run (never the latest run).
func TestFlatOwnershipPreflight_DeniedVersusAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	dir, err := runtimebroker.DefaultStateDir("rb-a")
	require.NoError(t, err)
	st := runtimebroker.NewOwnershipStore(dir, "rb-a")
	require.NoError(t, st.BeginRun("proj-1", "agent-gone", "gone", "run-1"))
	require.NoError(t, st.AddResource("proj-1", "agent-gone", "run-1", api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "gone", UID: "cid-gone-0123456789"}))
	require.NoError(t, st.AddResource("proj-1", "agent-gone", "run-1", api.ResourceHandle{Kind: api.ResourceKindSecret, Name: "scion-auth-gone", UID: "uid-secret"}))
	require.NoError(t, st.BeginRun("proj-1", "agent-live", "live", "run-old")) // crashed start, nothing created
	require.NoError(t, st.BeginRun("proj-1", "agent-live", "live", "run-2"))
	require.NoError(t, st.AddResource("proj-1", "agent-live", "run-2", api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "live", UID: "cid-live-0123456789"}))
	require.NoError(t, st.SetRunState("proj-1", "agent-live", "run-2", runtimebroker.OwnershipStateCreated))
	require.NoError(t, st.BeginRun("proj-1", "agent-new", "fresh", "run-only")) // created, not started

	denied := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, errors.New("permission denied")
	}
	require.Error(t, flatOwnershipPreflight(ctx, preflightCandidate(denied)))
	rec, _, err := st.Get("proj-1", "agent-gone")
	require.NoError(t, err)
	assert.True(t, rec.OwnsUID("cid-gone-0123456789"), "a denied read never marks an object absent")

	live := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{{Name: "live", ID: "cid-live-0123", ContainerID: "cid-live-0123", Labels: map[string]string{
			api.LabelRuntimeBrokerID: "rb-a", "scion.project_id": "proj-1", "agent_id": "agent-live", "scion.name": "live", api.LabelRunID: "run-2"}}}, nil
	}
	liveCand := preflightCandidate(live)
	// The exact check confirms the unlisted object is gone.
	liveCand.Runtime.(*runtime.MockRuntime).ResourceAbsentFunc = func(_ context.Context, h api.ResourceHandle) (bool, error) {
		return h.UID == "cid-gone-0123456789", nil
	}
	require.NoError(t, flatOwnershipPreflight(ctx, liveCand))
	rec, _, err = st.Get("proj-1", "agent-gone")
	require.NoError(t, err)
	assert.False(t, rec.OwnsUID("cid-gone-0123456789"), "a complete read that does not show the object marks it absent")
	assert.True(t, rec.OwnsUID("uid-secret"), "child objects are not in the listing and stay recorded")
	rec, _, err = st.Get("proj-1", "agent-live")
	require.NoError(t, err)
	assert.True(t, rec.OwnsUID("cid-live-0123456789"), "a listed object (short ID) stays recorded")
	assert.Equal(t, runtimebroker.OwnershipStateDeleted, rec.Run("run-old").State, "an older empty provisioning run is finished")
	rec, _, err = st.Get("proj-1", "agent-new")
	require.NoError(t, err)
	assert.Equal(t, runtimebroker.OwnershipStateProvisioning, rec.Run("run-only").State, "the latest run is kept")
}

// TestFlatOwnershipPreflight_UnfinishedDeleteDoesNotRefuse: after a delete
// that moved the record (or only the run) to deleting failed and the
// object stayed, a restart's preflight accepts the object as known and
// pending deletion, without appending to the record; an object of a run
// already deleted is still unresolved.
func TestFlatOwnershipPreflight_UnfinishedDeleteDoesNotRefuse(t *testing.T) {
	ctx := context.Background()
	object := func(agentID, runID string) api.AgentInfo {
		return api.AgentInfo{Name: "worker", ContainerID: "cid-" + agentID, Labels: map[string]string{api.LabelRuntimeBrokerID: "rb-a",
			"scion.project_id": "proj-1", "agent_id": agentID, "scion.name": "worker-" + agentID, api.LabelRunID: runID}}
	}
	for name, tc := range map[string]struct {
		set     func(st *runtimebroker.OwnershipStore)
		refused bool
	}{
		"record deleting": {func(st *runtimebroker.OwnershipStore) {
			require.NoError(t, st.SetRecordState("proj-1", "agent-1", runtimebroker.OwnershipStateDeleting))
		}, false},
		"run deleting": {func(st *runtimebroker.OwnershipStore) {
			require.NoError(t, st.SetRunState("proj-1", "agent-1", "run-1", runtimebroker.OwnershipStateDeleting))
		}, false},
		"run deleted": {func(st *runtimebroker.OwnershipStore) {
			require.NoError(t, st.SetRunState("proj-1", "agent-1", "run-1", runtimebroker.OwnershipStateDeleted))
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir, err := runtimebroker.DefaultStateDir("rb-a")
			require.NoError(t, err)
			st := runtimebroker.NewOwnershipStore(dir, "rb-a")
			require.NoError(t, st.BeginRun("proj-1", "agent-1", "worker-agent-1", "run-1"))
			tc.set(st)
			before, _, _ := st.Get("proj-1", "agent-1")
			err = flatOwnershipPreflight(ctx, preflightCandidate(func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{object("agent-1", "run-1")}, nil
			}))
			if tc.refused {
				require.Error(t, err)
				return
			}
			require.NoError(t, err, "an unfinished delete must not refuse the instance")
			after, _, _ := st.Get("proj-1", "agent-1")
			assert.Equal(t, len(before.Runs[0].Resources), len(after.Runs[0].Resources), "nothing appended to a record being deleted")
		})
	}
}

// childScopeRuntime is a scope with main objects (List), owned child
// objects (ListOwnedResources) and an exact absence check.
type childScopeRuntime struct {
	*runtime.MockRuntime
	children    []runtime.OwnedResource
	childrenErr error
	absent      func(api.ResourceHandle) (bool, error)
}

func (r *childScopeRuntime) ListOwnedResources(context.Context, string) ([]runtime.OwnedResource, error) {
	return r.children, r.childrenErr
}

func (r *childScopeRuntime) ResourceAbsent(_ context.Context, h api.ResourceHandle) (bool, error) {
	return r.absent(h)
}

func childLabels(agentID string) map[string]string {
	l := map[string]string{api.LabelRuntimeBrokerID: "rb-a", "scion.project_id": "proj-1", "scion.name": "worker", api.LabelRunID: "run-1"}
	if agentID != "" {
		l[api.LabelAgentID] = agentID
	}
	return l
}

// TestFlatOwnershipPreflight_ChildRecovery: the start-up preflight
// recovers a fully identified child object of this instance on its own (a
// crash after the child was created but before its handle was recorded,
// with no main object), refuses an incomplete (historical) one and a child
// listing failure, and keeps recorded children whose absence cannot be
// established (a read failure, or a child still terminating) while
// marking a confirmed-gone one absent.
func TestFlatOwnershipPreflight_ChildRecovery(t *testing.T) {
	ctx := context.Background()
	secret := func(name, uid string) api.ResourceHandle {
		return api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "agents", Name: name, UID: uid}
	}
	candidate := func(rt runtime.Runtime) brokerhost.Candidate {
		return brokerhost.Candidate{
			Instance: config.V1RuntimeBrokerInstanceConfig{Key: "k8s-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "kubernetes"}},
			Identity: &brokeridentity.Identity{InstanceKey: "k8s-a", RuntimeBrokerID: "rb-a"},
			Runtime:  rt,
		}
	}
	noObjects := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil }}
	store := func(t *testing.T) *runtimebroker.OwnershipStore {
		dir, err := runtimebroker.DefaultStateDir("rb-a")
		require.NoError(t, err)
		return runtimebroker.NewOwnershipStore(dir, "rb-a")
	}

	t.Run("child-only crash window", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		st := store(t)
		require.NoError(t, st.BeginRun("proj-1", "agent-1", "worker", "run-1")) // recorded before any create
		rt := &childScopeRuntime{MockRuntime: noObjects,
			children: []runtime.OwnedResource{{Handle: secret("scion-auth-worker", "uid-child"), Labels: childLabels("agent-1")}},
			absent:   func(api.ResourceHandle) (bool, error) { return false, nil }}
		require.NoError(t, flatOwnershipPreflight(ctx, candidate(rt)))
		rec, ok, err := st.Get("proj-1", "agent-1")
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, rec.OwnsUID("uid-child"), "the orphaned child is recovered into its run")
	})

	t.Run("child with no record at all", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		rt := &childScopeRuntime{MockRuntime: noObjects,
			children: []runtime.OwnedResource{{Handle: secret("scion-auth-worker", "uid-child"), Labels: childLabels("agent-1")}},
			absent:   func(api.ResourceHandle) (bool, error) { return false, nil }}
		require.NoError(t, flatOwnershipPreflight(ctx, candidate(rt)))
		rec, ok, err := store(t).Get("proj-1", "agent-1")
		require.NoError(t, err)
		require.True(t, ok, "the record is rebuilt from the child's complete labels")
		assert.True(t, rec.OwnsUID("uid-child"))
	})

	t.Run("historical child without agent ID", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		rt := &childScopeRuntime{MockRuntime: noObjects,
			children: []runtime.OwnedResource{{Handle: secret("scion-auth-old", "uid-old"), Labels: childLabels("")}},
			absent:   func(api.ResourceHandle) (bool, error) { return false, nil }}
		err := flatOwnershipPreflight(ctx, candidate(rt))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scion-auth-old")
	})

	t.Run("child listing failure", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		rt := &childScopeRuntime{MockRuntime: noObjects, childrenErr: errors.New("forbidden"),
			absent: func(api.ResourceHandle) (bool, error) { return false, nil }}
		err := flatOwnershipPreflight(ctx, candidate(rt))
		require.Error(t, err, "a per-kind read failure refuses; it is never read as absence")
	})

	t.Run("recorded children: read failure, terminating, gone", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		st := store(t)
		require.NoError(t, st.BeginRun("proj-1", "agent-1", "worker", "run-1"))
		for _, uid := range []string{"uid-unreadable", "uid-terminating", "uid-gone"} {
			require.NoError(t, st.AddResource("proj-1", "agent-1", "run-1", secret("s-"+uid, uid)))
		}
		rt := &childScopeRuntime{MockRuntime: noObjects, absent: func(h api.ResourceHandle) (bool, error) {
			switch h.UID {
			case "uid-unreadable":
				return false, errors.New("timeout")
			case "uid-terminating":
				return false, nil
			}
			return true, nil
		}}
		require.NoError(t, flatOwnershipPreflight(ctx, candidate(rt)))
		rec, _, err := st.Get("proj-1", "agent-1")
		require.NoError(t, err)
		assert.True(t, rec.OwnsUID("uid-unreadable"), "a read failure keeps the child recorded")
		assert.True(t, rec.OwnsUID("uid-terminating"), "a terminating child stays recorded")
		assert.False(t, rec.OwnsUID("uid-gone"), "a confirmed-gone child is marked absent")
	})
}
