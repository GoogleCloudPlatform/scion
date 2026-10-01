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
	"testing"
	"time"
)

func TestLaunchRegistry_BeginFirstRecord_NoSupersededDone(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p1", Slug: "agent-1"}
	rec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})

	if done := r.Begin(key, rec); done != nil {
		t.Fatalf("expected no superseded done channel for the first record, got %v", done)
	}
}

// TestLaunchRegistry_BeginReplacesAndCancelsOld covers design §3.8.1: "Begin
// with a new ID for a key that is already held cancels the old record
// locally. There is no 409."
func TestLaunchRegistry_BeginReplacesAndCancelsOld(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p1", Slug: "agent-1"}

	oldCancelled := make(chan struct{})
	oldRec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() { close(oldCancelled) })
	if done := r.Begin(key, oldRec); done != nil {
		t.Fatalf("expected nil superseded done for the first record")
	}

	newRec := newLaunchRecord("L2", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	supersededDone := r.Begin(key, newRec)
	if supersededDone == nil {
		t.Fatalf("expected the old record's done channel")
	}

	select {
	case <-oldCancelled:
	case <-time.After(time.Second):
		t.Fatal("Begin did not locally cancel the superseded record")
	}

	// The superseded record's done channel is not closed until its own
	// cleanup finishes (simulated here by Finish).
	select {
	case <-supersededDone:
		t.Fatal("superseded done closed before Finish")
	default:
	}
	r.Finish(key, oldRec)
	select {
	case <-supersededDone:
	default:
		t.Fatal("superseded done not closed after Finish")
	}
}

func TestLaunchRegistry_CancelLocal_WakesTheHeldRecord(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p1", Slug: "agent-1"}

	cancelled := make(chan struct{})
	rec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() { close(cancelled) })
	r.Begin(key, rec)

	// A key with no held record is a harmless no-op (design: "optimisation
	// only").
	r.CancelLocal(launchKey{ProjectID: "p1", Slug: "no-such-agent"})

	r.CancelLocal(key)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("CancelLocal did not cancel the held record")
	}
}

// TestLaunchRegistry_FinishOnlyRemovesIfStillCurrent covers the case where a
// newer launch has already replaced the record Finish is being called for:
// Finish must not delete the newer record from the map.
func TestLaunchRegistry_FinishOnlyRemovesIfStillCurrent(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p1", Slug: "agent-1"}

	oldRec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	r.Begin(key, oldRec)

	newRec := newLaunchRecord("L2", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	r.Begin(key, newRec)

	// Finish the OLD record after it has already been superseded.
	r.Finish(key, oldRec)

	r.mu.Lock()
	current := r.records[key]
	r.mu.Unlock()
	if current != newRec {
		t.Fatalf("Finish(old) must not remove the newer current record")
	}
}

func TestWaitSuperseded_NilIsNoOp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	WaitSuperseded(ctx, nil) // must return immediately, not block for the ctx timeout
}

func TestWaitSuperseded_ReturnsOnCtxDoneIfNeverClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	never := make(chan struct{})
	start := time.Now()
	WaitSuperseded(ctx, never)
	if time.Since(start) > time.Second {
		t.Fatal("WaitSuperseded did not respect ctx as a backstop")
	}
}

// TestLaunchRegistry_BeginThenWaitSupersededBlocksUntilFinish exercises the
// exact Begin+WaitSuperseded sequence runLaunch uses (design §3.8.2 step
// 5.2, F5): a new launch's marker write must wait for the superseded
// record's own cleanup (Finish) to complete, not just for Begin to return.
func TestLaunchRegistry_BeginThenWaitSupersededBlocksUntilFinish(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p1", Slug: "agent-1"}

	oldRec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	r.Begin(key, oldRec)

	newRec := newLaunchRecord("L2", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	supersededDone := r.Begin(key, newRec)

	waited := make(chan struct{})
	go func() {
		WaitSuperseded(context.Background(), supersededDone)
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("WaitSuperseded returned before the superseded launch finished cleaning up")
	case <-time.After(50 * time.Millisecond):
	}

	r.Finish(key, oldRec)

	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("WaitSuperseded did not return after the superseded launch's Finish")
	}
}

func TestLaunchRecord_SetOwnerHub_PinsOnlyOnce(t *testing.T) {
	rec := newLaunchRecord("L1", "agent-id-1", "create", "", time.Now().Add(time.Minute), func() {})
	if got := rec.OwnerHub(); got != "" {
		t.Fatalf("expected no owner hub initially, got %q", got)
	}
	rec.SetOwnerHub("hub-a")
	rec.SetOwnerHub("hub-b") // must not override the first pin
	if got := rec.OwnerHub(); got != "hub-a" {
		t.Fatalf("expected the first pinned owner hub to stick, got %q", got)
	}
}
