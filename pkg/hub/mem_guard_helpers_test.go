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
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"sync/atomic"
	"time"
)

// The pkg/hub test binary has been OOM-killed on shared hosts at over 100 GB
// of anonymous RSS. A chunked run of every test on main peaks at about 700 MB
// RSS. startMemGuard makes a runaway fail the run itself, with goroutine
// stacks that name the running test, instead of letting the kernel
// OOM-killer take down the host.
//
// The guard measures process memory, not just the Go heap. On Linux it reads
// resident set size from /proc/self/statm, which includes goroutine stacks,
// cgo allocations (mattn/go-sqlite3) and race-detector shadow memory. It also
// reads the runtime's mapped-and-not-released total, which is the only
// signal on other platforms. Whichever is larger is compared with the cap.
const (
	// memGuardDefaultCap is the process memory at which the binary dumps
	// diagnostics and exits. It is more than 5x the measured healthy peak.
	memGuardDefaultCap = 4 << 30
	// memGuardCapEnv overrides the cap in bytes. Zero disables the guard,
	// including its soft limit.
	memGuardCapEnv = "SCION_HUB_TEST_MEM_CAP_BYTES"
	// memGuardIntervalEnv overrides the sampling interval (a
	// time.ParseDuration string). The regression test uses it.
	memGuardIntervalEnv = "SCION_HUB_TEST_MEM_GUARD_INTERVAL"

	// memGuardMinSoftLimit is the smallest soft limit worth setting. Below
	// it the GC would run almost continuously, so tiny caps get none.
	memGuardMinSoftLimit = 64 << 20

	memGuardDefaultInterval = time.Second
	memGuardMetricTotal     = "/memory/classes/total:bytes"
	memGuardMetricReleased  = "/memory/classes/heap/released:bytes"
)

// memGuardRunning records that startMemGuard started a watchdog, so a test
// can check that the TestMain wiring is in place.
var memGuardRunning atomic.Bool

// startMemGuard starts the memory watchdog and returns a function that
// stops it.
func startMemGuard() (stop func()) {
	limit := uint64(memGuardDefaultCap)
	if v := os.Getenv(memGuardCapEnv); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory guard: ignoring invalid %s=%q: %v\n", memGuardCapEnv, v, err)
		} else {
			limit = n
		}
	}
	if limit == 0 {
		return func() {}
	}
	interval := memGuardDefaultInterval
	if v := os.Getenv(memGuardIntervalEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "memory guard: ignoring invalid %s=%q\n", memGuardIntervalEnv, v)
		} else {
			interval = d
		}
	}

	// A soft limit at half the cap makes the GC work harder well before
	// the cap is reached. A caller's own GOMEMLIMIT always wins.
	if os.Getenv("GOMEMLIMIT") == "" && limit/2 >= memGuardMinSoftLimit {
		debug.SetMemoryLimit(int64(limit / 2))
	}

	done := make(chan struct{})
	memGuardRunning.Store(true)
	go runMemGuard(done, os.Stderr, interval, limit, memGuardUsage, memGuardAbort)
	return func() {
		memGuardRunning.Store(false)
		close(done)
	}
}

// runMemGuard calls sample every interval and calls abort once the usage
// exceeds limit. It returns when done is closed, after abort, or when sample
// reports that no signal is available, which it reports to w.
func runMemGuard(done <-chan struct{}, w io.Writer, interval time.Duration, limit uint64,
	sample func() (used uint64, source string, ok bool), abort func(used, limit uint64, source string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		used, source, ok := sample()
		if !ok {
			_, _ = fmt.Fprintln(w, "memory guard: no memory signal available; guard disabled")
			return
		}
		if used > limit {
			abort(used, limit, source)
			return
		}
	}
}

// memGuardUsage returns the larger of the process RSS and the Go runtime's
// mapped memory, and names which one it is. Neither read stops the world.
func memGuardUsage() (uint64, string, bool) {
	var used uint64
	source := ""
	ok := false
	if rss, rssOK := memGuardRSS(); rssOK {
		used, source, ok = rss, "process RSS", true
	}
	sample := []metrics.Sample{{Name: memGuardMetricTotal}, {Name: memGuardMetricReleased}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 && sample[1].Value.Kind() == metrics.KindUint64 {
		// The two metrics are read together, but guard against released
		// exceeding total so a skewed sample cannot wrap to a huge value
		// and abort a healthy run.
		total, released := sample[0].Value.Uint64(), sample[1].Value.Uint64()
		if total >= released {
			if goMem := total - released; goMem > used {
				used, source = goMem, "Go runtime memory"
			}
			ok = true
		}
	}
	return used, source, ok
}

// memGuardRSS reads the resident set size from /proc/self/statm. It reports
// false where procfs is unavailable.
func memGuardRSS() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := bytes.Fields(data)
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

// memGuardAbort reports runaway memory and exits the test binary. It
// deliberately skips TestMain teardown (the isolated HOME, ent test
// databases): cleanup that allocates is the wrong thing to run here, and the
// heap profile is meant to survive.
func memGuardAbort(used, limit uint64, source string) {
	fmt.Fprintf(os.Stderr, "\nmemory guard: %s %d MiB exceeds cap %d MiB (%s); aborting the test binary.\n",
		source, used>>20, limit>>20, memGuardCapEnv)
	if f, err := os.CreateTemp("", "hub-test-mem-*.pprof"); err == nil {
		if err := pprof.WriteHeapProfile(f); err == nil {
			fmt.Fprintf(os.Stderr, "memory guard: heap profile written to %s\n", f.Name())
		}
		_ = f.Close()
	}
	fmt.Fprintln(os.Stderr, "memory guard: goroutine stacks follow; the running test is in them.")
	_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
	os.Exit(3)
}
