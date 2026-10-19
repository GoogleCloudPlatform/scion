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
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// dialErr is the live #5370 error as execInPod returns it.
var dialErr = fmt.Errorf("exec failed: %w (stderr: )", errors.New("error dialing backend: No agent available"))

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
		{"live wording wrapped by execInPod", dialErr, true},
		{"error dialing backend only", errors.New("error dialing backend: dial tcp 10.0.0.1:10250: i/o timeout"), true},
		{"no agent available only", errors.New("No agent available"), true},
		{"internal status error with dial text", k8serrors.NewInternalError(errors.New("error dialing backend: No agent available")), true},
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
		wantCalls int   // exact call count; 0 means check maxCalls instead
		maxCalls  int
		wantSlept []time.Duration
	}{
		{
			name: "transient then success",
			exec: func(call int) error {
				if call <= 2 {
					return dialErr
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
			// Sleeps 1s and 2s (now +3s); the next 4s backoff would reach the
			// deadline at +5s, so it stops after the third attempt.
			name:      "deadline expiring",
			deadline:  5 * time.Second,
			exec:      always(dialErr),
			wantErr:   true,
			wantCalls: 3,
			wantSlept: []time.Duration{time.Second, 2 * time.Second},
		},
		{
			// 1+2+4+8*6 = 55s, then 5s to reach the 60s cap, then give up.
			name:      "persistent dial error hits the cap",
			exec:      always(dialErr),
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
		return "", dialErr
	}
	_, err := rt.execInPodDialRetry(ctx, "default", "p", []string{"true"}, "a", "home-files")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
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
					return "", dialErr
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
