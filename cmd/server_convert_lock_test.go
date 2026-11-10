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

//go:build !windows

package cmd

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
)

func lockHeldElsewhere(t *testing.T, g string) bool {
	t.Helper()
	h, err := brokerconvert.AcquireHostLock(g)
	if err != nil {
		return true
	}
	h.Release()
	return false
}

// Test (a): while a server start holds the lock (from its step 5b take
// until its broker listener is bound), a convert operation refuses.
func TestServerStartLock_ConvertRefusesUntilListening(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)

	listening := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		releaseConvertLockWhenListening(ctx, lock, []<-chan struct{}{listening})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	assert.True(t, lockHeldElsewhere(t, g), "not released before Listening closes")
	_, err = brokerconvert.AcquireHostLock(g)
	assert.ErrorIs(t, err, brokerconvert.ErrHostLockHeld)

	close(listening)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released after Listening closed")
	}
	assert.False(t, lockHeldElsewhere(t, g), "released once the listener is bound")
}

// Test (b): a server start refuses while a convert operation holds the
// lock (the try-lock refuses; it never waits).
func TestServerStartLock_RefusesWhileConvertHoldsIt(t *testing.T) {
	g := t.TempDir()
	h, err := brokerconvert.AcquireHostLock(g)
	require.NoError(t, err)
	defer h.Release()

	start := time.Now()
	_, err = acquireConvertStartLock(g)
	assert.ErrorIs(t, err, brokerconvert.ErrHostLockHeld)
	assert.Less(t, time.Since(start), 2*time.Second, "a refusing try-lock, never a blocking wait")
}

// Test (c): after the bind the lock is released, and convert's P-4 sees
// the running server through its own listener.
func TestServerStartLock_AfterBindP4SeesServer(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	listening := make(chan struct{})
	close(listening)
	releaseConvertLockWhenListening(context.Background(), lock, []<-chan struct{}{listening})

	h, err := brokerconvert.AcquireHostLock(g)
	require.NoError(t, err, "released after the bind")
	defer h.Release()
	port := ln.Addr().(*net.TCPAddr).Port
	assert.True(t, checkPort("127.0.0.1", port).inUse, "P-4 finds the server on its port")
}

// A start whose own listener never binds (it fails) does not release the
// lock from the goroutine: the start's deferred Release frees it as it
// fails, and apply's P-4 still refuses while anything holds the port.
func TestServerStartLock_FailedBindReleasesThroughDefer(t *testing.T) {
	g := t.TempDir()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = busy.Close() }()

	func() {
		lock, err := acquireConvertStartLock(g)
		require.NoError(t, err)
		defer lock.Release() // the start's deferred release

		neverListening := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			releaseConvertLockWhenListening(ctx, lock, []<-chan struct{}{neverListening})
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		assert.True(t, lockHeldElsewhere(t, g))
		cancel() // the start fails and stops
		<-done
		assert.True(t, lockHeldElsewhere(t, g), "the goroutine does not release on a failed bind")
	}()
	assert.False(t, lockHeldElsewhere(t, g), "the deferred release frees it")
	assert.True(t, checkPort("127.0.0.1", busy.Addr().(*net.TCPAddr).Port).inUse, "P-4 still refuses: something holds the port")
}

func TestServerStartLock_NoListenersReleasesAtOnce(t *testing.T) {
	g := t.TempDir()
	lock, err := acquireConvertStartLock(g)
	require.NoError(t, err)
	releaseConvertLockWhenListening(context.Background(), lock, nil)
	assert.False(t, lockHeldElsewhere(t, g))
}
