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

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Rusage is the measured cost of one child command.
type Rusage struct {
	ExitCode int     `json:"exit_code"`
	WallSec  float64 `json:"wall_s"`
	UserSec  float64 `json:"user_s"`
	SysSec   float64 `json:"sys_s"`
	// PeakRSSBytes is ru_maxrss from wait4 on the direct child. The kernel
	// folds in the maximum of every descendant the child waited for, so for
	// `go build` / `go test -c` it is the RSS of the largest single process
	// (normally the biggest compile or the link), not the sum of the tree.
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
	// Cgroup memory.peak before and after the run, for context only. It
	// covers the WHOLE cgroup (page cache, the agent harness and anything
	// else in the container), is monotonic on kernels that cannot reset it,
	// and is NOT comparable to RSS. Do not use it for gates; use
	// PeakRSSBytes. 0 when not readable.
	CgroupPeakBefore int64 `json:"cgroup_peak_before_bytes,omitempty"`
	CgroupPeakAfter  int64 `json:"cgroup_peak_after_bytes,omitempty"`
}

// execOpts controls how the child is started.
type execOpts struct {
	stdout     string // file to receive child stdout; "" = inherit
	cgroupPeak string // memory.peak path; "" = do not read
}

// measure runs argv as a direct child, forwarding SIGINT/SIGTERM/SIGHUP, and
// returns its rusage. A non-zero exit is reported in Rusage.ExitCode, not as
// an error; err is set only if the command could not be run at all.
func measure(argv []string, o execOpts, stderr io.Writer) (*Rusage, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command given after --")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = stderr
	cmd.Stdout = os.Stdout
	if o.stdout != "" {
		f, err := os.Create(o.stdout)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		cmd.Stdout = f
	}

	ru := &Rusage{CgroupPeakBefore: readInt(o.cgroupPeak)}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	waitErr := cmd.Wait()
	close(done)
	ru.WallSec = round1(time.Since(start).Seconds())
	ru.CgroupPeakAfter = readInt(o.cgroupPeak)

	ps := cmd.ProcessState
	if ps == nil {
		return nil, waitErr
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return nil, waitErr
	}
	ru.ExitCode = ps.ExitCode()
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		ru.ExitCode = 128 + int(ws.Signal())
	}
	ru.UserSec = round1(ps.UserTime().Seconds())
	ru.SysSec = round1(ps.SystemTime().Seconds())
	ru.PeakRSSBytes = maxRSSBytes(ps)
	return ru, nil
}

func readInt(path string) int64 {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// defaultCgroupPeak returns the cgroup v2 memory.peak path if readable.
func defaultCgroupPeak() string {
	const p = "/sys/fs/cgroup/memory.peak"
	if readInt(p) > 0 {
		return p
	}
	return ""
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func printRusage(w io.Writer, r *Rusage) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "rc\twall\tuser\tsys\tpeak RSS (largest process)\tcgroup memory.peak (whole cgroup incl. page cache; context only, not for gates)")
	cg := "not captured"
	switch {
	case r.CgroupPeakAfter > r.CgroupPeakBefore && r.CgroupPeakBefore > 0:
		cg = fmt.Sprintf("%s (was %s)", fmtGiB(r.CgroupPeakAfter), fmtGiB(r.CgroupPeakBefore))
	case r.CgroupPeakAfter > 0:
		cg = fmt.Sprintf("unchanged at %s", fmtGiB(r.CgroupPeakAfter))
	}
	fmt.Fprintf(tw, "%d\t%.1fs\t%.1fs\t%.1fs\t%s\t%s\n", r.ExitCode, r.WallSec, r.UserSec, r.SysSec, fmtGiB(r.PeakRSSBytes), cg)
	tw.Flush()
}
