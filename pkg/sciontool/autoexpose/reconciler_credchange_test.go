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
	"sync"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// The production hub client reports credential changes to the reconciler.
var _ credentialNotifier = (*scionhub.Client)(nil)

// notifyingHubClient answers ListPorts through list and reports credential
// changes from a real hub client, whose SetToken closes the channel.
type notifyingHubClient struct {
	creds *scionhub.Client

	mu        sync.Mutex
	listCalls int
	list      func(call int) ([]scionhub.ExposedPort, error)
	reached   chan struct{}
	stopAt    int
}

func (c *notifyingHubClient) CredentialChanged() <-chan struct{} {
	return c.creds.CredentialChanged()
}

func (c *notifyingHubClient) ListPorts(ctx context.Context) ([]scionhub.ExposedPort, error) {
	c.mu.Lock()
	c.listCalls++
	call := c.listCalls
	c.mu.Unlock()
	if call == c.stopAt {
		close(c.reached)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.list(call)
}

func (c *notifyingHubClient) RegisterPort(context.Context, scionhub.RegisterPortRequest) (*scionhub.ExposedPort, error) {
	return nil, errors.New("unexpected RegisterPort")
}

func (c *notifyingHubClient) DeletePort(context.Context, int) error {
	return errors.New("unexpected DeletePort")
}

func (c *notifyingHubClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCalls
}

// runNeverFiring runs r.Run with a delay timer that never fires, until the
// client's stopAt call is reached (or fails after a real-time bound).
func runNeverFiring(t *testing.T, r *Reconciler, c *notifyingHubClient, onWait func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.after = func(time.Duration) <-chan time.Time {
		if onWait != nil {
			onWait()
		}
		return nil
	}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-c.reached:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatalf("ListPorts called %d times, want %d: the loop did not wake", c.calls(), c.stopAt)
	}
	cancel()
	<-done
}

func newNotifyingClient(list func(call int) ([]scionhub.ExposedPort, error)) *notifyingHubClient {
	return &notifyingHubClient{
		creds:   scionhub.NewClientWithConfig("http://127.0.0.1:1", "old-credential", "agent-1"),
		list:    list,
		reached: make(chan struct{}),
		stopAt:  2,
	}
}

var errUnauthorized = &scionhub.StatusError{StatusCode: http.StatusUnauthorized}

// A credential change during the refused request wakes the 401 wait.
func TestRun_UnauthorizedWakesOnCredentialChangeDuringRequest(t *testing.T) {
	var c *notifyingHubClient
	c = newNotifyingClient(func(int) ([]scionhub.ExposedPort, error) {
		c.creds.SetToken("new-credential")
		return nil, errUnauthorized
	})
	r := newLoopReconciler(c)
	runNeverFiring(t, r, c, nil)
}

// A credential change after the loop parked on a 401 wakes it.
func TestRun_UnauthorizedWakesOnLaterCredentialChange(t *testing.T) {
	c := newNotifyingClient(func(int) ([]scionhub.ExposedPort, error) {
		return nil, errUnauthorized
	})
	r := newLoopReconciler(c)
	runNeverFiring(t, r, c, func() { go c.creds.SetToken("new-credential") })
}

// A credential change does not cut a backoff for a failure other than 401:
// the loop must take the (unbuffered) backoff timer before its next pass,
// so a loop woken by the change starts the next pass first and is caught.
func TestRun_CredentialChangeDoesNotCutOtherBackoff(t *testing.T) {
	var c *notifyingHubClient
	c = newNotifyingClient(func(int) ([]scionhub.ExposedPort, error) {
		c.creds.SetToken("new-credential")
		return nil, &scionhub.StatusError{StatusCode: http.StatusServiceUnavailable}
	})
	c.stopAt = -1 // never block in ListPorts
	r := newLoopReconciler(c)
	timer := make(chan time.Time)
	waiting := make(chan struct{})
	nextPass := make(chan struct{})
	waits := 0
	r.after = func(time.Duration) <-chan time.Time {
		waits++
		switch waits {
		case 1:
			close(waiting)
			return timer
		case 2:
			close(nextPass)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	<-waiting
	select {
	case timer <- time.Time{}:
	case <-nextPass:
		t.Fatal("a credential change cut a non-401 backoff")
	case <-time.After(10 * time.Second):
		t.Fatal("the loop neither took the backoff timer nor ran another pass")
	}
	select {
	case <-nextPass:
	case <-time.After(10 * time.Second):
		t.Fatal("no next pass after the backoff timer fired")
	}
	cancel()
	<-done
}
