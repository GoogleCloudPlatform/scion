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
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilexec "k8s.io/client-go/util/exec"
)

// errDial is the live #5370 error as execInPod returns it.
var errDial = fmt.Errorf("exec failed: %w (stderr: )", errors.New("error dialing backend: No agent available"))

// statusDialErr returns an API status error of the given kind whose message
// also carries the dial text, so only the status check can refuse it.
func statusDialErr(kind string) error {
	const msg = "error dialing backend: No agent available"
	var err error
	switch kind {
	case "401":
		err = k8serrors.NewUnauthorized(msg)
	case "403":
		err = k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", errors.New(msg))
	case "404":
		err = k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "p: "+msg)
	default:
		panic(kind)
	}
	return fmt.Errorf("exec failed: %w (stderr: )", err)
}

func TestIsExecDialTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"live wording wrapped by execInPod", errDial, true},
		{"error dialing backend only", errors.New("error dialing backend: dial tcp 10.0.0.1:10250: i/o timeout"), true},
		{"no agent available only", errors.New("No agent available"), true},
		{"internal status error with dial text", k8serrors.NewInternalError(errors.New("error dialing backend: No agent available")), true},
		{"command exit with dial text in stderr", fmt.Errorf("exec failed: %w (stderr: no agent available)",
			utilexec.CodeExitError{Err: errors.New("command terminated with non-zero exit code"), Code: 1}), false},
		{"401 with dial text", statusDialErr("401"), false},
		{"403 with dial text", statusDialErr("403"), false},
		{"404 with dial text", statusDialErr("404"), false},
		{"EOF", fmt.Errorf("exec failed: %w", io.EOF), false},
		{"connection reset", errors.New("read: connection reset by peer"), false},
		{"timeout", errors.New("i/o timeout"), false},
		{"command exit", errors.New("command terminated with exit code 1"), false},
		{"canceled", fmt.Errorf("exec failed: %w", context.Canceled), false},
		{"deadline", fmt.Errorf("exec failed: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecDialTransientError(tc.err); got != tc.want {
				t.Errorf("isExecDialTransientError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// execRetryCallLimit is a safety bound for the fake executor so a broken
// retry loop fails the test instead of spinning.
const execRetryCallLimit = 30

func TestExecInPodDialRetry(t *testing.T) {
	type step func(call int) error
	always := func(err error) step { return func(int) error { return err } }
	cases := []struct {
		name string
		// deadline, when non-zero, sets a ctx deadline this far after the
		// fake clock's start.
		deadline  time.Duration
		exec      step
		wantErr   bool
		wantIs    error // the returned error must wrap this
		wantCalls int   // exact call count
		wantSlept []time.Duration
	}{
		{
			name: "transient then success",
			exec: func(call int) error {
				if call <= 2 {
					return errDial
				}
				return nil
			},
			wantCalls: 3,
			wantSlept: []time.Duration{time.Second, 2 * time.Second},
		},
		{name: "401 not retried", exec: always(statusDialErr("401")), wantErr: true, wantCalls: 1},
		{name: "403 not retried", exec: always(statusDialErr("403")), wantErr: true, wantCalls: 1},
		{name: "404 not retried", exec: always(statusDialErr("404")), wantErr: true, wantCalls: 1},
		{name: "EOF not retried", exec: always(fmt.Errorf("exec failed: %w", io.EOF)), wantErr: true, wantIs: io.EOF, wantCalls: 1},
		{
			// Sleeps 1s and 2s (now +3s); the next 4s backoff plus the 5s
			// attempt margin would reach the deadline at +10s, so it stops
			// after the third attempt.
			name:      "deadline expiring",
			deadline:  10 * time.Second,
			exec:      always(errDial),
			wantErr:   true,
			wantCalls: 3,
			wantSlept: []time.Duration{time.Second, 2 * time.Second},
		},
		{
			// 1s backoff plus the 5s attempt margin ends at +6s, past the
			// +5s deadline, so there is no retry at all.
			name:      "deadline leaves no room for an attempt",
			deadline:  5 * time.Second,
			exec:      always(errDial),
			wantErr:   true,
			wantCalls: 1,
		},
		{
			// At +55s the 8s backoff is first shortened to the 5s left of the
			// cap: 55+5+5 = 65 is before the +66s deadline, so the last retry
			// is made (8s unshortened would give 68 and stop one early).
			name:      "backoff shortened to the cap before the deadline check",
			deadline:  66 * time.Second,
			exec:      always(errDial),
			wantErr:   true,
			wantCalls: 11,
			wantSlept: []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second,
				8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second,
				5 * time.Second},
		},
		{
			// 1+2+4+8*6 = 55s, then 5s to reach the 60s cap, then give up.
			name:      "persistent dial error hits the cap",
			exec:      always(errDial),
			wantErr:   true,
			wantCalls: 11,
			wantSlept: []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second,
				8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second,
				5 * time.Second},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeExecReadyClock(t)
			// Start the fake clock an hour in the real future so a ctx
			// deadline set against it never fires in real time.
			clock.virtualTime = time.Now().Add(time.Hour)
			rt := newExecReadyTestRuntime(clock)
			calls := 0
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				calls++
				if calls > execRetryCallLimit {
					return "", errors.New("call limit reached")
				}
				if !slices.Equal(cmd, []string{"sh", "-c", "script"}) {
					t.Errorf("cmd = %q", cmd)
				}
				return "out", tc.exec(calls)
			}
			ctx := context.Background()
			var deadline time.Time
			if tc.deadline != 0 {
				deadline = clock.virtualTime.Add(tc.deadline)
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
			}

			out, err := rt.execInPodDialRetry(ctx, "default", "p", []string{"sh", "-c", "script"}, "a", "home-files")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && out != "out" {
				t.Errorf("out = %q", out)
			}
			if tc.wantErr && tc.wantIs == nil && calls > 1 && !strings.Contains(err.Error(), "No agent available") {
				t.Errorf("err = %v, want the last dial error kept", err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantIs)
			}
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if !slices.Equal(clock.slept, tc.wantSlept) {
				t.Errorf("slept = %v, want %v", clock.slept, tc.wantSlept)
			}
			if !deadline.IsZero() && clock.virtualTime.After(deadline) {
				t.Errorf("slept past the deadline: now %v, deadline %v", clock.virtualTime, deadline)
			}
		})
	}
}

