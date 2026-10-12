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

//go:build unix && !openbsd

package hub

import (
	"fmt"
	"syscall"
)

// addressSpaceLimit reports a finite RLIMIT_AS (as set by ulimit -v). Headless
// Chromium cannot start under one: V8 reserves a very large virtual address
// range at startup, far beyond any practical cap, and the browser aborts
// before the test can talk to it.
func addressSpaceLimit() (string, bool) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &rl); err != nil {
		return "", false
	}
	// Not compared against syscall.RLIM_INFINITY on purpose: it is an
	// untyped negative constant on linux (-1) and solaris (-3), so
	// uint64(syscall.RLIM_INFINITY) does not compile there, and Rlimit.Cur
	// is int64 on freebsd. "No limit" is all ones on linux and 1<<63-1 on
	// darwin and freebsd; no real cap comes anywhere near 1<<62 bytes.
	if rl.Cur >= 1<<62 {
		return "", false
	}
	return fmt.Sprintf("%d MiB", rl.Cur>>20), true
}
