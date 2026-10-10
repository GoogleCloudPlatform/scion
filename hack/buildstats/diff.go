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
	"text/tabwriter"
)

func cmdDiff(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("diff", "OLD.json NEW.json", stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errUsage
	}
	a, err := readRecord(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := readRecord(fs.Arg(1))
	if err != nil {
		return err
	}
	diffRecords(stdout, a, b)
	return nil
}

// metric is one comparable number.
type metric struct {
	name     string
	old, new float64
	unit     string
}

// diffRecords prints comparability warnings and a metric table for the
// sections both records share.
func diffRecords(w io.Writer, a, b *Record) {
	for _, warn := range comparabilityWarnings(a, b) {
		fmt.Fprintf(w, "WARNING: %s\n", warn)
	}
	var ms []metric
	add := func(name string, o, n float64, unit string) {
		ms = append(ms, metric{name, o, n, unit})
	}
	if a.Rusage != nil && b.Rusage != nil {
		add("wall", a.Rusage.WallSec, b.Rusage.WallSec, "s")
		add("user", a.Rusage.UserSec, b.Rusage.UserSec, "s")
		add("sys", a.Rusage.SysSec, b.Rusage.SysSec, "s")
		add("peak RSS", float64(a.Rusage.PeakRSSBytes)/gib, float64(b.Rusage.PeakRSSBytes)/gib, "GiB")
	}
	if a.Actiongraph != nil && b.Actiongraph != nil {
		add("actiongraph build sum", a.Actiongraph.BuildSec, b.Actiongraph.BuildSec, "s")
		add("actiongraph link sum", a.Actiongraph.LinkSec, b.Actiongraph.LinkSec, "s")
		oa, na := topByKey(a.Actiongraph.Top), topByKey(b.Actiongraph.Top)
		for _, r := range a.Actiongraph.Top {
			k := r.Mode + " " + r.Package
			if n, ok := na[k]; ok {
				add(k, oa[k], n, "s")
			}
		}
	}
	ob := benchByPkg(a.Bench)
	for _, nb := range b.Bench {
		o, ok := ob[nb.Package]
		if !ok {
			continue
		}
		add("compile total "+nb.Package, o.TotalSec, nb.TotalSec, "s")
		op := map[string]BenchPhase{}
		for _, p := range o.Phases {
			op[p.Phase] = p
		}
		for _, p := range nb.Phases {
			if p.Phase == "total" {
				continue
			}
			if q, ok := op[p.Phase]; ok {
				add("  "+p.Phase, q.Seconds, p.Seconds, "s")
				if p.Count > 0 && q.Count > 0 {
					add("  "+p.Phase+" "+p.Unit, float64(q.Count), float64(p.Count), "")
				}
			}
		}
	}
	if na, nb := a.Normalized, b.Normalized; na != nil && nb != nil {
		add("lines "+nb.Package, float64(na.Lines), float64(nb.Lines), "")
		add("funcs "+nb.Package, float64(na.Funcs), float64(nb.Funcs), "")
		add("peak GiB per 100k lines", na.PeakRSSGiBPer100kLines, nb.PeakRSSGiBPer100kLines, "ratio")
		add("peak GiB per 10k funcs", na.PeakRSSGiBPer10kFuncs, nb.PeakRSSGiBPer10kFuncs, "ratio")
		add("compile s per 100k lines", na.CompileSecPer100kLines, nb.CompileSecPer100kLines, "s")
		add("compile s per 10k funcs", na.CompileSecPer10kFuncs, nb.CompileSecPer10kFuncs, "s")
	}
	ot := map[string]TestSummary{}
	for _, t := range a.Tests {
		ot[t.Package] = t
	}
	for _, t := range b.Tests {
		o, ok := ot[t.Package]
		if !ok {
			continue
		}
		add("tests elapsed "+t.Package, o.ElapsedSec, t.ElapsedSec, "s")
		add("tests top-level "+t.Package, float64(sumInts(o.TopLevel)), float64(sumInts(t.TopLevel)), "")
		add("tests failed "+t.Package, float64(o.TopLevel["fail"]), float64(t.TopLevel["fail"]), "")
	}
	od := map[string]DepCount{}
	for _, d := range a.Deps {
		od[fmt.Sprint(d.Test, d.Package)] = d
	}
	for _, d := range b.Deps {
		o, ok := od[fmt.Sprint(d.Test, d.Package)]
		if !ok {
			continue
		}
		mode := "deps"
		if d.Test {
			mode = "test deps"
		}
		add(mode+" "+d.Package, float64(o.Total), float64(d.Total), "")
		add(mode+" non-std "+d.Package, float64(o.NonStd), float64(d.NonStd), "")
	}

	if len(ms) == 0 {
		fmt.Fprintln(w, "no comparable sections")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "old\tnew\tdelta\tdelta %\t metric\t")
	for _, m := range ms {
		pct := "n/a"
		if m.old != 0 {
			pct = fmt.Sprintf("%+.1f%%", 100*(m.new-m.old)/m.old)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t %s\t\n", fmtNum(m.old, m.unit), fmtNum(m.new, m.unit), fmtDelta(m.new-m.old, m.unit), pct, m.name)
	}
	tw.Flush()
}

func fmtNum(v float64, unit string) string {
	if unit == "" {
		return fmt.Sprintf("%.0f", v)
	}
	if unit == "ratio" {
		return fmt.Sprintf("%.3f", v)
	}
	if unit == "GiB" {
		return fmt.Sprintf("%.2f%s", v, unit)
	}
	return fmt.Sprintf("%.1f%s", v, unit)
}

func fmtDelta(v float64, unit string) string {
	s := fmtNum(v, unit)
	if v >= 0 {
		s = "+" + s
	}
	return s
}

func topByKey(rows []AGAction) map[string]float64 {
	m := map[string]float64{}
	for _, r := range rows {
		m[r.Mode+" "+r.Package] = r.WallSec
	}
	return m
}

func benchByPkg(recs []BenchRecord) map[string]BenchRecord {
	m := map[string]BenchRecord{}
	for _, r := range recs {
		m[r.Package] = r
	}
	return m
}

// comparabilityWarnings lists settings that differ between the records and
// change compile cost, so a delta is not mistaken for a code effect.
func comparabilityWarnings(a, b *Record) []string {
	var w []string
	if a.Kind != b.Kind {
		w = append(w, fmt.Sprintf("kind differs: %s vs %s", a.Kind, b.Kind))
	}
	if a.Host == nil || b.Host == nil {
		return w
	}
	ha, hb := a.Host, b.Host
	fa, fb := ha.Form, hb.Form
	for _, c := range []struct{ k, a, b string }{
		{"GOMAXPROCS", fa.GOMAXPROCS, fb.GOMAXPROCS},
		{"GOGC", fa.GOGC, fb.GOGC},
		{"GOMEMLIMIT", fa.GOMEMLIMIT, fb.GOMEMLIMIT},
		{"GOFLAGS", fa.GOFLAGS, fb.GOFLAGS},
		{"rlimit_as", fa.RlimitAS, fb.RlimitAS},
	} {
		if c.a != c.b {
			w = append(w, fmt.Sprintf("%s differs: %q vs %q", c.k, c.a, c.b))
		}
	}
	for _, k := range []string{"GOTOOLCHAIN", "CGO_ENABLED"} {
		if ha.Env[k] != hb.Env[k] {
			w = append(w, fmt.Sprintf("%s differs: %q vs %q", k, ha.Env[k], hb.Env[k]))
		}
	}
	if ha.CgroupCPU != hb.CgroupCPU || ha.NumCPU != hb.NumCPU {
		w = append(w, fmt.Sprintf("CPU differs: %d cpus cgroup %q vs %d cpus cgroup %q", ha.NumCPU, ha.CgroupCPU, hb.NumCPU, hb.CgroupCPU))
	}
	return w
}
