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

// Package writesonread holds the process-wide switch that read-only
// commands turn on so that read paths do not write. While it is set, the
// settings loaders keep a legacy key's value in memory instead of rewriting
// the file (no lock file, backup or rewrite), the legacy global layout is
// not migrated, and a git workspace record is not healed. The hidden
// "scion server preflight" command sets it: it runs the newly built binary
// against the running hub's files, which it must leave untouched.
//
// For read-only commands only; read paths must not write while it is set.
// Set is never undone in production. IsolateForTest is for tests only. The
// package is internal, so only packages in this module can import it.
package writesonread

import "sync/atomic"

var disabled atomic.Bool

// Set turns the switch on for the rest of the process.
func Set() { disabled.Store(true) }

// Disabled reports whether the switch is on: read paths must not write.
func Disabled() bool { return disabled.Load() }

// TB is the part of testing.TB that IsolateForTest needs.
type TB interface {
	Helper()
	Cleanup(func())
}

// IsolateForTest turns the switch off for t, and when t ends restores the
// value it had before, so a test that sets it does not hand it to the next
// test, and a nested isolate in a subtest does not clear its parent's Set.
// For tests only: production code never calls it
// (TestIsolateForTest_NoProductionCallers).
func IsolateForTest(t TB) {
	t.Helper()
	prev := disabled.Swap(false)
	t.Cleanup(func() { disabled.Store(prev) })
}
