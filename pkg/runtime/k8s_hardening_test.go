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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// --- Stage 2.1: Sync retry behavior ---

func TestSyncWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestSyncWithRetry_RetriesOnTransientError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("stream error: connection reset by peer")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestSyncWithRetry_NoRetryOnPermanentError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("permission denied: you do not have access")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on permanent error), got %d", calls)
	}
}

func TestSyncWithRetry_MaxRetriesExceeded(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("connection reset by peer")
	})
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	// 1 initial + 3 retries = 4 total
	if calls != 4 {
		t.Errorf("expected 4 calls, got %d", calls)
	}
}

func TestSyncWithRetry_RespectsContextCancellation(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	// Cancel immediately to prevent any backoff waits
	cancel()
	err := rt.syncWithRetry(ctx, func() error {
		calls++
		return fmt.Errorf("connection reset by peer")
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestIsSyncTransientError(t *testing.T) {
	tests := []struct {
		err       string
		transient bool
	}{
		{"connection reset by peer", true},
		{"stream error: broken pipe", true},
		{"unexpected EOF", true},
		{"i/o timeout", true},
		{"TLS handshake failure", true},
		{"use of closed network connection", true},
		// F15: a scale-from-zero node's exec tunnel (e.g. GKE konnectivity)
		// isn't up yet when the first exec runs right after the container
		// starts.
		{"stream failed: error dialing backend: No agent available", true},
		{"error dialing backend", true},
		{"No agent available", true},
		{"permission denied", false},
		{"pod not found", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isSyncTransientError(fmt.Errorf("%s", tt.err))
		if got != tt.transient {
			t.Errorf("isSyncTransientError(%q) = %v, want %v", tt.err, got, tt.transient)
		}
	}
}

// --- F15: exec-readiness wait (waitForExecReady / execReadyWithRetry) ---

// execReadySafetyValveSleeps caps how many backoff sleeps
// fakeExecReadyClock.Sleep tolerates before failing the test outright. The
// worst case exercised below (TestExecReadyWithRetry_GivesUpAtCap) needs 14;
// this is a generous multiple of that so a broken loop (e.g. a backoff/cap
// bug that never reaches execReadyMaxWait) fails fast with a clear test
// error instead of hanging until the package-level test timeout.
const execReadySafetyValveSleeps = 50

// fakeExecReadyClock is a controllable execReadyClock for tests: Sleep
// advances a virtual clock instead of actually blocking, so the 90s cap and
// multi-step backoff can be exercised without the test taking 90s.
type fakeExecReadyClock struct {
	t           *testing.T
	virtualTime time.Time
	slept       []time.Duration
	calls       int // every Sleep invocation, regardless of outcome
}

func newFakeExecReadyClock(t *testing.T) *fakeExecReadyClock {
	return &fakeExecReadyClock{t: t, virtualTime: time.Unix(0, 0)}
}

func (f *fakeExecReadyClock) Now() time.Time { return f.virtualTime }

func (f *fakeExecReadyClock) Sleep(ctx context.Context, d time.Duration) error {
	// Count and valve-check before the ctx.Done() check below, so a caller
	// that ignores this method's returned error (e.g. a retry loop with the
	// "if err := clock.sleep(...); err != nil { return err }" check dropped)
	// still trips the valve. If the count were only incremented on the
	// success path (after the ctx check), an already-cancelled ctx would
	// make every call return early via ctx.Err() without ever advancing
	// f.calls or f.virtualTime — the loop would then spin forever on a
	// frozen clock, and this valve would never fire.
	f.calls++
	if f.calls > execReadySafetyValveSleeps {
		f.t.Fatalf("exec-ready retry loop did not terminate after %d Sleep calls (last backoff %v) — looks like a broken backoff/cap, or a dropped sleep error, not a slow pod",
			f.calls, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	f.slept = append(f.slept, d)
	f.virtualTime = f.virtualTime.Add(d)
	return nil
}

func newExecReadyTestRuntime(clock *fakeExecReadyClock) *KubernetesRuntime {
	rt, _, _ := newTestK8sRuntime()
	rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}
	return rt
}

func TestExecReadyWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)

	calls := 0
	err := rt.execReadyWithRetry(context.Background(), func() error {
		calls++
		return nil
	}, "test-agent", "default", "test-pod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
	if len(clock.slept) != 0 {
		t.Errorf("expected no backoff sleeps, got %v", clock.slept)
	}
}

func TestExecReadyWithRetry_RetriesOnTransientError(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)

	calls := 0
	err := rt.execReadyWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("stream failed: error dialing backend: No agent available")
		}
		return nil
	}, "test-agent", "default", "test-pod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
	// Two failures before success → two backoffs: 1s, then 2s.
	wantBackoffs := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(clock.slept) != len(wantBackoffs) {
		t.Fatalf("expected %d backoff sleeps, got %v", len(wantBackoffs), clock.slept)
	}
	for i, want := range wantBackoffs {
		if clock.slept[i] != want {
			t.Errorf("backoff[%d] = %v, want %v", i, clock.slept[i], want)
		}
	}
}

func TestExecReadyWithRetry_NonTransientErrorFailsFast(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)

	calls := 0
	err := rt.execReadyWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("permission denied: you do not have access")
	}, "test-agent", "default", "test-pod")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on permanent error), got %d", calls)
	}
	if len(clock.slept) != 0 {
		t.Errorf("expected no backoff sleeps, got %v", clock.slept)
	}
}

