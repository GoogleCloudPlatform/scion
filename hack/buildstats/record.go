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
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// schemaVersion is bumped when a field changes meaning or is removed.
const schemaVersion = 1

// Record is the JSON document written by every subcommand. Sections that a
// subcommand does not produce are omitted.
type Record struct {
	Schema  int       `json:"schema"`
	Kind    string    `json:"kind"`
	Label   string    `json:"label,omitempty"`
	Time    time.Time `json:"time"`
	Host    *HostInfo `json:"host,omitempty"`
	Command []string  `json:"command,omitempty"`

	Rusage      *Rusage             `json:"rusage,omitempty"`
	Actiongraph *ActiongraphSummary `json:"actiongraph,omitempty"`
	Bench       []BenchRecord       `json:"compiler_bench,omitempty"`
	Tests       []TestSummary       `json:"tests,omitempty"`
	Deps        []DepCount          `json:"deps,omitempty"`

	// Artifacts lists the raw files kept next to the record (actiongraph,
	// bench, test2json output), so a later run can re-summarise them.
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// HostInfo captures the settings that change compile cost, so two records
// can be checked for comparability before their numbers are compared.
type HostInfo struct {
	Hostname   string            `json:"hostname,omitempty"`
	GOOS       string            `json:"goos"`
	GOARCH     string            `json:"goarch"`
	NumCPU     int               `json:"num_cpu"`
	CgroupCPU  string            `json:"cgroup_cpu_max,omitempty"`
	RlimitAS   string            `json:"rlimit_as,omitempty"`
	GitHead    string            `json:"git_head,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	ToolGoVers string            `json:"buildstats_go_version"`
}

// envKeys are recorded verbatim when set.
var envKeys = []string{"GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GOFLAGS", "GOCACHE", "GOTOOLCHAIN", "CGO_ENABLED"}

func newRecord(kind, label string, command []string) *Record {
	return &Record{
		Schema:  schemaVersion,
		Kind:    kind,
		Label:   label,
		Time:    time.Now().UTC().Truncate(time.Second),
		Host:    collectHost(),
		Command: command,
	}
}

func collectHost() *HostInfo {
	h := &HostInfo{
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		ToolGoVers: runtime.Version(),
		Env:        map[string]string{},
	}
	h.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		h.CgroupCPU = strings.TrimSpace(string(b))
	}
	h.RlimitAS = rlimitAS()
	for _, k := range envKeys {
		if v, ok := os.LookupEnv(k); ok {
			h.Env[k] = v
		}
	}
	// git is not a go command, so this is safe to run under a go-command
	// queue. Failure (no git, not a repo) just leaves the field empty.
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		h.GitHead = strings.TrimSpace(string(out))
	}
	return h
}

// writeJSON writes rec to path ("-" means w).
func writeJSON(rec *Record, path string, w io.Writer) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "-" {
		_, err = w.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func readRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// printRecord renders every section present in rec as human-readable tables.
func printRecord(w io.Writer, rec *Record) {
	if rec.Label != "" {
		fmt.Fprintf(w, "== %s (%s)\n", rec.Label, rec.Kind)
	}
	if len(rec.Command) > 0 {
		fmt.Fprintf(w, "command: %s\n", truncate(shellJoin(rec.Command), maxCommandDisplay))
	}
	if rec.Host != nil && len(rec.Host.Env) > 0 {
		var parts []string
		for _, k := range envKeys {
			if v, ok := rec.Host.Env[k]; ok {
				parts = append(parts, k+"="+v)
			}
		}
		if rec.Host.RlimitAS != "" {
			parts = append(parts, "rlimit_as="+rec.Host.RlimitAS)
		}
		fmt.Fprintf(w, "env: %s\n", strings.Join(parts, " "))
	}
	if rec.Rusage != nil {
		printRusage(w, rec.Rusage)
	}
	if rec.Actiongraph != nil {
		printActiongraph(w, rec.Actiongraph)
	}
	if len(rec.Bench) > 0 {
		printBench(w, rec.Bench)
	}
	for i := range rec.Tests {
		printTests(w, &rec.Tests[i])
	}
	if len(rec.Deps) > 0 {
		printDeps(w, rec.Deps)
	}
}

const gib = 1 << 30

func fmtGiB(b int64) string {
	if b <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f GiB", float64(b)/gib)
}

// shellJoin renders argv so it can be pasted back into a POSIX shell.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// maxCommandDisplay caps the command shown in tables; the JSON record always
// holds the full argv.
const maxCommandDisplay = 400

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d more bytes; full command in the JSON record)", len(s)-n)
}
