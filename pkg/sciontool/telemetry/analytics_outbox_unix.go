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

//go:build unix

package telemetry

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// lock takes the outbox's exclusive flock, waiting up to analyticsLockWait.
func (o *AnalyticsOutbox) lock() (func(), error) {
	f, _, err := o.openCreateOwned(o.path("lock"), os.O_RDWR)
	if err != nil {
		return nil, err
	}
	// A blocking flock queues in the kernel instead of polling, so a
	// waiter is not starved by peers that re-take the lock immediately. It
	// runs in a goroutine so the wait stays bounded. Ownership of the lock
	// and the file is handed over under mu: exactly one side releases them
	// -- the caller if the result was delivered, the goroutine if the wait
	// was abandoned first.
	const (
		waiting = iota
		delivered
		abandoned
	)
	var mu sync.Mutex
	state := waiting
	done := make(chan error, 1) // buffered: the goroutine never blocks on send
	go func() {
		var err error
		for {
			err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if o.lockAcquiredHook != nil {
			o.lockAcquiredHook()
		}
		mu.Lock()
		if state == abandoned {
			mu.Unlock()
			if err == nil {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			_ = f.Close()
			return
		}
		state = delivered
		mu.Unlock()
		if o.lockDeliveredHook != nil {
			o.lockDeliveredHook()
		}
		done <- err
	}()
	wait := o.lockWait
	if wait <= 0 {
		wait = analyticsLockWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("analytics outbox: locking: %w", err)
		}
	case <-timer.C:
		if o.lockTimeoutHook != nil {
			o.lockTimeoutHook()
		}
		mu.Lock()
		if state == delivered {
			// The goroutine handed over just before the timeout: the
			// result is (or is about to be) in done, and it is ours to
			// release.
			mu.Unlock()
			if err := <-done; err == nil {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
			_ = f.Close()
			return nil, ErrAnalyticsOutboxLocked
		}
		state = abandoned // the goroutine releases when it gets the lock
		mu.Unlock()
		return nil, ErrAnalyticsOutboxLocked
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// linksUnsupported reports whether a link error means the filesystem does
// not support hard links here (the only errors linkNoReplace falls back on).
func linksUnsupported(err error) bool {
	for _, e := range []error{unix.EPERM, unix.EXDEV, unix.ENOTSUP, unix.EOPNOTSUPP, unix.EMLINK, unix.ENOSYS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