// TestExecReadyWithRetry_GivesUpAtCap pins the exact backoff schedule, not
// just its bounds: 1s, 2s, 4s, then ten 8s steps, then one final step
// clamped to whatever is left of the 90s budget (3s) — 14 sleeps summing to
// exactly execReadyMaxWait, followed by one last probe (15 calls total)
// that observes the cap reached and gives up. Asserting the exact schedule
// (not just "total >= cap" and "each step <= 8s") catches a dropped
// remaining-time clamp, which would silently overshoot the cap instead of
// failing any bound check.
func TestExecReadyWithRetry_GivesUpAtCap(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)

	calls := 0
	err := rt.execReadyWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("error dialing backend: No agent available")
	}, "test-agent", "default", "test-pod")
	if err == nil {
		t.Fatal("expected error after the cap is reached")
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("expected error to explain the exec tunnel never came up, got: %v", err)
	}

	wantBackoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for i := 0; i < 10; i++ {
		wantBackoffs = append(wantBackoffs, execReadyMaxBackoff)
	}
	wantBackoffs = append(wantBackoffs, 3*time.Second) // clamped to the remaining budget
	if len(clock.slept) != len(wantBackoffs) {
		t.Fatalf("expected %d backoff sleeps, got %d: %v", len(wantBackoffs), len(clock.slept), clock.slept)
	}
	for i, want := range wantBackoffs {
		if clock.slept[i] != want {
			t.Errorf("backoff[%d] = %v, want %v", i, clock.slept[i], want)
		}
	}

	if wantCalls := len(wantBackoffs) + 1; calls != wantCalls {
		t.Errorf("expected %d calls (one per sleep, plus the final cap-triggering probe), got %d", wantCalls, calls)
	}

	if total := clock.virtualTime.Sub(time.Unix(0, 0)); total != execReadyMaxWait {
		t.Errorf("expected cumulative backoff to equal the %s cap exactly, got %s", execReadyMaxWait, total)
	}
}

func TestExecReadyWithRetry_RespectsContextCancellation(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the first backoff wait

	calls := 0
	err := rt.execReadyWithRetry(ctx, func() error {
		calls++
		return fmt.Errorf("error dialing backend: No agent available")
	}, "test-agent", "default", "test-pod")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 attempt before the cancelled context aborts the wait, got %d", calls)
	}
	if len(clock.slept) != 0 {
		t.Errorf("expected no completed sleeps once ctx is already cancelled, got %v", clock.slept)
	}
}

