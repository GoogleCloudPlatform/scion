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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the background auxiliary runtime discovery (ptone/scion#7010).

// shortAuxCheckTimeout sets auxProfileCheckTimeout to d for the test.
func shortAuxCheckTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := auxProfileCheckTimeout
	auxProfileCheckTimeout = d
	t.Cleanup(func() { auxProfileCheckTimeout = prev })
}

// blockingResolver returns a resolver that blocks every discovery check
// (agent name "") of the profiles in block until release is closed, then
// fails them, and resolves every other call through byProfile. Commands
// pass the agent's name, so they are never blocked.
func blockingResolver(block map[string]bool, release <-chan struct{}, byProfile map[string]runtime.Runtime, calls *atomic.Int32) func(string, string, string) (runtime.Runtime, config.RuntimeBuildRecord) {
	return func(_, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile == "local" {
			// The fixture's local profile is the broker's own default runtime.
			return &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{}
		}
		if calls != nil {
			calls.Add(1)
		}
		if agentName == "" && block[profile] {
			<-release
			return &runtime.ErrorRuntime{Err: errors.New("cluster unreachable")}, config.RuntimeBuildRecord{}
		}
		if rt, ok := byProfile[profile]; ok {
			return rt, config.RuntimeBuildRecord{}
		}
		return &runtime.ErrorRuntime{Err: fmt.Errorf("token=SECRET-VALUE exec plugin failed for %q", profile)}, config.RuntimeBuildRecord{}
	}
}

const twoK8sProfiles = `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: local-docker
  stale:
    runtime: k8s-stale
  good:
    runtime: k8s-good
runtimes:
  local-docker:
    type: docker
  k8s-stale:
    type: kubernetes
    context: deleted-cluster
  k8s-good:
    type: kubernetes
    context: good-cluster
`

// TestStartServices_HubConnectNotDelayedByBlockingProfileCheck: a profile
// whose runtime check hangs does not delay the hub connections. They are
// started first, startServices returns within a bound, and discovery runs
// only after that, in the background.
func TestStartServices_HubConnectNotDelayedByBlockingProfileCheck(t *testing.T) {
	setupTestScionEnv(t)
	home := os.Getenv("HOME")
	projectDir := filepath.Join(home, ".scion", "projects", "p1", ".scion")
	writeProjectSettings(t, projectDir, twoK8sProfiles)

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := New(cfg, &filteringMockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)

	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var connected atomic.Bool
	var checkBeforeConnect atomic.Bool
	srv.onHubConnectionsStarted = func() { connected.Store(true) }
	stub := blockingResolver(map[string]bool{"stale": true}, release, map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)
	srv.resolveAuxiliaryRuntime = func(p, a, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if !connected.Load() {
			checkBeforeConnect.Store(true)
		}
		return stub(p, a, profile)
	}

	start := time.Now()
	require.NoError(t, srv.startServices(context.Background(), false))
	assert.Less(t, time.Since(start), 2*time.Second, "startServices waited for the blocked profile check")
	assert.True(t, connected.Load(), "hub connections were not started")
	assert.False(t, srv.waitAuxiliaryRuntimeDiscovery(100*time.Millisecond), "discovery finished although a check is blocked")

	once.Do(func() { close(release) })
	require.True(t, srv.waitAuxiliaryRuntimeDiscovery(5*time.Second), "discovery did not finish after the check was released")
	assert.False(t, checkBeforeConnect.Load(), "a profile runtime check ran before the hub connections started")
}

// TestAuxDiscovery_ProfileCheckTimeoutApplied: a hung check is abandoned
// after auxProfileCheckTimeout, and the profile is reported unavailable.
func TestAuxDiscovery_ProfileCheckTimeoutApplied(t *testing.T) {
	shortAuxCheckTimeout(t, 50*time.Millisecond)
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true}, release, map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)

	start := time.Now()
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	assert.Less(t, time.Since(start), time.Second, "the pass waited past the check timeout")
	assert.Equal(t, map[string]string{"profile_runtime.stale": api.BrokerHealthUnavailable}, srv.profileRuntimeHealthChecks())
	assert.Equal(t, []string{"kubernetes|context=good-cluster|namespace="}, auxKeys(srv), "the healthy profile was not registered")
}

