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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// ptone/scion#3274: one host-owned NFS mounter for every
// instance of a host.

func hostNFSConfig(autoMount bool, shares ...config.V1NFSShare) *config.V1NFSConfig {
	return &config.V1NFSConfig{MountRoot: "/mnt/nfs", AutoMount: autoMount, Shares: shares}
}

var (
	shareWS1 = config.V1NFSShare{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"}
	shareWS2 = config.V1NFSShare{ID: "ws2", Server: "10.0.0.3", Export: "/scion-homes"}
)

// newHostNFSServer builds an instance on runtime rtName that uses the
// host mounter.
func newHostNFSServer(t *testing.T, m *HostNFSMounter, brokerID, rtName string, nfs *config.V1NFSConfig, mc MountChecker) *Server {
	t.Helper()
	cfg := ServerConfig{Host: "127.0.0.1", Port: 0, BrokerID: brokerID, StateDir: t.TempDir(),
		NFSConfig: nfs, NFSMountChecker: mc, NFSHostMounter: m}
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return rtName }})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func TestHostNFSMounter_UnionOfHostBindRequirements(t *testing.T) {
	m := NewHostNFSMounter(newSyncMountChecker(), nil)
	r1, err := m.Register("docker-a", "docker", hostNFSConfig(false, shareWS1))
	require.NoError(t, err)
	r2, err := m.Register("docker-b", "podman", hostNFSConfig(true, shareWS1, shareWS2))
	require.NoError(t, err)
	require.NotNil(t, r1)
	assert.Same(t, r1, r2, "one reconciler for every host-bind instance")
	assert.Equal(t, []string{"ws1", "ws2"}, m.ShareIDs())
	assert.True(t, r1.MountsShares(), "auto_mount of any host-bind instance mounts the union")

	r, err := m.Register("k8s-c", "kubernetes", hostNFSConfig(true, config.V1NFSShare{ID: "ws3", Server: "10.0.0.9", Export: "/x"}))
	require.NoError(t, err)
	assert.Nil(t, r, "a Kubernetes instance registers nothing")
	assert.Equal(t, []string{"ws1", "ws2"}, m.ShareIDs(), "Kubernetes requirements never reach the host mounter")
}

// unscopedManager is a manager that cannot be restricted to one
// instance's objects (no SetOwner).
type unscopedManager struct{ agent.Manager }

// TestHostNFSMounter_InstanceThatCannotServeDoesNotRegister: a flat
// instance whose setup already failed when it is built (here: an agent
// manager that cannot be restricted to the instance's objects) adds nothing
// to the union and builds no reconciler of its own.
func TestHostNFSMounter_InstanceThatCannotServeDoesNotRegister(t *testing.T) {
	m := NewHostNFSMounter(newSyncMountChecker(), nil)
	cfg := ServerConfig{Host: "127.0.0.1", Port: 0, BrokerID: "rb-x", StateDir: t.TempDir(),
		NFSConfig: hostNFSConfig(true, shareWS1), NFSMountChecker: newSyncMountChecker(), NFSHostMounter: m,
		FlatInstance: &FlatInstanceConfig{Identity: &brokeridentity.Identity{RuntimeBrokerID: "rb-x"},
			Instance: config.V1RuntimeBrokerInstanceConfig{Key: "docker-x"}}}
	srv := New(cfg, unscopedManager{&mockManager{}}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	require.Error(t, srv.ownershipSetupErr)
	assert.Empty(t, m.ShareIDs(), "an instance that cannot serve registered its shares")
	assert.Nil(t, srv.nfsMountReconciler)

	ok := newHostNFSServer(t, m, "rb-y", "docker", hostNFSConfig(true, shareWS2), newSyncMountChecker())
	assert.Equal(t, []string{"ws2"}, m.ShareIDs())
	assert.NotNil(t, ok.nfsMountReconciler)
}

func TestHostNFSMounter_IncompatibleRequirementsRefused(t *testing.T) {
	for name, req := range map[string]*config.V1NFSConfig{
		"one share id, another source": hostNFSConfig(true, config.V1NFSShare{ID: "ws1", Server: "10.0.0.99", Export: "/scion-workspaces"}),
		"another mount root":           {MountRoot: "/srv/nfs", Shares: []config.V1NFSShare{shareWS1}},
		"other mount options":          {MountRoot: "/mnt/nfs", MountOptions: "vers=4", Shares: []config.V1NFSShare{shareWS1}},
	} {
		t.Run(name, func(t *testing.T) {
			m := NewHostNFSMounter(newSyncMountChecker(), nil)
			_, err := m.Register("docker-a", "docker", hostNFSConfig(true, shareWS1, shareWS2))
			require.NoError(t, err)
			_, err = m.Register("docker-b", "docker", req)
			require.ErrorIs(t, err, ErrNFSRequirementIncompatible)
			assert.Equal(t, []string{"ws1", "ws2"}, m.ShareIDs(), "a refused requirement changes nothing")
		})
	}
	// Through the server: the instance with the conflicting requirement
	// does not serve.
	m := NewHostNFSMounter(newSyncMountChecker(), nil)
	newHostNFSServer(t, m, "rb-a", "docker", hostNFSConfig(true, shareWS1), newSyncMountChecker())
	b := newHostNFSServer(t, m, "rb-b", "docker", hostNFSConfig(true, config.V1NFSShare{ID: "ws1", Server: "10.9.9.9", Export: "/other"}), newSyncMountChecker())
	err := b.StartServices(context.Background())
	require.ErrorIs(t, err, ErrNFSRequirementIncompatible)
}

// TestHostNFSMounter_SingleOwnerMountsOnce: two host-bind instances share
// the mounter's reconciler and never run a loop of their own; the host's
// loop mounts each share once, and concurrent dispatch checks from both
// instances go through the one owner (never two mounts at once).
func TestHostNFSMounter_SingleOwnerMountsOnce(t *testing.T) {
	mc := newSyncMountChecker()
	m := NewHostNFSMounter(mc, nil)
	a := newHostNFSServer(t, m, "rb-a", "docker", hostNFSConfig(true, shareWS1), mc)
	b := newHostNFSServer(t, m, "rb-b", "docker", hostNFSConfig(true, shareWS1, shareWS2), mc)
	require.Same(t, a.nfsMountReconciler, b.nfsMountReconciler)

	require.NoError(t, a.StartServices(context.Background()))
	require.NoError(t, b.StartServices(context.Background()))
	for _, s := range []*Server{a, b} {
		s.mu.RLock()
		loop := s.nfsReconcileCancel
		s.mu.RUnlock()
		assert.Nil(t, loop, "instances never run a mount loop of their own")
	}
	mounts, _, _ := mc.counts()
	assert.Zero(t, mounts)

	require.NoError(t, m.Start(context.Background()))
	waitClosed(t, m.FirstPassDone(), "host mounter first pass")
	waitClosed(t, a.nfsStartupReconcileDone, "instance A sees the host's first pass")
	mounts, _, _ = mc.counts()
	assert.Equal(t, 2, mounts, "each share of the union mounted once by the host")
	assert.Equal(t, "healthy", a.GetHealthInfo(context.Background()).Checks["nfs_mounts"])

	// Drop ws1 and dispatch from both instances at once.
	mc.mu.Lock()
	delete(mc.mountpoints, "/mnt/nfs/ws1")
	mc.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range []*Server{a, b} {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			assert.NoError(t, s.nfsMountReconciler.EnsureShareMounted(context.Background(), "ws1"))
		}(s)
	}
	wg.Wait()
	mounts, _, _ = mc.counts()
	assert.Equal(t, 3, mounts, "the dropped share is remounted once, by the one owner")
	mc.mu.Lock()
	assert.Equal(t, 1, mc.maxInFlight, "never two mounts at once")
	mc.mu.Unlock()
	m.Stop(context.Background())
}