// TestExecReadyWithRetry_NilClockFallsBackToReal pins the nil-clock fallback
// in execReadyWithRetry: a KubernetesRuntime built as a bare struct literal
// (not via NewKubernetesRuntime) has a zero-value execReadyClock, whose
// now/sleep funcs are nil. Without the fallback, this panics on the first
// clock.now() call; every other test here goes through newTestK8sRuntime
// (which does call NewKubernetesRuntime), so none of them would catch that
// fallback being removed.
func TestExecReadyWithRetry_NilClockFallsBackToReal(t *testing.T) {
	rt := &KubernetesRuntime{}

	calls := 0
	err := rt.execReadyWithRetry(context.Background(), func() error {
		calls++
		return nil
	}, "test-agent", "default", "test-pod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

// TestRealExecReadyClock_SleepRespectsContextCancellation covers the real
// (non-fake) clock's own ctx handling directly — fakeExecReadyClock does its
// own ctx.Done() check up front, so a break in realExecReadyClock's select
// would not otherwise be caught by any test above.
func TestRealExecReadyClock_SleepRespectsContextCancellation(t *testing.T) {
	clock := realExecReadyClock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := clock.sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("expected sleep to return promptly on an already-cancelled context, took %v", elapsed)
	}
}

// TestWaitForExecReady_NoTransport_FailsFast pins that waitForExecReady,
// wired through the real execInPod, still fails immediately (not after
// retrying for up to execReadyMaxWait) when there is no exec transport at
// all — the same "K8s REST config not available" guard execInPod already
// has for fake test clientsets. This is a wiring check for waitForExecReady
// itself; the retry/backoff/cap logic is covered above via
// execReadyWithRetry with a test-supplied op.
func TestWaitForExecReady_NoTransport_FailsFast(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	err := rt.waitForExecReady(context.Background(), "default", "some-pod", "test-agent")
	if err == nil {
		t.Fatal("expected error with no exec transport available")
	}
	if !strings.Contains(err.Error(), "REST config not available") {
		t.Errorf("expected the no-transport guard error, got: %v", err)
	}
}

// TestWaitForExecReady_ProbeIsBoundedByItsOwnTimeout pins that waitForExecReady
// really does run each probe under its own execReadyProbeTimeout-bounded
// context (via context.WithTimeout), not an unbounded one. execProbe lets a
// test substitute a probe that blocks until its ctx argument is done,
// instead of a real exec transport: if the per-probe timeout were replaced
// with an unbounded derived context (e.g. context.WithCancel and no
// deadline), that ctx would never become done on its own, the probe would
// block forever, and this test would hang instead of completing in
// milliseconds. execReadyProbeTimeout is temporarily shrunk so the timeout
// actually firing doesn't require waiting anywhere near its 10s production
// value, and a goroutine plus an explicit test-level timeout bounds the
// hang risk if the per-probe timeout is ever removed.
func TestWaitForExecReady_ProbeIsBoundedByItsOwnTimeout(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	origProbeTimeout := execReadyProbeTimeout
	execReadyProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { execReadyProbeTimeout = origProbeTimeout })

	clock := newFakeExecReadyClock(t)
	rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}

	calls := 0
	rt.execProbe = func(ctx context.Context, namespace, podName string) error {
		calls++
		if calls < 3 {
			<-ctx.Done() // only the probe's own bounded timeout should end this
			return ctx.Err()
		}
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- rt.waitForExecReady(context.Background(), "default", "some-pod", "test-agent")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the wait to succeed once the probe stops stalling, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForExecReady did not return — the per-probe timeout may not be bounding the probe's context (e.g. WithCancel instead of WithTimeout)")
	}

	if calls != 3 {
		t.Errorf("expected 3 probe calls, got %d", calls)
	}
	// Two probe timeouts before success → two backoffs via the fake clock,
	// proving each timeout was classified transient (via wrapProbeTimeout)
	// and retried, rather than failing the whole wait outright.
	if len(clock.slept) != 2 {
		t.Errorf("expected 2 backoff sleeps (one per timed-out probe), got %v", clock.slept)
	}
}

// --- F15: per-probe timeout treated as transient (wrapProbeTimeout) ---

