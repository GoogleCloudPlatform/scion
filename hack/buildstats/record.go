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
	"path/filepath"
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
	Normalized  *Normalized         `json:"normalized,omitempty"`

	// Artifacts lists the raw files kept next to the record (actiongraph,
	// bench, test2json output), so a later run can re-summarise them.
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// HostInfo captures the settings that change compile cost, so two records
// can be checked for comparability before their numbers are compared.
type HostInfo struct {
	Hostname  string            `json:"hostname,omitempty"`
	GOOS      string            `json:"goos"`
	GOARCH    string            `json:"goarch"`
	NumCPU    int               `json:"num_cpu"`
	CgroupCPU string            `json:"cgroup_cpu_max,omitempty"`
	RlimitAS  string            `json:"rlimit_as,omitempty"`
	GitHead   string            `json:"git_head,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// Form is the memory/concurrency form of the run, always populated
	// ("unset" when a variable is not set) so gates can check that two
	// records were taken the same way.
	Form       RuntimeForm `json:"form"`
	ToolGoVers string      `json:"buildstats_go_version"`
	// GoToolchain is the version of the go toolchain that ran the measured
	// command (not buildstats' own). Source is "bench" (the compiler's
	// -bench "commit:" line, authoritative) or "GOROOT/VERSION" (read from
	// the go binary's GOROOT without running go; a GOTOOLCHAIN switch would
	// not be visible there).
	GoToolchain       string `json:"go_toolchain,omitempty"`
	GoToolchainSource string `json:"go_toolchain_source,omitempty"`
}

// RuntimeForm records the settings that change Go compile time and memory.
// The measured go command inherits buildstats' environment, so these are the
// values it ran with.
type RuntimeForm struct {
	GOMAXPROCS string `json:"GOMAXPROCS"`
	GOGC       string `json:"GOGC"`
	GOMEMLIMIT string `json:"GOMEMLIMIT"`
	GOFLAGS    string `json:"GOFLAGS"`
	RlimitAS   string `json:"rlimit_as"`
}

func (f RuntimeForm) String() string {
	return fmt.Sprintf("GOMAXPROCS=%s GOGC=%s GOMEMLIMIT=%s GOFLAGS=%s rlimit_as=%s", f.GOMAXPROCS, f.GOGC, f.GOMEMLIMIT, f.GOFLAGS, f.RlimitAS)
}

func envOrUnset(k string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return "unset"
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
	h.Form = RuntimeForm{
		GOMAXPROCS: envOrUnset("GOMAXPROCS"),
		GOGC:       envOrUnset("GOGC"),
		GOMEMLIMIT: envOrUnset("GOMEMLIMIT"),
		GOFLAGS:    envOrUnset("GOFLAGS"),
		RlimitAS:   h.RlimitAS,
	}
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

// goBinary returns the go binary a command uses: argv[0] when it is a go
// binary, otherwise "go" from PATH.
func goBinary(argv []string) string {
	if len(argv) > 0 && filepath.Base(argv[0]) == "go" {
		return argv[0]
	}
	return "go"
}

// setToolchain records the go toolchain version from bin's GOROOT/VERSION.
// It never runs the go command (on hosts that queue go commands, buildstats
// itself is the one queued command). Failure leaves the fields empty.
func (h *HostInfo) setToolchain(bin string) {
	root := os.Getenv("GOROOT")
	if root == "" {
		path, err := exec.LookPath(bin)
		if err != nil {
			return
		}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		root = filepath.Dir(filepath.Dir(path)) // GOROOT/bin/go
	}
	b, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return
	}
	v, _, _ := strings.Cut(string(b), "\n")
	if v = strings.TrimSpace(v); v != "" {
		h.GoToolchain, h.GoToolchainSource = v, "GOROOT/VERSION"
	}
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
	backfillForm(r.Host)
	return &r, nil
}

// backfillForm fills Host.Form for records written before it existed, from
// Host.Env (a variable absent there was unset) and Host.RlimitAS.
func backfillForm(h *HostInfo) {
	if h == nil || h.Form != (RuntimeForm{}) {
		return
	}
	get := func(k string) string {
		if v, ok := h.Env[k]; ok {
			return v
		}
		return "unset"
	}
	h.Form = RuntimeForm{
		GOMAXPROCS: get("GOMAXPROCS"),
		GOGC:       get("GOGC"),
		GOMEMLIMIT: get("GOMEMLIMIT"),
		GOFLAGS:    get("GOFLAGS"),
		RlimitAS:   h.RlimitAS,
	}
}

// printRecord renders every section present in rec as human-readable tables.
func printRecord(w io.Writer, rec *Record) error {
	p := newPrinter(w)
	if rec.Label != "" {
		p.printf("== %s (%s)\n", rec.Label, rec.Kind)
	}
	if len(rec.Command) > 0 {
		p.printf("command: %s\n", truncate(shellJoin(rec.Command), maxCommandDisplay))
	}
	if rec.Host != nil && rec.Host.Form != (RuntimeForm{}) {
		p.printf("form: %s\n", rec.Host.Form)
	}
	if rec.Host != nil && rec.Host.GoToolchain != "" {
		p.printf("go toolchain: %s (from %s)\n", rec.Host.GoToolchain, rec.Host.GoToolchainSource)
	}
	if rec.Rusage != nil {
		printRusage(p, rec.Rusage)
	}
	if rec.Actiongraph != nil {
		printActiongraph(p, rec.Actiongraph)
	}
	if len(rec.Bench) > 0 {
		printBench(p, rec.Bench)
	}
	for i := range rec.Tests {
		printTests(p, &rec.Tests[i])
	}
	if len(rec.Deps) > 0 {
		printDeps(p, rec.Deps)
	}
	if rec.Normalized != nil {
		printNormalized(p, rec.Normalized)
	}
	return p.err
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
