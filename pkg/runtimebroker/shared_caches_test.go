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
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A flat host's caches (ptone/scion#3274): one object per cache directory,
// the file caches partitioned per instance, the GitHub resolution cache
// shared.

func newCacheTestServer(t *testing.T, brokerID, rtName string, sc *SharedCaches) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.BrokerID = brokerID
	cfg.StateDir = t.TempDir()
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://127.0.0.1:1"
	cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{BrokerID: brokerID, SecretKey: "c2VjcmV0", HubEndpoint: cfg.HubEndpoint}
	cfg.TemplateCacheDir = filepath.Join(t.TempDir(), "own", "templates")
	cfg.SharedCaches = sc
	return New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return rtName }})
}

// TestSharedCaches_PartitionedPerInstanceAcrossScopes: two instances with
// different runtime scopes (Docker and Kubernetes) each use their own
// partition of the file caches and the host's single GitHub resolution
// cache; another server for the same instance gets the same objects (one
// object per directory); a server without shared caches still opens its
// own in the single-broker directories.
func TestSharedCaches_PartitionedPerInstanceAcrossScopes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := filepath.Join(t.TempDir(), "cache")
	sc, err := NewSharedCaches(filepath.Join(base, "templates"), 0)
	require.NoError(t, err)
	docker := newCacheTestServer(t, "rb-docker", "docker", sc)
	k8s := newCacheTestServer(t, "rb-k8s", "kubernetes", sc)
	require.NotNil(t, docker.cache)
	require.NotNil(t, k8s.cache)
	assert.NotSame(t, docker.cache, k8s.cache)
	assert.NotSame(t, docker.hcCache, k8s.hcCache)
	assert.NotSame(t, docker.skCache, k8s.skCache)
	require.NotNil(t, sc.GitHub, "the GitHub resolution cache opens under a writable HOME")
	assert.Same(t, sc.GitHub, docker.ghResolutionCache)
	assert.Same(t, sc.GitHub, k8s.ghResolutionCache)

	again := newCacheTestServer(t, "rb-docker", "docker", sc)
	assert.Same(t, docker.cache, again.cache)
	assert.Same(t, docker.hcCache, again.hcCache)
	assert.Same(t, docker.skCache, again.skCache)

	for _, c := range []struct {
		put func(string, map[string][]byte) (string, error)
		dir string
	}{
		{docker.cache.Put, filepath.Join(base, "instances", "rb-docker", "templates")},
		{docker.hcCache.Put, filepath.Join(base, "instances", "rb-docker", "harness-configs")},
		{docker.skCache.Put, filepath.Join(base, "instances", "rb-docker", "skills")},
		{k8s.cache.Put, filepath.Join(base, "instances", "rb-k8s", "templates")},
	} {
		p, err := c.put("e1", map[string][]byte{"f": []byte("x")})
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(c.dir, "e1"), p)
	}

	solo := newCacheTestServer(t, "rb-solo", "docker", nil)
	require.NotNil(t, solo.cache)
	p, err := solo.cache.Put("e1", map[string][]byte{"f": []byte("x")})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(solo.config.TemplateCacheDir, "e1"), p, "a server without shared caches uses the single-broker directory")
}

// TestSharedCaches_AnotherInstanceNeverEvictsAResolvedPath: a path one
// instance resolved survives any eviction pressure from another instance's
// inserts, however long the start takes to read it.
func TestSharedCaches_AnotherInstanceNeverEvictsAResolvedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sc, err := NewSharedCaches(filepath.Join(t.TempDir(), "cache", "templates"), 100)
	require.NoError(t, err)
	a := newCacheTestServer(t, "rb-a", "docker", sc)
	b := newCacheTestServer(t, "rb-b", "kubernetes", sc)
	big := map[string][]byte{"scion-agent.yaml": make([]byte, 60)}
	resolved, err := a.cache.Put("t-a", big)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := b.cache.Put(fmt.Sprintf("t-b%d", i), big)
		require.NoError(t, err)
	}
	assert.DirExists(t, resolved, "another instance's inserts evicted a path this instance resolved")
	_, ok := a.cache.Get("t-a")
	assert.True(t, ok)
}

// TestSharedCaches_RefusesABrokerIDThatIsNotOnePathElement: the partition
// directory is named for the broker ID, which must be a single path element.
func TestSharedCaches_RefusesABrokerIDThatIsNotOnePathElement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sc, err := NewSharedCaches(filepath.Join(t.TempDir(), "cache", "templates"), 0)
	require.NoError(t, err)
	for _, id := range []string{"", ".", "..", "../x", "a/b"} {
		_, err := sc.forInstance(id)
		assert.Error(t, err, "broker ID %q", id)
	}
}

// TestSharedCaches_ClosedOnceByTheHostNotByInstances: an instance's
// shutdown never closes the shared GitHub resolution cache; the host's
// Stop does, once.
func TestSharedCaches_ClosedOnceByTheHostNotByInstances(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sc, err := NewSharedCaches(filepath.Join(t.TempDir(), "cache", "templates"), 0)
	require.NoError(t, err)
	require.NotNil(t, sc.GitHub, "the GitHub resolution cache opens under a writable HOME")
	a := newCacheTestServer(t, "rb-a", "docker", sc)
	require.NoError(t, a.Shutdown(context.Background()))
	assert.False(t, sc.GitHub.Closed(), "an instance's shutdown leaves the shared cache open")
	sc.Stop(context.Background())
	assert.True(t, sc.GitHub.Closed())
}
