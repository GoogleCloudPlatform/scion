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

package rebuildpreflight

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// Could-not-run kinds, in classification order (design 1.4).
const (
	KindNotReady      = "not_ready"
	KindUnsupported   = "unsupported"
	KindTimedOut      = "timed_out"
	KindFailed        = "failed"
	KindCrashed       = "crashed"
	KindInvalidReport = "invalid_report"
)

// NoErrorLineClass is the fallback class when preflight exits 1 without a
// line beginning with "Error: " (CN3).
const NoErrorLineClass = "exit 1 (no error line)"

// RequiredPreflightChecks lists the checks every valid preflight report
// must have run (design 1.5).
var RequiredPreflightChecks = []string{preflight.ProfilesCheckName}

// ProbeResult is the outcome of `<bin> server preflight --help`.
type ProbeResult struct {
	Attempted bool
	ExitCode  int
	TimedOut  bool
	StartErr  error
	Stdout    string
}

// RunResult is the raw outcome of running `<bin> server preflight`.
type RunResult struct {
	ExitCode       int
	Signal         string
	TimedOut       bool
	StartErr       error
	WaitDelay      bool
	Stdout         []byte
	StdoutOversize bool
	RedactedStderr string
}

// CouldNotRunDetail records why preflight could not produce a valid report.
type CouldNotRunDetail struct {
	Kind   string `json:"kind"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

// Finding returns the synthetic block finding representing this could-not-run
// outcome.
func (d *CouldNotRunDetail) Finding() preflight.Finding {
	return preflight.Finding{
		Check:    "preflight",
		Code:     "preflight.could_not_run." + d.Kind,
		Severity: preflight.SeverityBlock,
		Message:  d.Reason,
		Data:     map[string]any{"class": d.Class},
	}
}

// Outcome is the classified result of running preflight: either a validated
// Report or a CouldNotRunDetail (never both).
type Outcome struct {
	Report      *preflight.Report
	CouldNotRun *CouldNotRunDetail
}

// Classify applies design sections 1.4 and 1.5 in strict order:
//  0. ready == false -> not_ready
//  1. probe failed -> unsupported
//  2. run.TimedOut -> timed_out (tested before exit code)
//  3. run.StartErr != nil -> crashed (class "start")
//  4. exit 1 (no signal) -> failed (Error: line class, else CN3 fallback)
//  5. signal or exit code not in {0, 3} -> crashed
//  6. exit 0 or 3, stdout empty or not starting with '{' -> unsupported
//  7. exit 0 or 3, stdout starting with '{' failing 1.5 rules -> invalid_report
//  8. valid report -> Outcome{Report: &rep}
func Classify(ready bool, probe ProbeResult, run RunResult) Outcome {
	// 0. Embedded broker readiness.
	if !ready {
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindNotReady,
			Class:  KindNotReady,
			Reason: "embedded broker registration is still pending",
		}}
	}

	// 1. Probe check.
	if !probe.Attempted || probe.StartErr != nil || probe.TimedOut || probe.ExitCode != 0 || !strings.Contains(probe.Stdout, "--colocated-broker-id") {
		reason := "built binary does not support server preflight --colocated-broker-id"
		if probe.TimedOut {
			reason = "preflight support probe timed out"
		} else if probe.StartErr != nil {
			reason = fmt.Sprintf("preflight support probe could not start: %v", probe.StartErr)
		}
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindUnsupported,
			Class:  KindUnsupported,
			Reason: reason,
		}}
	}

	// 2. Timeout (tested before exit code).
	if run.TimedOut {
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindTimedOut,
			Class:  KindTimedOut,
			Reason: "preflight timed out",
		}}
	}

	// 3. Start error (CN-b).
	if run.StartErr != nil {
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindCrashed,
			Class:  "start",
			Reason: capReason("start error: "+run.StartErr.Error(), 300),
		}}
	}

	// 4. Exit 1 -> failed (Q-A, C1, CN3).
	if run.Signal == "" && run.ExitCode == 1 {
		class, reason := extractExit1ClassAndReason(run.RedactedStderr)
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindFailed,
			Class:  class,
			Reason: reason,
		}}
	}

	// 5. Signal or unexpected non-zero exit -> crashed.
	if run.Signal != "" {
		class := "signal " + run.Signal
		reason := "killed by signal " + run.Signal
		if tail := stderrTail(run.RedactedStderr, 240); tail != "" {
			reason = capReason(reason+": "+tail, 300)
		}
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindCrashed,
			Class:  class,
			Reason: reason,
		}}
	}
	if run.ExitCode != 0 && run.ExitCode != 3 {
		class := fmt.Sprintf("exit %d", run.ExitCode)
		reason := class
		if tail := stderrTail(run.RedactedStderr, 240); tail != "" {
			reason = capReason(reason+": "+tail, 300)
		}
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindCrashed,
			Class:  class,
			Reason: reason,
		}}
	}

	// 6. Exit 0 or 3: stdout must start with '{', else unsupported (R4).
	trimmed := bytes.TrimSpace(run.Stdout)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Outcome{CouldNotRun: &CouldNotRunDetail{
			Kind:   KindUnsupported,
			Class:  KindUnsupported,
			Reason: "preflight did not emit a JSON report",
		}}
	}

	// 7. Report validation (1.5).
	if run.StdoutOversize || len(run.Stdout) > MaxStdoutBytes {
		return invalidReport("too_large")
	}

	var rep preflight.Report
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&rep); err != nil {
		return invalidReport("invalid_json")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return invalidReport("trailing_document")
	}

	if rep.Version != preflight.ReportVersion {
		return invalidReport("version")
	}

	var blockCount, warnCount int
	for _, f := range rep.Findings {
		if strings.TrimSpace(f.Code) == "" {
			return invalidReport("empty_code")
		}
		switch f.Severity {
		case preflight.SeverityBlock:
			blockCount++
		case preflight.SeverityWarn:
			warnCount++
		case preflight.SeverityInfo:
		default:
			return invalidReport("severity")
		}
	}
	if rep.Blocking != blockCount || rep.Warnings != warnCount {
		return invalidReport("counts")
	}
	if (run.ExitCode == 3) != (rep.Blocking > 0) {
		return invalidReport("exit_mismatch")
	}

	checkSet := make(map[string]bool, len(rep.Checks))
	for _, name := range rep.Checks {
		checkSet[name] = true
	}
	for _, req := range RequiredPreflightChecks {
		if !checkSet[req] {
			return invalidReport("required_check")
		}
	}

	return Outcome{Report: &rep}
}

func invalidReport(rule string) Outcome {
	return Outcome{CouldNotRun: &CouldNotRunDetail{
		Kind:   KindInvalidReport,
		Class:  rule,
		Reason: rule,
	}}
}

func extractExit1ClassAndReason(redactedStderr string) (class, reason string) {
	lines := strings.Split(redactedStderr, "\n")
	var lastErrLine string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "Error: ") {
			lastErrLine = strings.TrimSpace(strings.TrimPrefix(line, "Error: "))
		}
	}
	if lastErrLine != "" {
		reason = capReason(lastErrLine, 300)
		class = reason
		if idx := strings.IndexByte(reason, ':'); idx > 0 {
			class = strings.TrimSpace(reason[:idx])
		}
		return class, reason
	}
	tail := stderrTail(redactedStderr, 300)
	if tail == "" {
		tail = NoErrorLineClass
	}
	return NoErrorLineClass, tail
}

func stderrTail(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxChars {
		return s
	}
	return strings.TrimSpace(s[len(s)-maxChars:])
}

func capReason(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxChars {
		return s
	}
	return strings.TrimSpace(s[:maxChars])
}