func TestExecInPodDialRetry_CancelStopsWait(t *testing.T) {
	clock := newFakeExecReadyClock(t)
	rt := newExecReadyTestRuntime(clock)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	rt.podExec = func(context.Context, string, string, []string) (string, error) {
		calls++
		cancel()
		return "", errDial
	}
	_, err := rt.execInPodDialRetry(ctx, "default", "p", []string{"true"}, "a", "home-files")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "No agent available") {
		t.Errorf("err = %v, want the last dial error named", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no attempt after cancel)", calls)
	}
}

// TestPlaceK8sHomeFiles_RetriesDialError checks that both placement modes
// retry a transient dial error on each exec and fail at once on a 403.
func TestPlaceK8sHomeFiles_RetriesDialError(t *testing.T) {
	modes := []struct {
		name  string
		cfg   RunConfig
		mode  k8sHomeFileMode
		execs int // distinct execs placement issues
	}{
		{"copy", nfsHomeTestConfig(false), k8sHomeFilesCopy, 1},
		{"link", nfsHomeTestConfig(true), k8sHomeFilesLink, 2},
	}
	for _, m := range modes {
		t.Run(m.name+"/transient then success", func(t *testing.T) {
			clock := newFakeExecReadyClock(t)
			rt := newExecReadyTestRuntime(clock)
			seen := map[string]int{}
			var cmds []string
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				joined := strings.Join(cmd, " ")
				cmds = append(cmds, joined)
				seen[joined]++
				if len(cmds) > execRetryCallLimit {
					return "", errors.New("call limit reached")
				}
				if seen[joined] == 1 {
					return "", errDial
				}
				return "", nil
			}
			if err := rt.placeK8sHomeFiles(context.Background(), "default", "p", m.cfg, m.mode); err != nil {
				t.Fatalf("placement failed: %v", err)
			}
			if len(seen) != m.execs || len(cmds) != 2*m.execs {
				t.Errorf("execs = %q, want each of %d commands run twice", cmds, m.execs)
			}
		})
		t.Run(m.name+"/403 not retried", func(t *testing.T) {
			clock := newFakeExecReadyClock(t)
			rt := newExecReadyTestRuntime(clock)
			calls := 0
			rt.podExec = func(context.Context, string, string, []string) (string, error) {
				calls++
				return "", statusDialErr("403")
			}
			err := rt.placeK8sHomeFiles(context.Background(), "default", "p", m.cfg, m.mode)
			if err == nil || !k8serrors.IsForbidden(err) {
				t.Fatalf("err = %v, want the 403", err)
			}
			if calls != 1 {
				t.Errorf("calls = %d, want 1", calls)
			}
			if len(clock.slept) != 0 {
				t.Errorf("slept = %v, want none", clock.slept)
			}
		})
	}
}

