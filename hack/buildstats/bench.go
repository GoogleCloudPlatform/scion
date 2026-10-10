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
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
)

// BenchRecord is one compiler invocation's -bench output. The compiler
// appends one block per invocation to the same file, so a `go test -c`
// whose -gcflags pattern also reaches the generated test main yields two
// records: the package itself and "main".
type BenchRecord struct {
	Package  string       `json:"package"`
	TotalSec float64      `json:"total_s"`
	Phases   []BenchPhase `json:"phases"`
}

// BenchPhase is one BenchmarkCompile line, e.g. "be:compilefuncs".
type BenchPhase struct {
	Phase   string  `json:"phase"`
	Seconds float64 `json:"s"`
	Percent float64 `json:"pct"`
	// Count/Unit carry the optional throughput figure ("805772 lines",
	// "379120 funcs").
	Count int64  `json:"count,omitempty"`
	Unit  string `json:"unit,omitempty"`
}

// parseBench parses the text written by `compile -bench=FILE`:
//
//	BenchmarkCompile:<pkg>:fe:parse  1  12317403 ns/op  14.16 %  811 lines  65842 lines/s
//
// The package path may itself contain colons only in theory; phase names
// never do, so the phase is taken from the known fe:/be:/total suffix.
func parseBench(r io.Reader) ([]BenchRecord, error) {
	var recs []BenchRecord
	var cur *BenchRecord
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "commit:") {
			cur = nil // new invocation
			continue
		}
		if !strings.HasPrefix(line, "BenchmarkCompile:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "ns/op" {
			return nil, fmt.Errorf("malformed bench line: %q", line)
		}
		name := strings.TrimPrefix(f[0], "BenchmarkCompile:")
		pkg, phase := splitBenchName(name)
		ns, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bench line %q: %w", line, err)
		}
		if cur == nil || cur.Package != pkg {
			recs = append(recs, BenchRecord{Package: pkg})
			cur = &recs[len(recs)-1]
		}
		p := BenchPhase{Phase: phase, Seconds: float64(ns) / 1e9}
		if len(f) >= 6 && f[5] == "%" {
			p.Percent, _ = strconv.ParseFloat(f[4], 64)
		}
		if len(f) >= 8 {
			if n, err := strconv.ParseInt(f[6], 10, 64); err == nil {
				p.Count, p.Unit = n, f[7]
			}
		}
		if phase == "total" {
			cur.TotalSec = round3(p.Seconds)
		}
		p.Seconds = round3(p.Seconds)
		cur.Phases = append(cur.Phases, p)
	}
	return recs, sc.Err()
}

func splitBenchName(name string) (pkg, phase string) {
	if strings.HasSuffix(name, ":total") {
		return strings.TrimSuffix(name, ":total"), "total"
	}
	for _, sep := range []string{":fe:", ":be:"} {
		if i := strings.LastIndex(name, sep); i >= 0 {
			return name[:i], name[i+1:]
		}
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

func parseBenchFile(path string) ([]BenchRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := parseBench(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return recs, nil
}

func printBench(w io.Writer, recs []BenchRecord) {
	for _, r := range recs {
		fmt.Fprintf(w, "\ncompiler phases: %s (total %.2fs)\n", r.Package, r.TotalSec)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "seconds\t%\t phase\t throughput\t")
		for _, p := range r.Phases {
			if p.Phase == "total" {
				continue
			}
			tp := ""
			if p.Count > 0 {
				tp = fmt.Sprintf("%d %s", p.Count, p.Unit)
				if p.Seconds > 0 {
					tp += fmt.Sprintf(" (%.0f/s)", float64(p.Count)/p.Seconds)
				}
			}
			fmt.Fprintf(tw, "%.2f\t%.1f\t %s\t %s\t\n", p.Seconds, p.Percent, p.Phase, tp)
		}
		tw.Flush()
	}
}
