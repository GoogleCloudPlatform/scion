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

//go:build !no_sqlite

package hub

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Server fields written under s.mu by a startup setter and read by
// CleanupResources, or written after serving starts and read by handlers,
// are read under s.mu.RLock (ptone/scion#1374). These tests drive the
// writer and the reader concurrently. They check for deadlock and panics in
// a normal run; under -race they also report an unguarded read.

// TestStartBackgroundServices_ConcurrentWithCleanupResources covers combined
// mode, where the CleanupResources goroutine is already waiting on ctx when
// StartBackgroundServices writes s.scheduler and starts the notification
// dispatcher and lifecycle hook evaluator.
func TestStartBackgroundServices_ConcurrentWithCleanupResources(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	srv, err := newTestHubServer(t, DefaultServerConfig(), s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.StartBackgroundServices(ctx)
	}()
	go func() {
		defer wg.Done()
		_ = srv.CleanupResources(context.Background())
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("StartBackgroundServices and CleanupResources did not both return (lock ordering)")
	}

	// If CleanupResources ran first, the scheduler started afterwards is
	// still running; stop it so the test leaves nothing behind.
	cancel()
	srv.mu.RLock()
	sched := srv.scheduler
	srv.mu.RUnlock()
	if sched == nil {
		t.Fatal("StartBackgroundServices did not set the scheduler")
	}
	sched.Stop()
}

// TestSetLocalImageChecker_ConcurrentWithGetImageManager covers the runtime
// reload path, which calls SetLocalImageChecker after serving has started
// while image handlers read the image manager.
func TestSetLocalImageChecker_ConcurrentWithGetImageManager(t *testing.T) {
	srv, _ := setupImageStatusTest(t)

	first := &fakeImageManager{exists: map[string]bool{}}
	second := &fakeImageManager{exists: map[string]bool{}}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				srv.SetLocalImageChecker(first)
			} else {
				srv.SetLocalImageChecker(second)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if mgr := srv.getImageManager(); mgr != nil && mgr != imageManager(first) && mgr != imageManager(second) {
			t.Fatalf("getImageManager returned an unexpected value %v", mgr)
		}
	}
	wg.Wait()

	if got := srv.getImageManager(); got != imageManager(second) {
		t.Fatalf("getImageManager = %v, want the last manager set", got)
	}
}
