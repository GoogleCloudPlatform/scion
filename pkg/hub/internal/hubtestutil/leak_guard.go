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

package hubtestutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// The package-exit leak guard (ptone/scion#3641) catches the class of leak
// that used to surface only when the memory guard (mem_guard.go)
// hit its cap: test stores that are never closed, and servers built with
// New() that are never shut down. Each unclosed *sql.DB keeps a
// database/sql.(*DB).connectionOpener goroutine (and about 3 MiB of
// in-memory SQLite) alive until the binary exits, and each server that is
// never shut down keeps its background loops, and through them the whole
// server graph, reachable.
//
// At package exit, checkPackageLeaks counts the goroutines whose stack
// contains one of LeakGuardSignatures and fails the run when the total is
// above leakGuardMaxGoroutines, printing every offending stack so the
// creating test helper is easy to find (run with
// GODEBUG=tracebackancestors=20 to also see the full creation chain).
//
// Why an absolute threshold bounds the full single-process CI run: these
// goroutines only exit when their owner is closed. A test that leaks one
// leaks it for the rest of the process, so the count at the end of the
// full package is the sum of what every test leaks. Running the package
// in shards partitions the tests, so the full-package count is the sum of
// the per-shard end counts. After the fix, every measured shard ends at 0
// (see the ci-3641 report), so the full package also ends at 0. The
// threshold leaves room for a handful of goroutines still winding down
// after an asynchronous close; it is far below a real regression, which
// shows up as one goroutine per test run of the leaking test.
const (
	// leakGuardMaxGoroutines is the largest tolerated number of goroutines
	// matching LeakGuardSignatures at package exit. Measured after the fix:
	// 0 at the end of every shard (and of the full package). The margin of
	// 5 absorbs stragglers; do not raise it to hide a leak.
	//
	// Sensitivity: one leaked *server* adds about 9 matching goroutines and
	// is always caught. One leaked *store* adds a single connectionOpener
	// (about 3 MiB), so up to 5 leaked stores pass this guard; the memory
	// guard still bounds them. This is a deliberate trade-off against
	// flaking on stragglers, not per-store sensitivity.
	leakGuardMaxGoroutines = 5

	// leakGuardSettle is how long the guard waits for goroutines that are
	// already shutting down (an async Close, a cleanup loop seeing its
	// stop channel) before it counts.
	leakGuardSettle = 3 * time.Second

	// leakGuardReportEnv, when set to a non-empty value, makes the guard
	// always print its counts, even when they are under the threshold.
	leakGuardReportEnv = "SCION_HUB_TEST_LEAK_REPORT"

	// leakGuardDisableEnv, when set to a non-empty value, turns the guard
	// off (it still reports if leakGuardReportEnv is set).
	leakGuardDisableEnv = "SCION_HUB_TEST_LEAK_GUARD_OFF"
)

// LeakGuardSignatures are the stack substrings that identify a leaked
// store or server background goroutine.
var LeakGuardSignatures = []string{
	// One per open *sql.DB: an unclosed test store.
	"database/sql.(*DB).connectionOpener",
	// Background loops started by New() and stopped by Server.Shutdown.
	"hub.(*chatLinkService).cleanupLoop",
	"hub.(*PreviewService).cleanupNonces",
	"hub.(*NonceCache).cleanup",
	// OIDC key cleanup/refresh loops, which run on the server-lifetime
	// context and stop on Shutdown (ptone/scion#3641).
	"hub.(*OIDCKeyManager).Start",
}

// LeakGuardStoreSignature is the LeakGuardSignatures entry for an unclosed
// store; every other entry is a server background loop.
const LeakGuardStoreSignature = "database/sql.(*DB).connectionOpener"

// LeakGuardResult is what checkPackageLeaks found.
type LeakGuardResult struct {
	counts map[string]int
	Total  int
	stacks []string
	// ids holds the goroutine ID of every counted goroutine.
	IDs map[string]bool
}