// TestWrapProbeTimeout_RewritesProbeOwnDeadline pins that a probe hitting
// its own execReadyProbeTimeout comes back as a "timeout" error (so
// isSyncTransientError retries it), not Go's plain "context deadline
// exceeded" (which that classifier does not otherwise recognize).
func TestWrapProbeTimeout_RewritesProbeOwnDeadline(t *testing.T) {
	parent := context.Background()
	probeCtx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second)) // already expired
	defer cancel()
	<-probeCtx.Done()

	origErr := errors.New("exec failed: context deadline exceeded")
	err := wrapProbeTimeout(parent, probeCtx, origErr)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Errorf("expected the rewritten error to contain the word isSyncTransientError matches on (timeout), got: %v", err)
	}
	if !isSyncTransientError(err) {
		t.Errorf("expected the rewritten probe timeout to classify as transient: %v", err)
	}
	if !errors.Is(err, origErr) {
		t.Errorf("expected the original error to still be in the chain (%%w), got: %v", err)
	}
}

// TestWrapProbeTimeout_LeavesParentCancellationAlone pins that when the
// *parent's own* deadline has expired (not just the probe's bounded child
// context — a child always inherits DeadlineExceeded once its parent's does,
// so probeCtx.Err() alone can't tell the two apart), the error is returned
// unchanged. That case must propagate up and end the retry loop via
// clock.sleep's ctx check, not be retried forever as if it were an ordinary
// transient probe failure, and rewriting it would also discard the real
// exec error in favor of a bare ctx.Err() once the loop does exit.
func TestWrapProbeTimeout_LeavesParentCancellationAlone(t *testing.T) {
	parent, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelParent()
	<-parent.Done()

	probeCtx, cancel := context.WithTimeout(parent, execReadyProbeTimeout)
	defer cancel()
	<-probeCtx.Done()

	// Confirm the test actually sets up the case it claims to: both the
	// parent and the derived probe context must already report
	// DeadlineExceeded, so the only thing distinguishing "the probe's own
	// timeout" from "the parent's own deadline" is the ctx.Err() != nil
	// check inside wrapProbeTimeout, not probeCtx.Err() alone.
	if parent.Err() != context.DeadlineExceeded {
		t.Fatalf("test setup invalid: parent.Err() = %v, want context.DeadlineExceeded", parent.Err())
	}
	if probeCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("test setup invalid: probeCtx.Err() = %v, want context.DeadlineExceeded", probeCtx.Err())
	}

	origErr := errors.New("exec failed: context deadline exceeded")
	if err := wrapProbeTimeout(parent, probeCtx, origErr); err != origErr {
		t.Errorf("expected the original error unchanged when the parent ctx's own deadline (not just the probe's) has expired, got: %v", err)
	}
}

// TestWrapProbeTimeout_NilErrorPassthrough pins the err == nil guard
// specifically: even with a live parent and an already-expired probeCtx
// (otherwise exactly the "rewrite" case), a nil err must stay nil rather
// than being turned into a non-nil wrapped error.
func TestWrapProbeTimeout_NilErrorPassthrough(t *testing.T) {
	parent := context.Background()
	probeCtx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer cancel()
	<-probeCtx.Done()

	if err := wrapProbeTimeout(parent, probeCtx, nil); err != nil {
		t.Errorf("expected nil passthrough when err is nil, even with an expired probeCtx, got: %v", err)
	}
}

// --- Stage 2.2: Pod spec hardening ---

func TestBuildPod_SecurityContext_FSGroup(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.SecurityContext == nil {
		t.Fatal("expected SecurityContext to be set")
	}
	if pod.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("expected FSGroup to be set")
	}
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 {
		t.Fatalf("expected RunAsUser=1000, got %v", pod.Spec.SecurityContext.RunAsUser)
	}
	if pod.Spec.SecurityContext.RunAsGroup == nil || *pod.Spec.SecurityContext.RunAsGroup != 1000 {
		t.Fatalf("expected RunAsGroup=1000, got %v", pod.Spec.SecurityContext.RunAsGroup)
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("expected RunAsNonRoot=true to be set")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil {
		t.Fatal("expected SeccompProfile to be set")
	}
	if pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("expected SeccompProfile RuntimeDefault, got %q", pod.Spec.SecurityContext.SeccompProfile.Type)
	}
}

