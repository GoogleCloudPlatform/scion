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
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

func okProbe() ProbeResult {
	return ProbeResult{Attempted: true, ExitCode: 0, Stdout: "Usage: scion server preflight --colocated-broker-id ..."}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClassify(t *testing.T) {
	t.Run("not_ready (S86)", func(t *testing.T) {
		o := Classify(false, okProbe(), RunResult{})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindNotReady {
			t.Fatalf("got %+v, want not_ready", o)
		}
	})

	t.Run("probe failed -> unsupported", func(t *testing.T) {
		o := Classify(true, ProbeResult{Attempted: true, ExitCode: 0, Stdout: "server help without flag"}, RunResult{})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindUnsupported {
			t.Fatalf("got %+v, want unsupported", o)
		}
	})

	t.Run("timed_out before exit code (S19)", func(t *testing.T) {
		o := Classify(true, okProbe(), RunResult{TimedOut: true, ExitCode: 137, Signal: "killed"})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindTimedOut {
			t.Fatalf("got %+v, want timed_out", o)
		}
	})

	t.Run("StartErr -> crashed class start (S98)", func(t *testing.T) {
		o := Classify(true, okProbe(), RunResult{StartErr: errors.New("permission denied")})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindCrashed || o.CouldNotRun.Class != "start" {
			t.Fatalf("got %+v, want crashed/start", o)
		}
	})

	t.Run("exit 1 -> failed with Error: class (S20) and CN3 fallback (S83)", func(t *testing.T) {
		o := Classify(true, okProbe(), RunResult{
			ExitCode:       1,
			RedactedStderr: "time=... level=WARN msg=something\nError: opening the postgres hub database read only: dial refused\n",
		})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindFailed || o.CouldNotRun.Class != "opening the postgres hub database read only" {
			t.Fatalf("got %+v", o.CouldNotRun)
		}

		oFallback := Classify(true, okProbe(), RunResult{
			ExitCode:       1,
			RedactedStderr: "some panic or raw line without prefix\n",
		})
		if oFallback.CouldNotRun == nil || oFallback.CouldNotRun.Kind != KindFailed || oFallback.CouldNotRun.Class != NoErrorLineClass {
			t.Fatalf("S83 fallback got %+v", oFallback.CouldNotRun)
		}
	})

	t.Run("non-brace stdout -> unsupported (S14)", func(t *testing.T) {
		for _, out := range []string{"", "   \n", "null", "[]", "plain text"} {
			o := Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: []byte(out)})
			if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindUnsupported {
				t.Errorf("stdout %q -> %+v, want unsupported", out, o.CouldNotRun)
			}
		}
	})

	t.Run("invalid_report rules (1.5)", func(t *testing.T) {
		validRep := preflight.Report{
			Version:  preflight.ReportVersion,
			Checks:   []string{preflight.ProfilesCheckName},
			Findings: []preflight.Finding{},
		}

		// Oversize (S69).
		o := Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: []byte("{}"), StdoutOversize: true})
		if o.CouldNotRun == nil || o.CouldNotRun.Kind != KindInvalidReport || o.CouldNotRun.Class != "too_large" {
			t.Errorf("oversize -> %+v", o.CouldNotRun)
		}

		// Trailing document.
		two := append(mustJSON(t, validRep), []byte(" {}")...)
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: two})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "trailing_document" {
			t.Errorf("trailing_document -> %+v", o.CouldNotRun)
		}

		// Wrong version (S18).
		badVer := validRep
		badVer.Version = 99
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, badVer)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "version" {
			t.Errorf("version -> %+v", o.CouldNotRun)
		}

		// Empty code.
		emptyCode := validRep
		emptyCode.Findings = []preflight.Finding{{Severity: preflight.SeverityWarn}}
		emptyCode.Warnings = 1
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, emptyCode)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "empty_code" {
			t.Errorf("empty_code -> %+v", o.CouldNotRun)
		}

		// Unknown severity.
		badSev := validRep
		badSev.Findings = []preflight.Finding{{Code: "x.y", Severity: "fatal"}}
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, badSev)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "severity" {
			t.Errorf("severity -> %+v", o.CouldNotRun)
		}

		// Counts mismatch (S16).
		badCounts := validRep
		badCounts.Blocking = 1
		o = Classify(true, okProbe(), RunResult{ExitCode: 3, Stdout: mustJSON(t, badCounts)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "counts" {
			t.Errorf("counts -> %+v", o.CouldNotRun)
		}

		// Exit mismatch (S15): exit 0 with blocking > 0.
		blockRep := validRep
		blockRep.Blocking = 1
		blockRep.Findings = []preflight.Finding{{Check: preflight.ProfilesCheckName, Code: "settings.profiles.x", Severity: preflight.SeverityBlock, Message: "m"}}
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, blockRep)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "exit_mismatch" {
			t.Errorf("S15 exit_mismatch -> %+v", o.CouldNotRun)
		}

		// Missing required check (S17).
		noCheck := validRep
		noCheck.Checks = []string{}
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, noCheck)})
		if o.CouldNotRun == nil || o.CouldNotRun.Class != "required_check" {
			t.Errorf("S17 required_check -> %+v", o.CouldNotRun)
		}

		// Valid clean report (exit 0) and valid blocking report (exit 3).
		o = Classify(true, okProbe(), RunResult{ExitCode: 0, Stdout: mustJSON(t, validRep)})
		if o.Report == nil || o.CouldNotRun != nil {
			t.Errorf("valid clean report -> %+v", o)
		}
		o = Classify(true, okProbe(), RunResult{ExitCode: 3, Stdout: mustJSON(t, blockRep)})
		if o.Report == nil || o.Report.Blocking != 1 || o.CouldNotRun != nil {
			t.Errorf("valid blocking report -> %+v", o)
		}
	})
}

