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

package autoexpose

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// scriptedHubClient answers ListPorts from a per-call error script (once it
// runs out, ListPorts returns the ports registered so far) and RegisterPort
// and DeletePort from fixed errors.
type scriptedHubClient struct {
	mu          sync.Mutex
	listErrs    []error
	registerErr error
	deleteErr   error
	// portErrs overrides registerErr for specific ports.
	portErrs    map[int]error
	listCalls   int
	regCalls    int
	regByPort   map[int]int
	deleteCalls int
	registered  map[int]bool
}

func (c *scriptedHubClient) ListPorts(context.Context) ([]scionhub.ExposedPort, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	if len(c.listErrs) > 0 {
		err := c.listErrs[0]
		c.listErrs = c.listErrs[1:]
		return nil, err
	}
	var out []scionhub.ExposedPort
	for p := range c.registered {
		out = append(out, scionhub.ExposedPort{Port: p, ExposedBy: autoExposeLabel})
	}
	return out, nil
}

func (c *scriptedHubClient) RegisterPort(_ context.Context, req scionhub.RegisterPortRequest) (*scionhub.ExposedPort, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regCalls++
	if c.regByPort == nil {
		c.regByPort = make(map[int]int)
	}
	c.regByPort[req.Port]++
	if err, ok := c.portErrs[req.Port]; ok {
		return nil, err
	}
	if c.registerErr != nil {
		return nil, c.registerErr
	}
	if c.registered == nil {
		c.registered = make(map[int]bool)
	}
	c.registered[req.Port] = true
	return nil, nil
}

func (c *scriptedHubClient) DeletePort(context.Context, int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteCalls++
	return c.deleteErr
}

// The scripted client never deletes from registered: the tests that use
// DeletePort only check the reconciler's own tracking.

func repeatErr(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}

var (
	errHub500 = &scionhub.StatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}
	errHub401 = &scionhub.StatusError{StatusCode: http.StatusUnauthorized, Body: "unauthorized"}
	errHub422 = &scionhub.StatusError{StatusCode: http.StatusUnprocessableEntity, Body: "maximum exposed ports per agent exceeded"}
	errHub404 = &scionhub.StatusError{StatusCode: http.StatusNotFound, Body: "not found"}
	errHub429 = &scionhub.StatusError{StatusCode: http.StatusTooManyRequests, Body: "slow down"}
)

// countLogs replaces the package log hooks with counters for the test.
func countLogs(t *testing.T) (errs, infos *int) {
	t.Helper()
	var e, i int
	origErr, origInfo := logError, logInfo
	logError = func(string, ...interface{}) { e++ }
	logInfo = func(string, ...interface{}) { i++ }
	t.Cleanup(func() { logError, logInfo = origErr, origInfo })
	return &e, &i
}

func listening(ports ...int) func() ([]ListenSocket, error) {
	return func() ([]ListenSocket, error) {
		out := make([]ListenSocket, len(ports))
		for i, p := range ports {
			out[i] = ListenSocket{Port: p, BindAddr: "0.0.0.0"}
		}
		return out, nil
	}
}

// newLoopReconciler returns a reconciler with a 3s interval and no jitter.
func newLoopReconciler(client HubPortClient) *Reconciler {
	cfg := defaultTestConfig()
	cfg.Interval = 3 * time.Second
	r := NewReconciler(client, cfg)
	r.scan = func() ([]ListenSocket, error) { return nil, nil }
	r.backoff.Rand = func() float64 { return 0 }
	return r
}

// runLoop runs r.Run until it has asked for n delays and returns them. No
// real time passes: the fake timer fires at once.
func runLoop(t *testing.T, r *Reconciler, n int) []time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	r.after = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		if len(delays) >= n {
			cancel()
			return nil // never fires; Run returns on ctx.Done
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	return delays
}

func secs(ns ...int) []time.Duration {
	out := make([]time.Duration, len(ns))
	for i, n := range ns {
		out[i] = time.Duration(n) * time.Second
	}
	return out
}

func TestRun_BackoffGrowsAndCaps(t *testing.T) {
	client := &scriptedHubClient{listErrs: repeatErr(errHub500, 100)}
	r := newLoopReconciler(client)

	got := runLoop(t, r, 10)
	want := secs(3, 6, 12, 24, 48, 96, 192, 300, 300, 300)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if client.listCalls != 10 {
		t.Fatalf("ListPorts calls = %d, want 10 (one per attempt)", client.listCalls)
	}
}

