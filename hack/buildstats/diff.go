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
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
	return diffRecords(stdout, a, b)
}

// metric is one comparable number.
type metric struct {
	name     string
	old, new float64
	unit     string
}

// diffRecords prints comparability warnings and a metric table for the
// sections both records share.
func diffRecords(w io.Writer, a, b *Record) error {
	p := newPrinter(w)
	for _, warn := range comparabilityWarnings(a, b) {
		p.printf("WARNING: %s\n", warn)
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
	ob := benchByKey(a.Bench)
	bk := benchKeys(b.Bench)
	for i, nb := range b.Bench {
		key := bk[i]
		o, ok := ob[key]
		if !ok {
			continue
		}
		add("compile total "+key, o.TotalSec, nb.TotalSec, "s")
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
		p.println("no comparable sections")
		return p.err
	}
	t, done := p.table(tabwriter.AlignRight)
	t.println("old\tnew\tdelta\tdelta %\t metric\t")
	for _, m := range ms {
		pct := "n/a"
		if m.old != 0 {
			pct = fmt.Sprintf("%+.1f%%", 100*(m.new-m.old)/m.old)
		}
		t.printf("%s\t%s\t%s\t%s\t %s\t\n", fmtNum(m.old, m.unit), fmtNum(m.new, m.unit), fmtDelta(m.new-m.old, m.unit), pct, m.name)
	}
	done()
	return p.err
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

// benchKeys names each -bench record by package, adding " (#n)" to the
// second and later records of the same package (for example the plain and
// the internal-test compile of one package), so no record overwrites another.
func benchKeys(recs []BenchRecord) []string {
	seen := map[string]int{}
	keys := make([]string, len(recs))
	for i, r := range recs {
		seen[r.Package]++
		keys[i] = r.Package
		if n := seen[r.Package]; n > 1 {
			keys[i] = fmt.Sprintf("%s (#%d)", r.Package, n)
		}
	}
	return keys
}

func benchByKey(recs []BenchRecord) map[string]BenchRecord {
	m := map[string]BenchRecord{}
	for i, k := range benchKeys(recs) {
		m[k] = recs[i]
	}
	return m
}

// normalizeCommand replaces the parts of a command that legitimately differ
// between two otherwise identical runs: the injected -debug-actiongraph and
// -bench file paths, the -o output path, and the go binary's directory.
func normalizeCommand(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case i == 0:
			a = filepath.Base(a)
		case strings.HasPrefix(a, "-debug-actiongraph="):
			a = "-debug-actiongraph=<path>"
		case a == "-o" && i+1 < len(argv):
			out = append(out, a, "<path>")
			i++
			continue
		case strings.HasPrefix(a, "-o="):
			a = "-o=<path>"
		}
		out = append(out, benchPathRE.ReplaceAllString(a, "-bench=<path>"))
	}
	return out
}

var benchPathRE = regexp.MustCompile(`-bench=\S+`)

// comparabilityWarnings lists settings that differ between the records and
// change compile cost, so a delta is not mistaken for a code effect.
func comparabilityWarnings(a, b *Record) []string {
	var w []string
	if a.Kind != b.Kind {
		w = append(w, fmt.Sprintf("kind differs: %s vs %s", a.Kind, b.Kind))
	}
	if len(a.Command) > 0 && len(b.Command) > 0 {
		na, nb := normalizeCommand(a.Command), normalizeCommand(b.Command)
		if !slices.Equal(na, nb) {
			w = append(w, fmt.Sprintf("command differs:\n  old: %s\n  new: %s",
				truncate(shellJoin(na), maxCommandDisplay), truncate(shellJoin(nb), maxCommandDisplay)))
		}
	}
	for _, r := range []struct {
		side string
		ru   *Rusage
	}{{"old", a.Rusage}, {"new", b.Rusage}} {
		if r.ru != nil && r.ru.ExitCode != 0 {
			w = append(w, fmt.Sprintf("%s run exited with rc=%d; its numbers may describe a partial run", r.side, r.ru.ExitCode))
		}
	}
	if a.Normalized != nil && b.Normalized != nil && a.Normalized.Package != b.Normalized.Package {
		w = append(w, fmt.Sprintf("normalised package differs: %s vs %s", a.Normalized.Package, b.Normalized.Package))
	}
	if ka, kb := sortedKeys(a.Bench), sortedKeys(b.Bench); len(ka) > 0 && len(kb) > 0 && !slices.Equal(ka, kb) {
		w = append(w, fmt.Sprintf("compiler -bench packages differ: %v vs %v", ka, kb))
	}
	if a.Host == nil || b.Host == nil {
		return w
	}
	ha, hb := a.Host, b.Host
	if ha.GoToolchain != hb.GoToolchain {
		w = append(w, fmt.Sprintf("go toolchain differs: %q vs %q", ha.GoToolchain, hb.GoToolchain))
	}
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

func sortedKeys(recs []BenchRecord) []string {
	k := benchKeys(recs)
	slices.Sort(k)
	return k
}