// TestHostNFSMounter_KubernetesOnlyNeverMounts: a host with only
// Kubernetes instances has nothing for the host to mount, and each
// instance's own check never mounts, even with auto_mount on.
func TestHostNFSMounter_KubernetesOnlyNeverMounts(t *testing.T) {
	mc := newSyncMountChecker()
	m := NewHostNFSMounter(mc, nil)
	k := newHostNFSServer(t, m, "rb-k", "kubernetes", hostNFSConfig(true, shareWS1), mc)
	require.NotNil(t, k.nfsMountReconciler)
	assert.False(t, k.nfsHostOwned)
	assert.False(t, k.nfsMountReconciler.MountsShares(), "a Kubernetes instance only verifies")
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, k.StartServices(context.Background()))
	waitClosed(t, k.nfsStartupReconcileDone, "instance check")
	mounts, mkdirs, unmounts := mc.counts()
	assert.Zero(t, mounts+mkdirs+unmounts, "nothing is mounted for a Kubernetes-only host")
	m.Stop(context.Background())
}

// TestHostNFSMounter_NeverUnmountsAtShutdown: stopping an instance, and
// then the host mounter, unmounts nothing (another instance or a running
// agent may still use the mount).
func TestHostNFSMounter_NeverUnmountsAtShutdown(t *testing.T) {
	mc := newSyncMountChecker()
	m := NewHostNFSMounter(mc, nil)
	a := newHostNFSServer(t, m, "rb-a", "docker", hostNFSConfig(true, shareWS1), mc)
	newHostNFSServer(t, m, "rb-b", "docker", hostNFSConfig(true, shareWS1), mc)
	require.NoError(t, m.Start(context.Background()))
	waitClosed(t, m.FirstPassDone(), "first pass")
	require.NoError(t, a.Shutdown(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Stop(ctx)
	_, _, unmounts := mc.counts()
	assert.Zero(t, unmounts)
	table, err := mc.ReadMountTable()
	require.NoError(t, err)
	_, mounted := table.Lookup("/mnt/nfs/ws1")
	assert.True(t, mounted, "the share stays mounted")
}

// TestHostNFSMounter_RegisterAfterStartRefused: requirements are fixed once
// the loop runs.
func TestHostNFSMounter_RegisterAfterStartRefused(t *testing.T) {
	m := NewHostNFSMounter(newSyncMountChecker(), nil)
	require.NoError(t, m.Start(context.Background()))
	_, err := m.Register("docker-a", "docker", hostNFSConfig(true, shareWS1))
	require.Error(t, err)
	m.Stop(context.Background())
}