func TestRun_HealthyUsesInterval(t *testing.T) {
	r := newLoopReconciler(&scriptedHubClient{})
	got := runLoop(t, r, 3)
	if want := secs(3, 3, 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRun_BackoffResetsOnSuccess(t *testing.T) {
	r := newLoopReconciler(&scriptedHubClient{})
	scanErrs := []error{errors.New("e1"), errors.New("e2"), errors.New("e3"), nil, errors.New("e4")}
	r.scan = func() ([]ListenSocket, error) {
		err := scanErrs[0]
		if len(scanErrs) > 1 {
			scanErrs = scanErrs[1:]
		}
		return nil, err
	}

	got := runLoop(t, r, 5)
	want := secs(3, 6, 12, 3, 3)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRun_UnauthorizedRetriesAtCapThenRecovers(t *testing.T) {
	client := &scriptedHubClient{listErrs: repeatErr(errHub401, 3)}
	r := newLoopReconciler(client)

	got := runLoop(t, r, 5)
	want := []time.Duration{DefaultMaxBackoff, DefaultMaxBackoff, DefaultMaxBackoff, 3 * time.Second, 3 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if r.state != stateHealthy {
		t.Fatalf("state = %v, want healthy after recovery", r.state)
	}
}

func TestReconcileOnce_UnauthorizedRegisterStopsPass(t *testing.T) {
	client := &scriptedHubClient{registerErr: errHub401}
	r := newLoopReconciler(client)
	r.scan = func() ([]ListenSocket, error) {
		return []ListenSocket{{Port: 3000, BindAddr: "0.0.0.0"}, {Port: 4000, BindAddr: "0.0.0.0"}}, nil
	}

	err := r.reconcileOnce(context.Background())
	if !scionhub.IsUnauthorized(err) {
		t.Fatalf("reconcileOnce error = %v, want a 401", err)
	}
	if client.regCalls != 1 {
		t.Fatalf("RegisterPort calls = %d, want 1 (pass ends at the first 401)", client.regCalls)
	}
}

func TestReconcileOnce_RegisterErrorTriggersBackoff(t *testing.T) {
	client := &scriptedHubClient{registerErr: errHub500}
	r := newLoopReconciler(client)
	r.scan = func() ([]ListenSocket, error) {
		return []ListenSocket{{Port: 3000, BindAddr: "0.0.0.0"}}, nil
	}

	got := runLoop(t, r, 3)
	if want := secs(3, 6, 12); !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

// A port the hub refuses with a non-retryable 4xx (422 at the hub's port
// limit) must not slow the loop, must not stop other ports, and must not be
// retried every pass.
func TestRun_RejectedPortDoesNotBackOffLoop(t *testing.T) {
	errs, _ := countLogs(t)
	client := &scriptedHubClient{portErrs: map[int]error{4000: errHub422}}
	r := newLoopReconciler(client)
	r.scan = listening(3000, 4000)

	got := runLoop(t, r, 4)
	if want := secs(3, 3, 3, 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if !r.autoExposed[3000] {
		t.Fatal("port 3000 should be auto-exposed despite port 4000 being refused")
	}
	if client.regByPort[3000] != 1 {
		t.Fatalf("port 3000 register calls = %d, want 1", client.regByPort[3000])
	}
	if client.regByPort[4000] != 1 {
		t.Fatalf("port 4000 register calls = %d, want 1 (held back after the refusal)", client.regByPort[4000])
	}
	if *errs != 1 {
		t.Fatalf("error log lines = %d, want 1 (the first refusal only)", *errs)
	}
}

func TestReconcileOnce_RejectedPortRetriedOnSlowSchedule(t *testing.T) {
	client := &scriptedHubClient{portErrs: map[int]error{4000: errHub422}}
	r := newLoopReconciler(client)
	r.scan = listening(4000)
	ctx := context.Background()

	wait := r.rejectedRetryPasses()
	if wait < 2 {
		t.Fatalf("rejectedRetryPasses = %d, want a multi-pass wait", wait)
	}
	for i := 0; i < wait; i++ {
		if err := r.reconcileOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v (a refused port must not fail the pass)", i+1, err)
		}
	}
	if client.regByPort[4000] != 1 {
		t.Fatalf("register calls after %d passes = %d, want 1", wait, client.regByPort[4000])
	}
	_ = r.reconcileOnce(ctx)
	if client.regByPort[4000] != 2 {
		t.Fatalf("register calls after the retry wait = %d, want 2", client.regByPort[4000])
	}
}

func TestReconcileOnce_RejectedPortRetriedWhenListenerReturns(t *testing.T) {
	client := &scriptedHubClient{portErrs: map[int]error{4000: errHub422}}
	r := newLoopReconciler(client)
	ctx := context.Background()

	r.scan = listening(4000)
	_ = r.reconcileOnce(ctx)
	r.scan = listening()
	_ = r.reconcileOnce(ctx)
	r.scan = listening(4000)
	_ = r.reconcileOnce(ctx)
	if client.regByPort[4000] != 2 {
		t.Fatalf("register calls = %d, want 2 (refusal forgotten once the listener went away)", client.regByPort[4000])
	}
}

func TestRun_TooManyRequestsBacksOff(t *testing.T) {
	client := &scriptedHubClient{registerErr: errHub429}
	r := newLoopReconciler(client)
	r.scan = listening(3000)

	got := runLoop(t, r, 3)
	if want := secs(3, 6, 12); !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestReconcileOnce_UnauthorizedDeregisterStopsPass(t *testing.T) {
	client := &scriptedHubClient{deleteErr: errHub401}
	r := newLoopReconciler(client)
	r.autoExposed[3000] = true
	r.autoExposed[4000] = true

	err := r.reconcileOnce(context.Background())
	if !scionhub.IsUnauthorized(err) {
		t.Fatalf("reconcileOnce error = %v, want a 401", err)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("DeletePort calls = %d, want 1 (pass ends at the first 401)", client.deleteCalls)
	}
}

func TestReconcileOnce_DeregisterNotFoundIsDone(t *testing.T) {
	client := &scriptedHubClient{deleteErr: errHub404}
	r := newLoopReconciler(client)
	r.autoExposed[3000] = true

	if err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if r.autoExposed[3000] {
		t.Fatal("port 3000 should no longer be tracked after a 404 on delete")
	}
}

// State-change logging: one line on entering backoff, one on entering the
// 401 slow mode, one on recovery; none for repeated failures.
func TestRun_LogsOnlyOnStateChange(t *testing.T) {
	errs, infos := countLogs(t)
	listErrs := append(repeatErr(errHub500, 3), repeatErr(errHub401, 3)...)
	client := &scriptedHubClient{listErrs: listErrs}
	r := newLoopReconciler(client)

	got := runLoop(t, r, 8)
	want := secs(3, 6, 12, 300, 300, 300, 3, 3)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if *errs != 2 || *infos != 1 {
		t.Fatalf("log lines: errors = %d, infos = %d; want 2 and 1", *errs, *infos)
	}
}
