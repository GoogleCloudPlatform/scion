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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// tjEvent is one test2json event (go test -json / go tool test2json).
type tjEvent struct {
	Action  string   `json:"Action"`
	Package string   `json:"Package"`
	Test    string   `json:"Test"`
	Elapsed *float64 `json:"Elapsed"`
}

// TestSummary summarises one package's test2json events.
type TestSummary struct {
	Package string `json:"package"`
	// Result is the package-level action: pass, fail, skip, or "" when the
	// stream ended without one (killed, timed out, truncated).
	Result     string  `json:"result"`
	ElapsedSec float64 `json:"elapsed_s"`
	// TopLevel counts top-level tests by final action; subtests are folded
	// into their parent's elapsed time and counted separately in Subtests.
	TopLevel map[string]int `json:"top_level"`
	Subtests int            `json:"subtests"`
	// SumSec is the sum of top-level test elapsed times.
	SumSec   float64  `json:"top_level_sum_s"`
	Over1s   int      `json:"top_level_over_1s"`
	Top20Pct float64  `json:"top20_share_pct"`
	Slowest  []TJTest `json:"slowest"`
}

// TJTest is one row of the slowest-tests table.
type TJTest struct {
	Name    string  `json:"name"`
	Action  string  `json:"action"`
	Seconds float64 `json:"s"`
}

// parseTest2JSON reads a test2json stream. Lines that are not JSON (build
// output interleaved by go test -json, or a raw "ok" line) are ignored.
// Packages are returned in order of first appearance.
func parseTest2JSON(r io.Reader, top int) ([]TestSummary, error) {
	type pkgState struct {
		sum      *TestSummary
		tests    map[string]TJTest
		subtests map[string]bool
	}
	var order []string
	pkgs := map[string]*pkgState{}
	get := func(name string) *pkgState {
		p, ok := pkgs[name]
		if !ok {
			p = &pkgState{
				sum:      &TestSummary{Package: name, TopLevel: map[string]int{}},
				tests:    map[string]TJTest{},
				subtests: map[string]bool{},
			}
			pkgs[name] = p
			order = append(order, name)
		}
		return p
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e tjEvent
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Action {
		case "pass", "fail", "skip":
		default:
			continue
		}
		p := get(e.Package)
		el := 0.0
		if e.Elapsed != nil {
			el = *e.Elapsed
		}
		switch {
		case e.Test == "":
			p.sum.Result = e.Action
			p.sum.ElapsedSec = el
		case strings.Contains(e.Test, "/"):
			p.subtests[e.Test] = true
		default:
			p.tests[e.Test] = TJTest{Name: e.Test, Action: e.Action, Seconds: el}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make([]TestSummary, 0, len(order))
	for _, name := range order {
		p := pkgs[name]
		s := p.sum
		s.Subtests = len(p.subtests)
		all := make([]TJTest, 0, len(p.tests))
		for _, t := range p.tests {
			all = append(all, t)
			s.TopLevel[t.Action]++
			s.SumSec += t.Seconds
			if t.Seconds > 1 {
				s.Over1s++
			}
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Seconds != all[j].Seconds {
				return all[i].Seconds > all[j].Seconds
			}
			return all[i].Name < all[j].Name
		})
		var top20 float64
		for i := 0; i < len(all) && i < 20; i++ {
			top20 += all[i].Seconds
		}
		if s.SumSec > 0 {
			s.Top20Pct = round1(100 * top20 / s.SumSec)
		}
		if top >= 0 && len(all) > top {
			all = all[:top]
		}
		s.Slowest = all
		s.SumSec = round1(s.SumSec)
		out = append(out, *s)
	}
	return out, nil
}

func parseTest2JSONFile(path string, top int) ([]TestSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := parseTest2JSON(f, top)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func printTests(w io.Writer, s *TestSummary) {
	res := s.Result
	if res == "" {
		res = "incomplete"
	}
	var counts []string
	for _, a := range []string{"pass", "fail", "skip"} {
		if n := s.TopLevel[a]; n > 0 {
			counts = append(counts, fmt.Sprintf("%s=%d", a, n))
		}
	}
	fmt.Fprintf(w, "\ntests: %s %s elapsed=%.1fs top-level=%d (%s) subtests=%d sum=%.1fs >1s=%d top20 share=%.0f%%\n",
		s.Package, res, s.ElapsedSec, sumInts(s.TopLevel), strings.Join(counts, " "),
		s.Subtests, s.SumSec, s.Over1s, s.Top20Pct)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	for _, t := range s.Slowest {
		fmt.Fprintf(tw, "%.2fs\t %s\t %s\t\n", t.Seconds, t.Action, t.Name)
	}
	tw.Flush()
}

func sumInts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
