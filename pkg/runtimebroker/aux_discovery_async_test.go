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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// blockingResolver returns a resolver that blocks every discovery check
// (agent name "") of the profiles in block until release is closed, then
// fails them, and resolves every other call through byProfile; any other
// profile fails at once. Commands pass the agent's name, so they are never
// blocked.
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

// releaseAtCleanup returns a channel that is closed when the test ends, so
// blocked stub checks finish.
func releaseAtCleanup(t *testing.T) chan struct{} {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return release
}

// runPassWithin runs a discovery pass over dirs and fails the test if it
// does not return within limit.
func runPassWithin(t *testing.T, srv *Server, dirs []string, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.discoverAuxiliaryRuntimesForProjects(dirs)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("the discovery pass did not return within %s", limit)
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

	cfg := testServerConfig()
	cfg.Host = "127.0.0.1"
	cfg.StateDir = t.TempDir()
	srv := New(cfg, &filteringMockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	srv.auxCheckTimeout = time.Hour

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
// after the check timeout, and the profile is reported unavailable.
func TestAuxDiscovery_ProfileCheckTimeoutApplied(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true}, releaseAtCleanup(t), map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)

	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)
	checks := srv.profileRuntimeHealthChecks()
	assert.Equal(t, api.BrokerHealthUnavailable, checks["profile_runtime.stale"])
	assert.NotContains(t, checks, "profile_runtime.good")
	assert.Equal(t, []string{"kubernetes|context=good-cluster|namespace="}, auxKeys(srv), "the healthy profile was not registered")
}

// TestAuxDiscovery_HungChecksDoNotStarveHealthyProfile: more hung checks
// than worker slots do not stop a later healthy profile from registering,
// because a timed-out check releases its slot at once.
func TestAuxDiscovery_HungChecksDoNotStarveHealthyProfile(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
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
	srv.resolveAuxiliaryRuntime = blockingResolver(block, releaseAtCleanup(t), map[string]runtime.Runtime{"zz-healthy": fakeKubernetesRuntime("ok", "")}, nil)

	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)
	assert.Equal(t, []string{"kubernetes|context=ok|namespace="}, auxKeys(srv), "the healthy profile after the hung ones was not registered")
	assert.Len(t, srv.profileRuntimeHealthChecks(), auxProfileHealthMax, "the profile checks are not capped")
}

// TestAuxDiscovery_UnreachableProfileWarningNamesProfile: the self-health
// report names a timed-out profile with a fixed value and carries nothing
// from the resolution error, and it does not degrade the broker's status.
func TestAuxDiscovery_UnreachableProfileWarningNamesProfile(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true}, releaseAtCleanup(t), map[string]runtime.Runtime{"good": fakeKubernetesRuntime("good-cluster", "")}, nil)

	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)

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

// TestAuxDiscovery_FastNonActiveFailureNotReported: a non-active profile
// whose runtime fails fast (for example the built-in remote profile on a
// broker without cluster access, ptone/scion#3605) gets no self-health
// check, so doctor shows nothing for it; a fast failure of the project's
// active profile is reported.
func TestAuxDiscovery_FastNonActiveFailureNotReported(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	projectDir := t.TempDir()
	// Only the embedded defaults' profiles, which include a built-in
	// Kubernetes profile ("remote") that fails fast here.
	writeProjectSettings(t, projectDir, "schema_version: \"1\"\n")
	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, nil, nil)
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectDir})
	assert.Empty(t, srv.profileRuntimeHealthChecks(), "a fast failure of a non-active profile was reported")
	assert.Empty(t, srv.GetHealthInfo(context.Background()).Checks["profile_runtime.remote"])

	activeDir := t.TempDir()
	writeProjectSettings(t, activeDir, `schema_version: "1"
active_profile: stale
profiles:
  stale:
    runtime: k8s-stale
runtimes:
  k8s-stale:
    type: kubernetes
    context: deleted-cluster
`)
	srv.discoverAuxiliaryRuntimesForProjects([]string{activeDir})
	assert.Equal(t, api.BrokerHealthUnavailable, srv.profileRuntimeHealthChecks()["profile_runtime.stale"], "a failure of the active profile was not reported")
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.remote")
}