func TestBuildPod_ContainerSecurityContextRestrictedDefaults(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected exactly one container, got %d", len(pod.Spec.Containers))
	}
	securityContext := pod.Spec.Containers[0].SecurityContext
	if securityContext == nil {
		t.Fatal("expected container SecurityContext to be set")
	}
	if securityContext.AllowPrivilegeEscalation == nil || *securityContext.AllowPrivilegeEscalation {
		t.Fatal("expected AllowPrivilegeEscalation=false to be set")
	}
	if securityContext.Capabilities == nil {
		t.Fatal("expected container capabilities to be set")
	}
	if len(securityContext.Capabilities.Drop) != 1 || securityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("expected capabilities.drop=[ALL], got %v", securityContext.Capabilities.Drop)
	}
}

func TestBuildPod_NodeSelector(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			NodeSelector: map[string]string{
				"gpu":  "true",
				"zone": "us-central1-a",
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.NodeSelector) != 2 {
		t.Errorf("expected 2 nodeSelector entries, got %d", len(pod.Spec.NodeSelector))
	}
	if pod.Spec.NodeSelector["gpu"] != "true" {
		t.Errorf("expected nodeSelector gpu=true, got %s", pod.Spec.NodeSelector["gpu"])
	}
}

func TestBuildPod_Tolerations(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			Tolerations: []api.K8sToleration{
				{
					Key:      "dedicated",
					Operator: "Equal",
					Value:    "agents",
					Effect:   "NoSchedule",
				},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("expected toleration key 'dedicated', got %s", pod.Spec.Tolerations[0].Key)
	}
	if pod.Spec.Tolerations[0].Value != "agents" {
		t.Errorf("expected toleration value 'agents', got %s", pod.Spec.Tolerations[0].Value)
	}
	if pod.Spec.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("expected effect NoSchedule, got %s", pod.Spec.Tolerations[0].Effect)
	}
}

func TestBuildPod_RuntimeClassName(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			RuntimeClassName: "gvisor",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.RuntimeClassName == nil {
		t.Fatal("expected RuntimeClassName to be set")
	}
	if *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("expected RuntimeClassName 'gvisor', got %s", *pod.Spec.RuntimeClassName)
	}
}

func TestBuildPod_EphemeralStorageLimits(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Resources: &api.ResourceSpec{
			Disk: "10Gi",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	res := pod.Spec.Containers[0].Resources

	// Disk should appear in both requests and limits
	if _, ok := res.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage in requests")
	}
	if _, ok := res.Limits[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage in limits")
	}

	reqVal := res.Requests[corev1.ResourceEphemeralStorage]
	limVal := res.Limits[corev1.ResourceEphemeralStorage]
	if reqVal.String() != limVal.String() {
		t.Errorf("expected requests (%s) == limits (%s) for ephemeral-storage", reqVal.String(), limVal.String())
	}
}

func TestBuildPod_SafeResourceParsing_InvalidValues(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		name   string
		config RunConfig
	}{
		{
			name: "invalid CPU request",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "not-a-cpu"}},
			},
		},
		{
			name: "invalid memory limit",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "xyz"}},
			},
		},
		{
			name: "invalid disk",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Disk: "bogus"},
			},
		},
		{
			name: "invalid k8s extended resource",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Kubernetes: &api.KubernetesConfig{
					Resources: &api.K8sResources{
						Limits: map[string]string{"nvidia.com/gpu": "not-a-number"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rt.buildPod("default", tt.config)
			if err == nil {
				t.Error("expected error for invalid resource value, got nil")
			}
		})
	}
}

// --- Stage 2.3: Image handling policy ---

func TestBuildPod_ImagePullPolicy(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		policy   string
		expected corev1.PullPolicy
	}{
		{"Always", corev1.PullAlways},
		{"Never", corev1.PullNever},
		{"IfNotPresent", corev1.PullIfNotPresent},
		{"", corev1.PullIfNotPresent}, // default
	}

	for _, tt := range tests {
		t.Run("policy_"+tt.policy, func(t *testing.T) {
			config := RunConfig{
				Name:         "test-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
			}
			if tt.policy != "" {
				config.Kubernetes = &api.KubernetesConfig{
					ImagePullPolicy: tt.policy,
				}
			}
			pod, err := rt.buildPod("default", config)
			if err != nil {
				t.Fatalf("buildPod failed: %v", err)
			}
			if pod.Spec.Containers[0].ImagePullPolicy != tt.expected {
				t.Errorf("expected pull policy %s, got %s", tt.expected, pod.Spec.Containers[0].ImagePullPolicy)
			}
		})
	}
}

