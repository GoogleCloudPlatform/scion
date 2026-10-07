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

package hub

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
// that used to surface only when the memory guard (mem_guard_helpers_test.go)
// hit its cap: test stores that are never closed, and servers built with
// New() that are never shut down. Each unclosed *sql.DB keeps a
// database/sql.(*DB).connectionOpener goroutine (and about 3 MiB of
// in-memory SQLite) alive until the binary exits, and each server that is
// never shut down keeps its background loops, and through them the whole
// server graph, reachable.
//
// At package exit, checkPackageLeaks counts the goroutines whose stack
// contains one of leakGuardSignatures and fails the run when the total is
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
	// matching leakGuardSignatures at package exit. Measured after the fix:
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

// leakGuardSignatures are the stack substrings that identify a leaked
// store or server background goroutine.
var leakGuardSignatures = []string{
	// One per open *sql.DB: an unclosed test store.
	"database/sql.(*DB).connectionOpener",
	// Background loops started by New() and stopped by Server.Shutdown.
	"hub.(*StoreDecisionAuditEmitter).next",
	"hub.(*chatLinkService).cleanupLoop",
	"hub.(*PreviewService).cleanupNonces",
	"hub.(*NonceCache).cleanup",
	// OIDC key cleanup/refresh loops, which run on the server-lifetime
	// context and stop on Shutdown (ptone/scion#3641).
	"hub.(*OIDCKeyManager).Start",
}

// leakGuardStoreSignature is the leakGuardSignatures entry for an unclosed
// store; every other entry is a server background loop.
const leakGuardStoreSignature = "database/sql.(*DB).connectionOpener"

// leakGuardResult is what checkPackageLeaks found.
type leakGuardResult struct {
	counts map[string]int
	total  int
	stacks []string
}

// scanLeakedGoroutines returns the live goroutines that match
// leakGuardSignatures. A goroutine counts once, under the first signature
// it matches.
func scanLeakedGoroutines() leakGuardResult {
	res := leakGuardResult{counts: map[string]int{}}
	for _, block := range strings.Split(string(allGoroutineStacks()), "\n\n") {
		block = strings.TrimSpace(block)
		for _, sig := range leakGuardSignatures {
			if strings.Contains(block, sig) {
				res.counts[sig]++
				res.total++
				res.stacks = append(res.stacks, block)
				break
			}
		}
	}
	sort.Strings(res.stacks)
	return res
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
	deadline := time.Now().Add(settle)
	res := scanLeakedGoroutines()
	for res.total > max && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		res = scanLeakedGoroutines()
	}
	if alwaysReport || res.total > max {
		var b bytes.Buffer
		fmt.Fprintf(&b, "leak guard: %d leaked store/server goroutines at package exit (threshold %d, total goroutines %d)\n",
			res.total, max, runtime.NumGoroutine())
		for _, sig := range leakGuardSignatures {
			fmt.Fprintf(&b, "  %-40s %d\n", sig, res.counts[sig])
		}
		_, _ = w.Write(b.Bytes())
	}
	if res.total <= max {
		return true
	}
	fmt.Fprintf(w, "FAIL: leak guard: a test leaks an unclosed store (connectionOpener) or a server that is never shut down.\n"+
		"Build servers with newTestHubServer (or register srv.Shutdown in t.Cleanup) and stores with newTestStore.\n"+
		"Offending goroutines:\n\n%s\n", strings.Join(res.stacks, "\n\n"))
	return false
}

// runLeakGuard is called by TestMain after m.Run. It returns the exit code
// to use: code unchanged when the guard passes or is disabled, 1 when it
// fails a run that otherwise passed.
func runLeakGuard(code int) int {
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
