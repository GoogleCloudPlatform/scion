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

package ruleengine

// Test-only hooks for deterministic state-machine interleaving tests (ptone/scion#9801, A-4).

// HookLockRun locks r.mu and returns its unlock function.
func HookLockRun(r *ChainRun) func() { r.mu.Lock(); return r.mu.Unlock }

// HookStateLocked returns r.state while the caller holds r.mu (via HookLockRun).
func HookStateLocked(r *ChainRun) int { return int(r.state) }

// HookState returns r.state under r.mu.
func HookState(r *ChainRun) int { r.mu.Lock(); defer r.mu.Unlock(); return int(r.state) }

// HookHasInflight reports whether r.inflight is non-nil under r.mu.
func HookHasInflight(r *ChainRun) bool { r.mu.Lock(); defer r.mu.Unlock(); return r.inflight != nil }

var (
	HookStatePending    = int(chainStatePending)
	HookStateDispatched = int(chainStateDispatched)
)
