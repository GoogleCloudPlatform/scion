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

// scriptedHubClient answers ListPorts and RegisterPort from per-call error
// scripts; once a script runs out, calls succeed.
type scriptedHubClient struct {
	mu          sync.Mutex
	listErrs    []error
	registerErr error
	listCalls   int
	regCalls    int
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
	return nil, nil
}

func (c *scriptedHubClient) RegisterPort(context.Context, scionhub.RegisterPortRequest) (*scionhub.ExposedPort, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regCalls++
	return nil, c.registerErr
}

func (c *scriptedHubClient) DeletePort(context.Context, int) error { return nil }

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
)

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