func TestDecide(t *testing.T) {
	f1 := preflight.Finding{Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m1", Data: map[string]any{"profile": "dev", "project": "p1"}}
	f2 := preflight.Finding{Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m2", Data: map[string]any{"profile": "prod", "project": "p1"}}
	warn := preflight.Finding{Code: "settings.profiles.warn", Severity: preflight.SeverityWarn, Message: "w"}

	// Warn-only proceeds unforced (S22).
	d := Decide(Outcome{Report: &preflight.Report{Warnings: 1, Findings: []preflight.Finding{warn}}}, nil)
	if !d.Proceed || d.Forced {
		t.Errorf("S22 warn-only = %+v", d)
	}

	// Block without grant stops (S23).
	d = Decide(Outcome{Report: &preflight.Report{Blocking: 1, Findings: []preflight.Finding{f1}}}, nil)
	if d.Proceed {
		t.Errorf("S23 block without grant proceeded")
	}

	// Q-D: forced run with 0 blocks -> Proceed=true, Forced=false, NotNeeded=true (S60).
	g := &ForceGrant{RerunOf: "r1", FailedFindings: []preflight.Finding{f1}}
	d = Decide(Outcome{Report: &preflight.Report{}}, g)
	if !d.Proceed || d.Forced || !d.NotNeeded {
		t.Errorf("S60 Q-D = %+v", d)
	}

	// Subset matches -> Proceed=true, Forced=true.
	d = Decide(Outcome{Report: &preflight.Report{Blocking: 1, Findings: []preflight.Finding{f1}}}, g)
	if !d.Proceed || !d.Forced {
		t.Errorf("subset match = %+v", d)
	}

	// Superset rejected (S12).
	d = Decide(Outcome{Report: &preflight.Report{Blocking: 2, Findings: []preflight.Finding{f1, f2}}}, g)
	if d.Proceed || len(d.NewFindings) != 1 {
		t.Errorf("S12 superset = %+v", d)
	}

	// Could-not-run matching (S13, S54).
	gCNR := &ForceGrant{RerunOf: "r1", FailedCouldNotRun: &CouldNotRun{Kind: KindFailed, Class: "class-a"}}
	d = Decide(Outcome{CouldNotRun: &CouldNotRunDetail{Kind: KindFailed, Class: "class-a"}}, gCNR)
	if !d.Proceed || !d.Forced {
		t.Errorf("same kind+class = %+v", d)
	}
	d = Decide(Outcome{CouldNotRun: &CouldNotRunDetail{Kind: KindFailed, Class: "class-b"}}, gCNR)
	if d.Proceed {
		t.Errorf("S54 different class for failed proceeded")
	}
	d = Decide(Outcome{CouldNotRun: &CouldNotRunDetail{Kind: KindTimedOut, Class: KindTimedOut}}, gCNR)
	if d.Proceed {
		t.Errorf("S13 different kind proceeded")
	}
}

func TestOverride_ForcedBy_RunResultMap_And_Emit(t *testing.T) {
	f1 := preflight.Finding{Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m1", SubjectCount: 5, Data: map[string]any{"profile": "dev", "agents": float64(5)}}
	f1Prev := preflight.Finding{Code: "settings.profiles.missing", Severity: preflight.SeverityBlock, Message: "m1", SubjectCount: 3, Data: map[string]any{"profile": "dev", "agents": float64(3)}}
	g := &ForceGrant{
		RerunOf:        "run-1",
		ActorID:        "u-1",
		ActorEmail:     "admin@example.com",
		PinnedBranch:   "main",
		PinnedCommit:   testValidSHA,
		FailedFindings: []preflight.Finding{f1Prev},
	}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ov := NewOverride(g, Outcome{Report: &preflight.Report{Blocking: 1, Findings: []preflight.Finding{f1}}}, now)
	if ov.Findings[0].Agents != 5 || ov.Findings[0].FailedRunAgents != 3 {
		t.Errorf("override agents = %+v", ov.Findings[0])
	}

	// R5: forcedBy minus runId deep-equals override.
	fb := ForcedByRecord(ov, "run-2")
	if fb["runId"] != "run-2" || fb["branch"] != "main" {
		t.Fatalf("S28 forcedBy = %+v", fb)
	}
	delete(fb, "runId")
	var ovMap map[string]any
	_ = json.Unmarshal(mustJSON(t, ov), &ovMap)
	if !reflect.DeepEqual(fb, ovMap) {
		t.Errorf("R5 forcedBy minus runId != override:\nfb=%+v\nov=%+v", fb, ovMap)
	}

	// RunResultMap emits all keys (S32, S33, CN4-a).
	bTrue := true
	rm := RunResultMap(RunState{
		Branch:          "main",
		Detached:        true,
		BranchPinnable:  &bTrue,
		Commit:          testValidSHA,
		Preflight:       &preflight.Report{},
		CouldNotRun:     &CouldNotRunDetail{Kind: KindFailed, Class: "c", Reason: "r"},
		Forced:          &bTrue,
		OverrideApplied: &bTrue,
		Installed:       true,
		ForceRejected:   "findings changed",
		ForceNotNeeded:  true,
		RerunOf:         "run-1",
		Override:        ov,
	})
	for _, k := range []string{"branch", "detached", "branchPinnable", "commit", "preflight", "couldNotRun", "forced", "overrideApplied", "installed", "forceRejected", "forceNotNeeded", "rerunOf", "override"} {
		if _, ok := rm[k]; !ok {
			t.Errorf("RunResultMap missing key %q", k)
		}
	}

	// Authority() & EmitOverride (S44, S59, S91).
	ev := &MaintenanceOverrideEvent{
		ActorID:    "u-1",
		ActorEmail: "admin@example.com",
		Timestamp:  now,
		RunID:      "run-2",
		RerunOf:    "run-1",
		Commit:     testValidSHA,
		Branch:     "main",
		Blocking:   1,
	}
	rec, auth := ev.Authority()
	if rec != "index" || auth != "maintenance run rows run-2, run-1" {
		t.Errorf("S59 Authority() = (%q, %q)", rec, auth)
	}

	// Nil sink -> fail-open Error log with all fields (S91).
	var loggedMsg string
	var loggedArgs []any
	EmitOverride(context.Background(), nil, ev, func(msg string, args ...any) {
		loggedMsg = msg
		loggedArgs = args
	})
	if !strings.Contains(loggedMsg, "audit emit failed") || len(loggedArgs) == 0 {
		t.Errorf("S91 nil sink did not log error")
	}

	// Failing sink -> fail-open Error log (S44).
	loggedMsg = ""
	EmitOverride(context.Background(), failingSink{}, ev, func(msg string, args ...any) {
		loggedMsg = msg
	})
	if !strings.Contains(loggedMsg, "audit emit failed") {
		t.Errorf("S44 failing sink did not log error")
	}
}

type failingSink struct{}

func (failingSink) LogMaintenanceOverrideEvent(context.Context, *MaintenanceOverrideEvent) error {
	return errors.New("sink boom")
}