func TestBuildPod_ImagePullPolicy_Invalid(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			ImagePullPolicy: "InvalidPolicy",
		},
	}

	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Error("expected error for invalid imagePullPolicy")
	}
}

// TestBuildPod_ImageAndPullPolicy_FromHubSettings pins the pod-spec end of
// ptone/scion#2156: a RunConfig shaped the way pkg/agent's resolution chain
// produces it for a Hub settings harness_configs.<h>.image /
// .image_pull_policy value (no template/agent override) must reach the pod
// spec's container image and pull policy unchanged. The settings-resolution
// precedence itself is pinned in pkg/agent (run_test.go); this only pins
// that once resolved, the values actually reach the pod the Kubernetes
// runtime creates.
func TestBuildPod_ImageAndPullPolicy_FromHubSettings(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "example.com/hub-settings-pinned:v1",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			ImagePullPolicy: "Always",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if got := pod.Spec.Containers[0].Image; got != "example.com/hub-settings-pinned:v1" {
		t.Errorf("pod container image = %q, want %q", got, "example.com/hub-settings-pinned:v1")
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
		t.Errorf("pod container ImagePullPolicy = %q, want %q", got, corev1.PullAlways)
	}
}

func TestImageExists_Validation(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		image   string
		wantErr bool
	}{
		{"valid:latest", false},
		{"gcr.io/project/image:tag", false},
		{"", true},
		{"image with spaces", true},
		{"image\twith\ttabs", true},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			_, err := rt.ImageExists(context.Background(), tt.image)
			if (err != nil) != tt.wantErr {
				t.Errorf("ImageExists(%q) error = %v, wantErr %v", tt.image, err, tt.wantErr)
			}
		})
	}
}

// --- Stage 2.4: Multi-namespace operations ---

func TestList_AllNamespaces(t *testing.T) {
	clientset := k8sfake.NewClientset()

	// Create pods in different namespaces
	for _, ns := range []string{"default", "production", "staging"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("agent-%s", ns),
				Namespace: ns,
				Labels:    map[string]string{"scion.name": fmt.Sprintf("agent-%s", ns)},
				Annotations: map[string]string{
					"scion.namespace": ns,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
		}
		_, err := clientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create pod in %s: %v", ns, err)
		}
	}

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)
	rt.ListAllNamespaces = true

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(agents) != 3 {
		t.Errorf("expected 3 agents across namespaces, got %d", len(agents))
	}

	// Verify namespace metadata is populated
	for _, a := range agents {
		if a.Kubernetes == nil {
			t.Error("expected Kubernetes metadata on agent info")
			continue
		}
		if a.Kubernetes.Namespace == "" {
			t.Error("expected namespace in Kubernetes metadata")
		}
	}
}

func TestList_SingleNamespace(t *testing.T) {
	clientset := k8sfake.NewClientset()

	// Create pods in different namespaces
	for _, ns := range []string{"default", "other"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("agent-%s", ns),
				Namespace: ns,
				Labels:    map[string]string{"scion.name": fmt.Sprintf("agent-%s", ns)},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
		}
		_, _ = clientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	}

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)
	// ListAllNamespaces is false by default

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Should only see the pod in the default namespace
	if len(agents) != 1 {
		t.Errorf("expected 1 agent (default namespace only), got %d", len(agents))
	}
}