// lockedWriter serialises writes to w.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestAuxDiscovery_LogLevels pins the log levels: a timed-out check logs at
// Warn naming the profile, a failure of the active profile stays Warn, and a
// fast failure of a non-active profile stays Info.
func TestAuxDiscovery_LogLevels(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, `schema_version: "1"
active_profile: act
profiles:
  act:
    runtime: k8s-act
  hung:
    runtime: k8s-hung
  fast:
    runtime: k8s-fast
runtimes:
  k8s-act:
    type: kubernetes
    context: act
  k8s-hung:
    type: kubernetes
    context: hung
  k8s-fast:
    type: kubernetes
    context: fast
`)
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"hung": true}, releaseAtCleanup(t), nil, nil)
	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	line := func(profile string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "profile="+profile+" ") || strings.HasSuffix(l, "profile="+profile) {
				return l
			}
		}
		return ""
	}
	assert.Contains(t, line("hung"), "level=WARN", "a timed-out check is not a warning")
	assert.Contains(t, line("hung"), "did not finish in time")
	assert.Contains(t, line("act"), "level=WARN", "an active-profile failure is not a warning")
	assert.Contains(t, line("fast"), "level=INFO", "a fast non-active failure is not Info")
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
	// Count only the fixture's profile: the embedded default settings add
	// built-in profiles to every project, and each of those is also checked
	// once.
	var calls atomic.Int32
	inner := blockingResolver(nil, nil, nil, nil)
	srv.resolveAuxiliaryRuntime = func(p, a, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile == "stale" {
			calls.Add(1)
		}
		return inner(p, a, profile)
	}

	srv.discoverAuxiliaryRuntimesForProjects(dirs)
	assert.Equal(t, int32(1), calls.Load(), "the shared stale profile was checked more than once")
}

// TestAuxDiscovery_SameNameOtherSettingsKeptApart: two projects define a
// profile of the same name with different settings. The one that timed out
// stays reported although the other resolves, also after a command
// resolves the other; and two that both time out get distinct check names.
func TestAuxDiscovery_SameNameOtherSettingsKeptApart(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	mk := func(context string) string {
		d := t.TempDir()
		writeProjectSettings(t, d, fmt.Sprintf(`schema_version: "1"
profiles:
  k8s:
    runtime: k
runtimes:
  k:
    type: kubernetes
    context: %s
`, context))
		return d
	}
	deadDir, okDir := mk("dead"), mk("ok")
	release := releaseAtCleanup(t)
	okRT := fakeKubernetesRuntime("ok", "")
	srv.resolveAuxiliaryRuntime = func(p, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile != "k8s" {
			return &runtime.ErrorRuntime{Err: errors.New("no")}, config.RuntimeBuildRecord{}
		}
		if p == okDir {
			return okRT, config.RuntimeBuildRecord{}
		}
		<-release
		return &runtime.ErrorRuntime{Err: errors.New("cluster unreachable")}, config.RuntimeBuildRecord{}
	}

	runPassWithin(t, srv, []string{deadDir, okDir}, 2*time.Second)
	assert.Equal(t, api.BrokerHealthUnavailable, srv.profileRuntimeHealthChecks()["profile_runtime.k8s"], "the other project's resolved profile hid the failure")

	res := srv.resolveManagerForOpts(api.StartOptions{Name: "a1", ProjectPath: okDir, Profile: "k8s"})
	require.Same(t, runtime.Runtime(okRT), res.Runtime)
	assert.Equal(t, api.BrokerHealthUnavailable, srv.profileRuntimeHealthChecks()["profile_runtime.k8s"], "a command resolving the other settings cleared the failure")

	// A command resolving the failing settings themselves clears them.
	deadRT := fakeKubernetesRuntime("dead", "")
	srv.resolveAuxiliaryRuntime = func(p, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if p == deadDir && profile == "k8s" {
			return deadRT, config.RuntimeBuildRecord{}
		}
		return okRT, config.RuntimeBuildRecord{}
	}
	res = srv.resolveManagerForOpts(api.StartOptions{Name: "a2", ProjectPath: deadDir, Profile: "k8s"})
	require.Same(t, runtime.Runtime(deadRT), res.Runtime)
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.k8s", "a command resolving the failing settings did not clear them")

	// Both time out: two checks, the second with a suffix.
	srv2 := newDiscoveryTestServer(t, "docker")
	srv2.auxCheckTimeout = 500 * time.Millisecond
	srv2.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"k8s": true}, release, nil, nil)
	runPassWithin(t, srv2, []string{deadDir, mk("dead-2")}, 2*time.Second)
	checks := srv2.profileRuntimeHealthChecks()
	assert.Equal(t, api.BrokerHealthUnavailable, checks["profile_runtime.k8s"])
	assert.Equal(t, api.BrokerHealthUnavailable, checks["profile_runtime.k8s-2"])
}