// ScanLeakedGoroutines returns the live goroutines that match
// LeakGuardSignatures. A goroutine counts once, under the first signature
// it matches. Goroutines whose ID is in exclude are skipped (a block
// whose ID cannot be parsed is never excluded or recorded); Go never
// reuses a goroutine ID within a process, so an excluded ID can only ever
// refer to the goroutine it was taken from.
func ScanLeakedGoroutines(exclude map[string]bool) LeakGuardResult {
	return ScanStacks(allGoroutineStacks(), exclude)
}

// ScanStacks is ScanLeakedGoroutines over a given runtime.Stack dump.
func ScanStacks(stacks []byte, exclude map[string]bool) LeakGuardResult {
	res := LeakGuardResult{counts: map[string]int{}, IDs: map[string]bool{}}
	for _, block := range strings.Split(string(stacks), "\n\n") {
		block = strings.TrimSpace(block)
		id := goroutineID(block)
		if id != "" && exclude[id] {
			continue
		}
		for _, sig := range LeakGuardSignatures {
			if strings.Contains(block, sig) {
				res.counts[sig]++
				res.Total++
				res.stacks = append(res.stacks, block)
				if id != "" {
					res.IDs[id] = true
				}
				break
			}
		}
	}
	sort.Strings(res.stacks)
	return res
}

// goroutineID returns the ID from a stack block's "goroutine N [...]:"
// header, or "" if the block has no such header.
func goroutineID(block string) string {
	rest, ok := strings.CutPrefix(block, "goroutine ")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, " ")
	if !ok {
		return ""
	}
	return id
}

func allGoroutineStacks() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// checkPackageLeaks waits up to settle for leaked goroutines to drop to the
// threshold, then reports to w. It returns false when the count is still
// above max.
func checkPackageLeaks(w io.Writer, max int, settle time.Duration, alwaysReport bool) bool {
	return CheckLeaksExcluding(w, max, settle, alwaysReport, nil)
}

// CheckLeaksExcluding is checkPackageLeaks, ignoring the goroutines whose
// IDs are in exclude (see ScanLeakedGoroutines).
func CheckLeaksExcluding(w io.Writer, max int, settle time.Duration, alwaysReport bool, exclude map[string]bool) bool {
	deadline := time.Now().Add(settle)
	res := ScanLeakedGoroutines(exclude)
	for res.Total > max && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		res = ScanLeakedGoroutines(exclude)
	}
	if alwaysReport || res.Total > max {
		var b bytes.Buffer
		fmt.Fprintf(&b, "leak guard: %d leaked store/server goroutines at package exit (threshold %d, total goroutines %d)\n",
			res.Total, max, runtime.NumGoroutine())
		for _, sig := range LeakGuardSignatures {
			fmt.Fprintf(&b, "  %-40s %d\n", sig, res.counts[sig])
		}
		_, _ = w.Write(b.Bytes())
	}
	if res.Total <= max {
		return true
	}
	_, _ = fmt.Fprintf(w, "FAIL: leak guard: a test leaks an unclosed store (connectionOpener) or a server that is never shut down.\n"+
		"Build servers with newTestHubServer (or register srv.Shutdown in t.Cleanup) and stores with newTestStore.\n"+
		"Offending goroutines:\n\n%s\n", strings.Join(res.stacks, "\n\n"))
	return false
}

// RunLeakGuard is called by TestMain after m.Run. It returns the exit code
// to use: code unchanged when the guard passes or is disabled, 1 when it
// fails a run that otherwise passed.
func RunLeakGuard(code int) int {
	report := os.Getenv(leakGuardReportEnv) != ""
	if os.Getenv(leakGuardDisableEnv) != "" {
		if report {
			checkPackageLeaks(os.Stderr, int(^uint(0)>>1), 0, true)
		}
		return code
	}
	if !checkPackageLeaks(os.Stderr, leakGuardMaxGoroutines, leakGuardSettle, report) && code == 0 {
		return 1
	}
	return code
}
