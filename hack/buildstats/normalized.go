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
	"fmt"
	"io"
)

// Normalized expresses peak RSS and compile time per unit of code in the
// measured package, so that a package that grows between two gates does not
// hide a real improvement (or fake one when it shrinks).
//
// Lines and funcs come from the compiler's -bench output for the measured
// package (fe:parse lines, be:compilefuncs funcs). The peak is the run's
// largest process, which for a large package is that package's compile.
type Normalized struct {
	Package string `json:"package"`
	Lines   int64  `json:"lines"`
	Funcs   int64  `json:"funcs"`
	// PeakRSSGiBPer100kLines / PeakRSSGiBPer10kFuncs: run peak RSS divided
	// by the measured package's size.
	PeakRSSGiBPer100kLines float64 `json:"peak_rss_gib_per_100k_lines,omitempty"`
	PeakRSSGiBPer10kFuncs  float64 `json:"peak_rss_gib_per_10k_funcs,omitempty"`
	// CompileSecPer100kLines / CompileSecPer10kFuncs: the package's -bench
	// total divided by its size.
	CompileSecPer100kLines float64 `json:"compile_s_per_100k_lines,omitempty"`
	CompileSecPer10kFuncs  float64 `json:"compile_s_per_10k_funcs,omitempty"`
}

// normalize picks the measured package's -bench record (the one with the
// largest total; the test main and an external _test package are small) and
// divides the peak and compile time by its size. It returns nil when there is
// no size to divide by.
func normalize(peakBytes int64, bench []BenchRecord) *Normalized {
	var best *BenchRecord
	for i := range bench {
		if best == nil || bench[i].TotalSec > best.TotalSec {
			best = &bench[i]
		}
	}
	if best == nil {
		return nil
	}
	n := &Normalized{Package: best.Package}
	for _, p := range best.Phases {
		switch {
		case p.Phase == "fe:parse" && p.Unit == "lines":
			n.Lines = p.Count
		case p.Phase == "be:compilefuncs" && p.Unit == "funcs":
			n.Funcs = p.Count
		}
	}
	if n.Lines == 0 && n.Funcs == 0 {
		return nil
	}
	peak := float64(peakBytes) / gib
	if n.Lines > 0 {
		per := float64(n.Lines) / 100_000
		if peakBytes > 0 {
			n.PeakRSSGiBPer100kLines = round3(peak / per)
		}
		n.CompileSecPer100kLines = round3(best.TotalSec / per)
	}
	if n.Funcs > 0 {
		per := float64(n.Funcs) / 10_000
		if peakBytes > 0 {
			n.PeakRSSGiBPer10kFuncs = round3(peak / per)
		}
		n.CompileSecPer10kFuncs = round3(best.TotalSec / per)
	}
	return n
}

func printNormalized(w io.Writer, n *Normalized) {
	fmt.Fprintf(w, "\nnormalised (%s: %d lines, %d funcs): peak %.3f GiB/100k lines, %.3f GiB/10k funcs; compile %.2fs/100k lines, %.2fs/10k funcs\n",
		n.Package, n.Lines, n.Funcs, n.PeakRSSGiBPer100kLines, n.PeakRSSGiBPer10kFuncs, n.CompileSecPer100kLines, n.CompileSecPer10kFuncs)
}