// TestAuxDiscovery_HungChecksDoNotStarveHealthyProfile: more hung checks
// than worker slots do not stop a later healthy profile from registering,
// because a timed-out check releases its slot at once.
func TestAuxDiscovery_HungChecksDoNotStarveHealthyProfile(t *testing.T) {
	shortAuxCheckTimeout(t, 50*time.Millisecond)
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	var b strings.Builder
	b.WriteString("schema_version: \"1\"\nprofiles:\n")
	block := map[string]bool{}
	for i := 0; i < auxDiscoveryConcurrency+1; i++ {
		name := fmt.Sprintf("hung%d", i)
		block[name] = true
		fmt.Fprintf(&b, "  %s:\n    runtime: k8s-%d\n", name, i)
	}
	b.WriteString("  zz-healthy:\n    runtime: k8s-ok\nruntimes:\n")
	for i := 0; i < auxDiscoveryConcurrency+1; i++ {
		fmt.Fprintf(&b, "  k8s-%d:\n    type: kubernetes\n    context: dead-%d\n", i, i)
	}
	b.WriteString("  k8s-ok:\n    type: kubernetes\n    context: ok\n")
	writeProjectSettings(t, projectDir, b.String())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.resolveAuxiliaryRuntime = blockingResolver(block, release, map[string]runtime.Runtime{"zz-healthy": fakeKubernetesRuntime("ok", "")}, nil)

	start := time.Now()
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	assert.Less(t, time.Since(start), time.Second, "hung checks held their worker slots")
	assert.Equal(t, []string{"kubernetes|context=ok|namespace="}, auxKeys(srv), "the healthy profile after the hung ones was not registered")
	assert.Len(t, srv.profileRuntimeHealthChecks(), auxProfileHealthMax, "the profile checks are not capped")
}

// TestAuxDiscovery_UnreachableProfileWarningNamesProfile: the self-health
// report names the profile with a fixed value and carries nothing from the
// resolution error, and it does not degrade the broker's status.
func TestAuxDiscovery_UnreachableProfileWarningNamesProfile(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})

	health := srv.GetHealthInfo(context.Background())
	assert.Equal(t, api.BrokerHealthUnavailable, health.Checks["profile_runtime.stale"])
	assert.Equal(t, "healthy", health.Status, "an unavailable non-active profile degraded the broker")
	report := api.NormalizeBrokerHealthReport(&api.BrokerHealthReport{Status: health.Status, Checks: health.Checks})
	assert.Equal(t, api.BrokerHealthUnavailable, report.Checks["profile_runtime.stale"], "the check does not survive normalization")
	for name, value := range health.Checks {
		assert.NotContains(t, name+value, "SECRET-VALUE")
		assert.NotContains(t, name+value, "exec plugin")
	}
	assert.Equal(t, "profile_runtime.a_b_c", profileRuntimeCheckName("a b/c"))
}

// TestAuxDiscovery_FailedProfileCheckedOncePerPass: one stale profile
// shared by three projects is checked once per pass.
func TestAuxDiscovery_FailedProfileCheckedOncePerPass(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	var dirs []string
	for i := 0; i < 3; i++ {
		d := t.TempDir()
		writeProjectSettings(t, d, `schema_version: "1"
profiles:
  stale:
    runtime: k8s-stale
runtimes:
  k8s-stale:
    type: kubernetes
    context: deleted-cluster
`)
		dirs = append(dirs, d)
	}
	var calls atomic.Int32
	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, nil, &calls)

	srv.discoverAuxiliaryRuntimesForProjects(dirs)
	assert.Equal(t, int32(1), calls.Load(), "the shared stale profile was checked more than once")
}

// TestAuxDiscovery_LateSuccessRegistersRuntimeAndClearsCheck: a check that
// finishes after its timeout and succeeds registers its runtime (under the
// registry lock) and clears the profile's unavailable check.
func TestAuxDiscovery_LateSuccessRegistersRuntimeAndClearsCheck(t *testing.T) {
	shortAuxCheckTimeout(t, 50*time.Millisecond)
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	release := make(chan struct{})
	late := fakeKubernetesRuntime("deleted-cluster", "")
	srv.resolveAuxiliaryRuntime = func(_, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile == "stale" {
			<-release
			return late, config.RuntimeBuildRecord{}
		}
		return &runtime.ErrorRuntime{Err: errors.New("no")}, config.RuntimeBuildRecord{}
	}

	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale")
	close(release)
	require.Eventually(t, func() bool {
		_, ok := srv.profileRuntimeHealthChecks()["profile_runtime.stale"]
		return !ok && len(auxKeys(srv)) == 1
	}, 5*time.Second, 10*time.Millisecond, "the late success did not register and clear the check")
	assert.Equal(t, []string{"kubernetes|context=deleted-cluster|namespace="}, auxKeys(srv))
}