func TestCheckK8sHomeFileTargets_RejectsTempName(t *testing.T) {
	err := checkK8sHomeFileTargets([]k8sFilePlacement{
		{Source: "/run/scion/auth-files/a", Target: "/home/scion/.config/" + k8sPlacementTempName, Mode: 0o600},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved name") {
		t.Errorf("err = %v, want the reserved temporary name rejected", err)
	}
	if err := checkK8sHomeFileTargets([]k8sFilePlacement{
		{Source: "/run/scion/auth-files/a", Target: "/home/scion/.config/" + k8sPlacementTempName + ".json", Mode: 0o600},
	}); err != nil {
		t.Errorf("a similar but different name was rejected: %v", err)
	}
}

// TestRun_StartupGateRetriesDialError checks that Run retries the startup
// gate touch after a transient dial error and starts the agent, and that a
// 403 on the gate touch fails the start without a retry.
func TestRun_StartupGateRetriesDialError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gateErr   error
		wantErr   bool
		wantGates int
		wantSlept []time.Duration
	}{
		{name: "transient then success", gateErr: errDial, wantGates: 2, wantSlept: []time.Duration{time.Second}},
		{name: "403 not retried", gateErr: statusDialErr("403"), wantErr: true, wantGates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeExecReadyClock(t)
			rt := newExecReadyTestRuntime(clock)
			clientset := rt.Client.Clientset
			rt.podReadyPollInterval = 5 * time.Millisecond
			rt.execProbe = func(context.Context, string, string) error { return nil }
			var mu sync.Mutex
			gates := 0
			rt.podExec = func(_ context.Context, _, _ string, cmd []string) (string, error) {
				if !strings.Contains(strings.Join(cmd, " "), ".scion-home-ready") {
					return "", nil
				}
				mu.Lock()
				defer mu.Unlock()
				gates++
				if gates > execRetryCallLimit {
					return "", errors.New("call limit reached")
				}
				if gates == 1 {
					return "", tc.gateErr
				}
				return "", nil
			}
			rt.homeSync = func(context.Context, string, string, string, string, []string) error { return nil }

			config := homeFileConfig()
			config.Name = "gate-agent"
			config.Harness = &MockHarness{}
			config.ResolvedAuth.Files = nil
			config.HomeDir = t.TempDir()

			errChan := make(chan error, 1)
			go func() {
				_, err := rt.Run(context.Background(), config)
				errChan <- err
			}()
			var pod *corev1.Pod
			for i := 0; i < 100; i++ {
				p, err := clientset.CoreV1().Pods("default").Get(context.Background(), "gate-agent", metav1.GetOptions{})
				if err == nil {
					pod = p
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if pod == nil {
				t.Fatal("pod was not created")
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: agentContainerName, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
			if _, err := clientset.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			var runErr error
			select {
			case runErr = <-errChan:
			case <-time.After(15 * time.Second):
				t.Fatal("Run did not return")
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.wantErr != (runErr != nil) {
				t.Fatalf("Run err = %v, wantErr %v", runErr, tc.wantErr)
			}
			if tc.wantErr && !k8serrors.IsForbidden(runErr) {
				t.Errorf("Run err = %v, want the 403", runErr)
			}
			if gates != tc.wantGates {
				t.Errorf("startup gate touches = %d, want %d", gates, tc.wantGates)
			}
			if !slices.Equal(clock.slept, tc.wantSlept) {
				t.Errorf("slept = %v, want %v", clock.slept, tc.wantSlept)
			}
		})
	}
}
