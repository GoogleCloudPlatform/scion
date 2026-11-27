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

package shipper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

const (
	// DefaultLockTimeout bounds how long a shipper operation waits for the
	// exclusive state lock before returning a timeout error.
	DefaultLockTimeout = 10 * time.Second

	lockPollMin = 2 * time.Millisecond
	lockPollMax = 50 * time.Millisecond
)

// acquireStateLock ensures the shipper state directory exists with private
// permissions and acquires the exclusive cross-process lock at state.lock.
// It honors both timeout and context cancellation.
func acquireStateLock(ctx context.Context, homeDir string, timeout time.Duration) (*filelock.Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}

	if err := verifyStateDirAncestors(homeDir); err != nil {
		return nil, err
	}
	stateDir := StateDir(homeDir)
	tree, err := privatefs.OpenTree(stateDir, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return nil, fmt.Errorf("open shipper state dir %s: %w", stateDir, err)
	}
	_ = tree.Close()

	lockPath := LockPath(homeDir)
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	poll := lockPollMin
	for {
		lock, err := filelock.Acquire(lockPath, 0o600, 0)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrTimeout) {
			return nil, fmt.Errorf("acquire shipper lock %s: %w", lockPath, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("acquire shipper lock %s: %w", lockPath, filelock.ErrTimeout)
		}
		wait := min(poll, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		poll = min(poll*2, lockPollMax)
	}
}
