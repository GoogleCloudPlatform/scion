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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// agAction is the subset of a cmd/go -debug-actiongraph entry we use.
type agAction struct {
	ID        int       `json:"ID"`
	Mode      string    `json:"Mode"`
	Package   string    `json:"Package"`
	TimeStart time.Time `json:"TimeStart"`
	TimeDone  time.Time `json:"TimeDone"`
	Cmd       []string  `json:"Cmd"`
	// CmdReal/CmdUser/CmdSys are time.Duration (ns) for actions that ran a
	// tool (compile, link, vet); absent for cache hits.
	CmdReal int64 `json:"CmdReal"`
	CmdUser int64 `json:"CmdUser"`
	CmdSys  int64 `json:"CmdSys"`
}

// ActiongraphSummary is the per-action timing breakdown of one build.
type ActiongraphSummary struct {
	Actions int            `json:"actions"`
	Timed   int            `json:"timed"`
	ByMode  map[string]int `json:"timed_by_mode"`
	// RanTool counts actions that actually executed a tool (Cmd set), i.e.
	// cache misses. A warm-cache rebuild of an unchanged tree has few.
	RanTool int `json:"ran_tool"`
	// BuildOver1s / BuildOver1sSec: build (compile) actions longer than 1s.
	BuildOver1s    int     `json:"build_actions_over_1s"`
	BuildOver1sSec float64 `json:"build_actions_over_1s_sum_s"`
	// BuildSec / LinkSec: summed wall of all build and link actions.
	BuildSec float64    `json:"build_sum_s"`
	LinkSec  float64    `json:"link_sum_s"`
	Top      []AGAction `json:"top"`
}

// AGAction is one row of the top-N table.
type AGAction struct {
	Mode    string  `json:"mode"`
	Package string  `json:"package"`
	WallSec float64 `json:"wall_s"`
	UserSec float64 `json:"user_s,omitempty"`
	SysSec  float64 `json:"sys_s,omitempty"`
}

func parseActiongraph(r io.Reader) ([]agAction, error) {
	var acts []agAction
	if err := json.NewDecoder(r).Decode(&acts); err != nil {
		return nil, fmt.Errorf("decode actiongraph: %w", err)
	}
	return acts, nil
}

// minTopWall is the wall time below which an action that ran no tool is
// left out of the top table.
const minTopWall = 0.05

func summarizeActiongraph(acts []agAction, top int) *ActiongraphSummary {
	s := &ActiongraphSummary{Actions: len(acts), ByMode: map[string]int{}}
	var rows []AGAction
	for _, a := range acts {
		if len(a.Cmd) > 0 {
			s.RanTool++
		}
		if a.TimeStart.IsZero() || a.TimeDone.IsZero() {
			continue
		}
		s.Timed++
		s.ByMode[a.Mode]++
		wall := a.TimeDone.Sub(a.TimeStart).Seconds()
		switch a.Mode {
		case "build":
			s.BuildSec += wall
			if wall > 1 {
				s.BuildOver1s++
				s.BuildOver1sSec += wall
			}
		case "link":
			s.LinkSec += wall
		}
		// Top rows are actions that did work: ran a tool (cache miss) or
		// took measurable time. Cache checks would otherwise pad the table.
		if len(a.Cmd) == 0 && wall < minTopWall {
			continue
		}
		rows = append(rows, AGAction{
			Mode:    a.Mode,
			Package: a.Package,
			WallSec: wall,
			UserSec: time.Duration(a.CmdUser).Seconds(),
			SysSec:  time.Duration(a.CmdSys).Seconds(),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].WallSec != rows[j].WallSec {
			return rows[i].WallSec > rows[j].WallSec
		}
		if rows[i].Package != rows[j].Package {
			return rows[i].Package < rows[j].Package
		}
		return rows[i].Mode < rows[j].Mode
	})
	if top >= 0 && len(rows) > top {
		rows = rows[:top]
	}
	for i := range rows {
		rows[i].WallSec = round1(rows[i].WallSec)
		rows[i].UserSec = round1(rows[i].UserSec)
		rows[i].SysSec = round1(rows[i].SysSec)
	}
	s.Top = rows
	s.BuildSec = round1(s.BuildSec)
	s.LinkSec = round1(s.LinkSec)
	s.BuildOver1sSec = round1(s.BuildOver1sSec)
	return s
}

func summarizeActiongraphFile(path string, top int) (*ActiongraphSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	acts, err := parseActiongraph(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return summarizeActiongraph(acts, top), nil
}

func printActiongraph(w io.Writer, s *ActiongraphSummary) {
	fmt.Fprintf(w, "\nactiongraph: %d actions, %d timed, %d ran a tool; build sum %.1fs, link sum %.1fs; build actions >1s: %d (%.1fs)\n",
		s.Actions, s.Timed, s.RanTool, s.BuildSec, s.LinkSec, s.BuildOver1s, s.BuildOver1sSec)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "wall\tuser\tsys\t mode\t package\t")
	for _, r := range s.Top {
		fmt.Fprintf(tw, "%.1fs\t%.1fs\t%.1fs\t %s\t %s\t\n", r.WallSec, r.UserSec, r.SysSec, r.Mode, r.Package)
	}
	tw.Flush()
}
