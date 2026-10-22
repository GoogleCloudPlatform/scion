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

// Package settingslockhook is for tests only. It holds the override of how
// long a settings write waits for the settings lock (pkg/config
// LockSettingsDir), the one way to change that wait. It is internal, so
// only in-tree packages can import it, and only tests call Set: production
// code never does (misuse could only change the wait, not skip the lock).
package settingslockhook

import (
	"sync/atomic"
	"time"
)

var wait atomic.Int64 // nanoseconds; 0 means no override

// Set makes settings writes wait at most d for the settings lock, until the
// returned function restores the previous value. For tests only.
func Set(d time.Duration) (restore func()) {
	prev := wait.Swap(int64(d))
	return func() { wait.Store(prev) }
}

// Wait returns the overriding wait, or def when there is none.
func Wait(def time.Duration) time.Duration {
	if d := wait.Load(); d > 0 {
		return time.Duration(d)
	}
	return def
}