// TestAuxDiscovery_LateSuccessRegistersRuntimeAndClearsCheck: a check that
// finishes after its timeout and succeeds registers its runtime (under the
// registry lock) and clears the profile's unavailable check.
func TestAuxDiscovery_LateSuccessRegistersRuntimeAndClearsCheck(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
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

	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale")
	close(release)
	require.Eventually(t, func() bool {
		_, ok := srv.profileRuntimeHealthChecks()["profile_runtime.stale"]
		return !ok && len(auxKeys(srv)) == 1
	}, 5*time.Second, 10*time.Millisecond, "the late success did not register and clear the check")
	assert.Equal(t, []string{"kubernetes|context=deleted-cluster|namespace="}, auxKeys(srv))
}

// TestAuxDiscovery_LaterPassClearsCheck: a later pass (the next start) that
// finds the runtime available clears the profile's unavailable check, and
// so does a command that resolves the profile while the check is reported.
func TestAuxDiscovery_LaterPassClearsCheck(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	release := releaseAtCleanup(t)
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true, "good": true}, release, nil, nil)
	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale")
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.good")

	// Second pass: stale resolves, good times out again; commands (a
	// non-empty agent name) resolve good.
	good := fakeKubernetesRuntime("good-cluster", "")
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"good": true}, release, map[string]runtime.Runtime{
		"stale": fakeKubernetesRuntime("deleted-cluster", ""),
		"good":  good,
	}, nil)
	runPassWithin(t, srv, []string{projectDir}, 2*time.Second)
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale", "a later pass did not clear the check")
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.good", "good must still be reported before the command")

	res := srv.resolveManagerForOpts(api.StartOptions{Name: "a1", ProjectPath: projectDir, Profile: "good"})
	require.Same(t, runtime.Runtime(good), res.Runtime)
	assert.NotContains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.good", "a command resolving the profile did not clear the check")
}

// TestAuxDiscovery_RecoverLaunchesAfterPass: launch recovery runs once the
// background pass has ended, also when no profile resolves.
func TestAuxDiscovery_RecoverLaunchesAfterPass(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = time.Hour
	writeProjectSettings(t, filepath.Join(os.Getenv("HOME"), ".scion", "projects", "p1", ".scion"), twoK8sProfiles)
	release := make(chan struct{})
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true, "good": true}, release, nil, nil)
	var recovered atomic.Bool
	srv.afterAuxDiscovery = func() { recovered.Store(true) }

	srv.startAuxiliaryRuntimeDiscovery(context.Background())
	assert.False(t, srv.waitAuxiliaryRuntimeDiscovery(50*time.Millisecond))
	assert.False(t, recovered.Load(), "launch recovery ran before the pass ended")
	close(release)
	require.True(t, srv.waitAuxiliaryRuntimeDiscovery(5*time.Second))
	assert.True(t, recovered.Load(), "launch recovery did not run after the pass")
}

// TestAuxDiscovery_ShutdownStopsPass: Shutdown's stop during a blocked
// pass ends it; after the checks are released and have returned, nothing is
// registered and launch recovery does not run.
func TestAuxDiscovery_ShutdownStopsPass(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = time.Hour
	writeProjectSettings(t, filepath.Join(os.Getenv("HOME"), ".scion", "projects", "p1", ".scion"), twoK8sProfiles)
	release := make(chan struct{})
	var started, returned atomic.Int32
	good := fakeKubernetesRuntime("good-cluster", "")
	srv.resolveAuxiliaryRuntime = func(_, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile == "good" || profile == "stale" {
			started.Add(1)
			defer returned.Add(1)
			<-release
			return good, config.RuntimeBuildRecord{}
		}
		return &runtime.ErrorRuntime{Err: errors.New("no")}, config.RuntimeBuildRecord{}
	}
	var recovered atomic.Bool
	srv.afterAuxDiscovery = func() { recovered.Store(true) }

	srv.startAuxiliaryRuntimeDiscovery(context.Background())
	require.False(t, srv.waitAuxiliaryRuntimeDiscovery(50*time.Millisecond))
	start := time.Now()
	srv.stopAuxiliaryRuntimeDiscovery()
	assert.Less(t, time.Since(start), auxDiscoveryStopWait, "stopping waited for the blocked checks")
	assert.True(t, srv.waitAuxiliaryRuntimeDiscovery(time.Second), "the pass did not end on stop")

	close(release)
	// Every blocked check that started has returned: nothing is left that
	// could register.
	require.Eventually(t, func() bool { return returned.Load() == started.Load() }, 5*time.Second, 5*time.Millisecond)
	assert.Empty(t, auxKeys(srv), "a runtime was registered after stop")
	assert.False(t, recovered.Load(), "launch recovery ran after stop")
}