// TestAuxDiscovery_LaterPassClearsCheck: a later pass that finds the
// runtime available clears the profile's unavailable check, and so does a
// command that resolves the profile.
func TestAuxDiscovery_LaterPassClearsCheck(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, nil, nil)
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale")
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.good")

	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, map[string]runtime.Runtime{"stale": fakeKubernetesRuntime("deleted-cluster", "")}, nil)
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale", "a later pass did not clear the check")

	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)
	res := srv.resolveManagerForOpts(api.StartOptions{Name: "a1", ProjectPath: projectDir, Profile: "good"})
	require.Equal(t, "kubernetes", res.RuntimeType)
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.good", "a command resolving the profile did not clear the check")
}

// TestAuxDiscovery_RecoverLaunchesAfterPass: launch recovery runs once the
// background pass has ended, also when no profile resolves.
func TestAuxDiscovery_RecoverLaunchesAfterPass(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	writeProjectSettings(t, filepath.Join(os.Getenv("HOME"), ".scion", "projects", "p1", ".scion"), twoK8sProfiles)
	release := make(chan struct{})
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true, "good": true}, release, nil, nil)
	var recovered atomic.Bool
	prev := afterAuxiliaryRuntimeDiscovery
	afterAuxiliaryRuntimeDiscovery = func(*Server) { recovered.Store(true) }
	t.Cleanup(func() { afterAuxiliaryRuntimeDiscovery = prev })

	srv.startAuxiliaryRuntimeDiscovery(context.Background())
	time.Sleep(50 * time.Millisecond)
	assert.False(t, recovered.Load(), "launch recovery ran before the pass ended")
	close(release)
	require.True(t, srv.waitAuxiliaryRuntimeDiscovery(5*time.Second))
	assert.True(t, recovered.Load(), "launch recovery did not run after the pass")
}

// TestAuxDiscovery_CommandDuringBlockedPassResolvesOwnRuntime: a start
// that arrives while the pass is blocked on its profile resolves the
// profile's runtime itself (unbounded, as before), or returns the error
// runtime, never the default runtime.
func TestAuxDiscovery_CommandDuringBlockedPassResolvesOwnRuntime(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	good := fakeKubernetesRuntime("good-cluster", "")
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true, "good": true}, release, map[string]runtime.Runtime{"good": good}, nil)

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	}()

	res := srv.resolveManagerForOpts(api.StartOptions{Name: "agent-1", ProjectPath: projectDir, Profile: "good"})
	assert.Same(t, runtime.Runtime(good), res.Runtime, "the start did not get its profile's runtime")
	assert.NotEqual(t, resolutionDefaultFallback, res.How)

	res = srv.resolveManagerForOpts(api.StartOptions{Name: "agent-2", ProjectPath: projectDir, Profile: "stale"})
	assert.Equal(t, resolutionErrorRuntime, res.How, "an unresolvable profile did not return the error runtime")
	assert.NotEqual(t, "docker", res.RuntimeType, "an unresolvable profile fell back to the default runtime")

	select {
	case <-passDone:
		t.Fatal("the pass finished although its checks are blocked")
	default:
	}
}

// TestAuxDiscovery_ActiveProfileFailureUnchanged: a failed default runtime
// still reports runtime unavailable and degrades the broker, as before, with
// profile checks present or not.
func TestAuxDiscovery_ActiveProfileFailureUnchanged(t *testing.T) {
	setupTestScionEnv(t)
	cfg := DefaultServerConfig()
	srv := New(cfg, &filteringMockManager{}, &runtime.ErrorRuntime{Err: errors.New("default runtime failed")}, config.RuntimeBuildRecord{})
	srv.setProfileRuntimeUnavailable("stale", true)

	health := srv.GetHealthInfo(context.Background())
	assert.Equal(t, "unavailable", health.Checks["runtime"])
	assert.Equal(t, "degraded", health.Status)
	assert.Equal(t, api.BrokerHealthUnavailable, health.Checks["profile_runtime.stale"])
}