func TestResolveNamespace_FromAnnotation(t *testing.T) {
	clientset := k8sfake.NewClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Labels:    map[string]string{"scion.name": "test-agent"},
			Annotations: map[string]string{
				"scion.namespace": "production",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	ns := rt.resolveNamespace(context.Background(), "test-agent")
	if ns != "production" {
		t.Errorf("expected namespace 'production', got %s", ns)
	}
}

func TestResolveNamespace_Default(t *testing.T) {
	clientset := k8sfake.NewClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Labels:    map[string]string{"scion.name": "test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	ns := rt.resolveNamespace(context.Background(), "test-agent")
	if ns != "default" {
		t.Errorf("expected namespace 'default', got %s", ns)
	}
}

func TestDelete_NamespaceSlashFormat(t *testing.T) {
	clientset := k8sfake.NewClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "production",
			Labels:    map[string]string{"scion.name": "test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("production").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	err := rt.Delete(context.Background(), RunRef{ID: "production/test-agent"})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify pod was deleted
	_, err = clientset.CoreV1().Pods("production").Get(context.Background(), "test-agent", metav1.GetOptions{})
	if err == nil {
		t.Error("expected pod to be deleted")
	}
}

func TestNamespaceAnnotation_Persisted(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.namespace": "custom-ns",
		},
	}

	// The Run method would set the namespace annotation; verify via buildPod
	// that annotations flow through. The namespace annotation is set in Run()
	// before buildPod, so we simulate it here.
	config.Annotations = map[string]string{
		"scion.namespace": "custom-ns",
	}

	pod, err := rt.buildPod("custom-ns", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Annotations["scion.namespace"] != "custom-ns" {
		t.Errorf("expected scion.namespace annotation 'custom-ns', got %s", pod.Annotations["scion.namespace"])
	}
}

// Verify buildPod still works correctly with all existing features after Stage 2 changes
func TestBuildPod_FullConfig_Stage2(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:             "full-test",
		Image:            "gcr.io/test/image:v1",
		UnixUsername:     "scion",
		Harness:          &EnvHarness{},
		TelemetryEnabled: true,
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"},
			Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "20Gi",
		},
		Kubernetes: &api.KubernetesConfig{
			RuntimeClassName:   "gvisor",
			ServiceAccountName: "agent-sa",
			ImagePullPolicy:    "Always",
			NodeSelector:       map[string]string{"pool": "agents"},
			Tolerations: []api.K8sToleration{
				{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			},
			Resources: &api.K8sResources{
				Limits: map[string]string{"nvidia.com/gpu": "1"},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Verify all Stage 2 features are applied
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil {
		t.Error("expected FSGroup security context")
	}
	if pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("expected RunAsUser=1000, got %v", pod.Spec.SecurityContext.RunAsUser)
	}
	if pod.Spec.SecurityContext.RunAsGroup == nil || *pod.Spec.SecurityContext.RunAsGroup != 1000 {
		t.Errorf("expected RunAsGroup=1000, got %v", pod.Spec.SecurityContext.RunAsGroup)
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("expected RunAsNonRoot=true")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("expected SeccompProfile RuntimeDefault")
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Error("expected RuntimeClassName gvisor")
	}
	if pod.Spec.ServiceAccountName != "agent-sa" {
		t.Error("expected ServiceAccountName agent-sa")
	}
	if pod.Spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Error("expected PullAlways")
	}
	if pod.Spec.NodeSelector["pool"] != "agents" {
		t.Error("expected nodeSelector pool=agents")
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Errorf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}
	containerSecurityContext := pod.Spec.Containers[0].SecurityContext
	if containerSecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if containerSecurityContext.AllowPrivilegeEscalation == nil || *containerSecurityContext.AllowPrivilegeEscalation {
		t.Error("expected AllowPrivilegeEscalation=false")
	}
	if containerSecurityContext.Capabilities == nil || len(containerSecurityContext.Capabilities.Drop) != 1 || containerSecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Errorf("expected capabilities.drop=[ALL], got %v", containerSecurityContext.Capabilities)
	}

	// Check resource values
	res := pod.Spec.Containers[0].Resources
	if res.Requests.Cpu().String() != "500m" {
		t.Errorf("expected CPU request 500m, got %s", res.Requests.Cpu().String())
	}
	if res.Limits.Cpu().String() != "2" {
		t.Errorf("expected CPU limit 2, got %s", res.Limits.Cpu().String())
	}
	if _, ok := res.Limits["nvidia.com/gpu"]; !ok {
		t.Error("expected GPU limit")
	}
	// Ephemeral storage in both requests and limits
	if _, ok := res.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage request")
	}
	if _, ok := res.Limits[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage limit")
	}
}

// Ensure existing tests still pass with signature change
func TestBuildPod_ReturnsError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod should succeed: %v", err)
	}
	if pod == nil {
		t.Fatal("expected non-nil pod")
	}
}