// TestAuxDiscovery_LateResultAfterStopNotRegistered: a check that timed out
// in a pass that ended normally, and that succeeds only after Shutdown's
// stop, registers nothing and stays reported.
func TestAuxDiscovery_LateResultAfterStopNotRegistered(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = 500 * time.Millisecond
	writeProjectSettings(t, filepath.Join(os.Getenv("HOME"), ".scion", "projects", "p1", ".scion"), twoK8sProfiles)
	release := make(chan struct{})
	late := fakeKubernetesRuntime("deleted-cluster", "")
	srv.resolveAuxiliaryRuntime = func(_, agentName, profile string) (runtime.Runtime, config.RuntimeBuildRecord) {
		if profile == "stale" {
			<-release
			return late, config.RuntimeBuildRecord{}
		}
		return &runtime.ErrorRuntime{Err: errors.New("no")}, config.RuntimeBuildRecord{}
	}
	lateDone := make(chan struct{})
	srv.afterLateAuxResult = func() { close(lateDone) }
	srv.afterAuxDiscovery = func() {}

	srv.startAuxiliaryRuntimeDiscovery(context.Background())
	require.True(t, srv.waitAuxiliaryRuntimeDiscovery(2*time.Second), "the pass did not end")
	require.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale")

	srv.stopAuxiliaryRuntimeDiscovery()
	close(release)
	select {
	case <-lateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the late result was not handled")
	}
	assert.Empty(t, auxKeys(srv), "a late result registered a runtime after stop")
	assert.Contains(t, srv.profileRuntimeHealthChecks(), "profile_runtime.stale", "a late result cleared the check after stop")
}

// TestAuxDiscovery_StopBeforeStartStartsNoPass: a stop that comes before
// the pass is started keeps the pass (and launch recovery) from running.
func TestAuxDiscovery_StopBeforeStartStartsNoPass(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	writeProjectSettings(t, filepath.Join(os.Getenv("HOME"), ".scion", "projects", "p1", ".scion"), twoK8sProfiles)
	var calls atomic.Int32
	srv.resolveAuxiliaryRuntime = blockingResolver(nil, nil, nil, &calls)
	var recovered atomic.Bool
	srv.afterAuxDiscovery = func() { recovered.Store(true) }

	srv.stopAuxiliaryRuntimeDiscovery()
	srv.startAuxiliaryRuntimeDiscovery(context.Background())
	assert.True(t, srv.waitAuxiliaryRuntimeDiscovery(time.Second))
	assert.Equal(t, int32(0), calls.Load(), "a pass ran after stop")
	assert.False(t, recovered.Load(), "launch recovery ran after stop")
}

// TestAuxDiscovery_CommandDuringBlockedPassResolvesOwnRuntime: a start
// that arrives while the pass is blocked on its profile resolves the
// profile's runtime itself (unbounded, as before), or returns the error
// runtime, never the default runtime.
func TestAuxDiscovery_CommandDuringBlockedPassResolvesOwnRuntime(t *testing.T) {
	srv := newDiscoveryTestServer(t, "docker")
	srv.auxCheckTimeout = time.Hour
	projectDir := t.TempDir()
	writeProjectSettings(t, projectDir, twoK8sProfiles)
	good := fakeKubernetesRuntime("good-cluster", "")
	srv.resolveAuxiliaryRuntime = blockingResolver(map[string]bool{"stale": true, "good": true}, releaseAtCleanup(t), map[string]runtime.Runtime{"good": good}, nil)

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
	srv.setProfileRuntimeUnavailable("stale-key", "stale", true)

	health := srv.GetHealthInfo(context.Background())
	assert.Equal(t, "unavailable", health.Checks["runtime"])
	assert.Equal(t, "degraded", health.Status)
	assert.Equal(t, api.BrokerHealthUnavailable, health.Checks["profile_runtime.stale"])
}

// TestSuffixedCheckName: a suffixed check name never exceeds the maximum and
// never panics, also when the suffix alone is at least the maximum.
func TestSuffixedCheckName(t *testing.T) {
	long := profileRuntimeCheckPrefix + strings.Repeat("a", 70)
	for _, tc := range []struct {
		name, base, suffix string
		max                int
		want               string
		wantOK             bool
	}{
		{"fits", "profile_runtime.k8s", "-2", 64, "profile_runtime.k8s-2", true},
		{"long name is cut before the suffix", long, "-2", 64, long[:62] + "-2", true},
		{"suffix exactly the maximum", "abc", "-1234", 5, "abc", false},
		{"suffix longer than the maximum", "abcdefgh", "-123456", 5, "abcde", false},
		{"zero maximum", "abc", "-2", 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := suffixedCheckName(tc.base, tc.suffix, tc.max)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantOK, ok)
			assert.LessOrEqual(t, len(got), tc.max)
		})
	}
}
