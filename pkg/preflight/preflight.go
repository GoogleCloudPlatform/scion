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

// Package preflight is the registry of named checks the admin rebuild runs
// with a newly built binary before installing it (ptone/scion#5057). Each
// check returns findings with a severity: block findings stop the rebuild,
// warn and info findings are shown only.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Severity is how a finding affects the rebuild.
type Severity string

const (
	// SeverityBlock stops the rebuild unless it is forced.
	SeverityBlock Severity = "block"
	// SeverityWarn is shown and does not stop the rebuild.
	SeverityWarn Severity = "warn"
	// SeverityInfo is shown and counted in neither Blocking nor Warnings.
	SeverityInfo Severity = "info"
)

func (s Severity) valid() bool {
	return s == SeverityBlock || s == SeverityWarn || s == SeverityInfo
}

// MaxSubjects caps Finding.Subjects; SubjectCount holds the full count.
const MaxSubjects = 20

// Finding is one result of a check. Message and Remediation are human text
// and carry no settings values.
type Finding struct {
	Check        string         `json:"check"`
	Code         string         `json:"code"`
	Severity     Severity       `json:"severity"`
	Message      string         `json:"message"`
	Remediation  string         `json:"remediation,omitempty"`
	Subjects     []string       `json:"subjects,omitempty"`
	SubjectCount int            `json:"subjectCount,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}

// Check is a named check. Run returns the check's findings; an error, a
// timeout or a panic becomes one block finding "<name>.could_not_run", whose
// message quotes the error or the panic value, so Run must return errors
// that name what failed (its class) without settings values.
type Check struct {
	Name    string
	Timeout time.Duration
	Run     func(ctx context.Context, env Env) ([]Finding, error)
}

// Env is what a check reads; the preflight command fills it. pkg/preflight
// imports no hub or store package: the database reader is an interface
// declared in this package (ReadOnlyQueries) and implemented by the
// command, so the dependency points from the command to this package.
type Env struct {
	// GlobalDir is the hub's global settings directory (~/.scion).
	GlobalDir string
	// ServerConfigPath is the path the server was started with, or "".
	ServerConfigPath string
	// ColocatedBrokerID is the hub's co-located broker, or "" for a hub
	// with none.
	ColocatedBrokerID string
	// DB reads the hub database. Optional: a check that needs it and finds
	// it nil cannot run.
	DB ReadOnlyQueries
}

// ReportVersion is the version of Report. Adding optional fields keeps it;
// removing or renaming a field, or changing a meaning, bumps it.
const ReportVersion = 1

// Report is the preflight document the command prints.
type Report struct {
	Version  int       `json:"version"`
	Checks   []string  `json:"checks"`
	Findings []Finding `json:"findings"`
	Blocking int       `json:"blocking"`
	Warnings int       `json:"warnings"`
}

var checkNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Registry holds named checks.
type Registry struct {
	mu     sync.Mutex
	checks map[string]Check
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{checks: map[string]Check{}} }

// NewDefaultRegistry returns a registry holding every built-in check: the
// checks "scion server preflight" runs. It is the one list to add a check
// to.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(ProfilesCheck())
	return r
}

// Register adds c. It panics on a programming error: a name that is not
// dotted lower case (for example "settings.profiles"), a duplicate name, a
// timeout that is not positive, or a nil Run.
func (r *Registry) Register(c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !checkNameRE.MatchString(c.Name):
		panic(fmt.Sprintf("preflight: check name %q is not dotted lower case", c.Name))
	case c.Timeout <= 0:
		panic(fmt.Sprintf("preflight: check %q has no positive timeout", c.Name))
	case c.Run == nil:
		panic(fmt.Sprintf("preflight: check %q has no Run", c.Name))
	}
	if _, dup := r.checks[c.Name]; dup {
		panic(fmt.Sprintf("preflight: check %q registered twice", c.Name))
	}
	r.checks[c.Name] = c
}

// RunAll runs every registered check in name order, each under its own
// timeout, and returns the report. A check that errors, times out or panics
// gives one block finding with code "<name>.could_not_run": findings it
// returned with an error are dropped, and a timed-out check's late result is
// discarded (its context is cancelled). The message says whether the
// check's own timeout, the parent's deadline or a cancellation ended it.
// A finding whose severity the registry does not know, or whose code does
// not start with "<name>.", gives "<name>.invalid_finding" (block) in its
// place. RunAll sets each
// finding's Check, and sorts findings by check, code and subjects.
func (r *Registry) RunAll(ctx context.Context, env Env) Report {
	r.mu.Lock()
	checks := make([]Check, 0, len(r.checks))
	for _, c := range r.checks {
		checks = append(checks, c)
	}
	r.mu.Unlock()
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}

	rep := Report{Version: ReportVersion, Checks: names, Findings: []Finding{}}
	for _, c := range checks {
		name := c.Name
		for _, f := range runOne(ctx, c, env) {
			if !f.Severity.valid() || !strings.HasPrefix(f.Code, name+".") {
				f = Finding{Check: name, Code: name + ".invalid_finding", Severity: SeverityBlock,
					Message: fmt.Sprintf("check %s returned an invalid finding: severity %q, code %q", name, f.Severity, f.Code)}
			}
			f.Check = name
			rep.Findings = append(rep.Findings, capSubjects(f))
		}
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Check != b.Check {
			return a.Check < b.Check
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return fmt.Sprint(a.Subjects) < fmt.Sprint(b.Subjects)
	})
	for _, f := range rep.Findings {
		switch f.Severity {
		case SeverityBlock:
			rep.Blocking++
		case SeverityWarn:
			rep.Warnings++
		}
	}
	return rep
}

// errCheckTimeout is the cause of a check's own timeout, so runOne can
// tell it from the parent context's deadline or cancellation.
var errCheckTimeout = errors.New("check timeout")

// runOne runs c under its timeout with panic recovery.
func runOne(ctx context.Context, c Check, env Env) []Finding {
	ctx, cancel := context.WithTimeoutCause(ctx, c.Timeout, errCheckTimeout)
	defer cancel()
	type result struct {
		findings []Finding
		err      error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		f, err := c.Run(ctx, env)
		done <- result{findings: f, err: err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return []Finding{couldNotRun(c.Name, res.err.Error())}
		}
		return res.findings
	case <-ctx.Done():
		why := "cancelled"
		switch {
		case errors.Is(context.Cause(ctx), errCheckTimeout):
			why = fmt.Sprintf("timed out after %s", c.Timeout)
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			why = "the preflight deadline passed"
		}
		return []Finding{couldNotRun(c.Name, why)}
	}
}

func couldNotRun(name, why string) Finding {
	return Finding{Check: name, Code: name + ".could_not_run", Severity: SeverityBlock,
		Message: fmt.Sprintf("check %s could not run: %s", name, why)}
}

// capSubjects keeps at most MaxSubjects subjects and records the full count.
func capSubjects(f Finding) Finding {
	if f.SubjectCount < len(f.Subjects) {
		f.SubjectCount = len(f.Subjects)
	}
	if len(f.Subjects) > MaxSubjects {
		f.Subjects = append([]string(nil), f.Subjects[:MaxSubjects]...)
	}
	return f
}
