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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// Decision is the executor's proceed/stop decision for a preflight Outcome.
type Decision struct {
	Proceed     bool
	Forced      bool
	NotNeeded   bool
	Reason      string
	NewFindings []preflight.Finding
}

// findingKey is the M6 subset-matching key for a blocking finding:
// (Code, Data["profile"], Data["project"]) when Data has a non-empty
// "profile", else (Code, Message).
type findingKey struct {
	code    string
	profile string
	project string
	message string
}

func keyOfFinding(f preflight.Finding) findingKey {
	if f.Data != nil {
		if prof, ok := f.Data["profile"].(string); ok && prof != "" {
			proj, _ := f.Data["project"].(string)
			return findingKey{code: f.Code, profile: prof, project: proj}
		}
	}
	return findingKey{code: f.Code, message: f.Message}
}

// Decide determines whether the rebuild may proceed given the preflight
// Outcome and an optional ForceGrant (design 1.6, 3.4, Q-D, C1, M6).
func Decide(o Outcome, g *ForceGrant) Decision {
	// Clean outcome: no could-not-run and no blocking findings.
	if o.CouldNotRun == nil && (o.Report == nil || o.Report.Blocking == 0) {
		if g != nil {
			// Q-D: a forced run with no blocking findings proceeds unforced
			// with forceNotNeeded: true.
			return Decision{
				Proceed:   true,
				Forced:    false,
				NotNeeded: true,
				Reason:    "force not needed: no blocking findings",
			}
		}
		return Decision{Proceed: true}
	}

	// Unforced run with blocks or could-not-run stops before install.
	if g == nil {
		if o.CouldNotRun != nil {
			return Decision{
				Proceed: false,
				Reason: fmt.Sprintf("preflight could not run (%s): %s; review the run detail and re-run with force",
					o.CouldNotRun.Kind, o.CouldNotRun.Reason),
			}
		}
		return Decision{
			Proceed: false,
			Reason: fmt.Sprintf("preflight found %d blocking finding(s); review them on the run detail and re-run with force",
				o.Report.Blocking),
		}
	}

	// Forced run: could-not-run path (C1).
	if o.CouldNotRun != nil {
		if g.FailedCouldNotRun != nil && couldNotRunMatches(o.CouldNotRun, g.FailedCouldNotRun) {
			return Decision{Proceed: true, Forced: true}
		}
		return Decision{
			Proceed: false,
			Reason:  "findings changed since the failed run; review them and re-run",
		}
	}

	// Forced run: report with blocking findings (M6 subset check).
	if g.FailedCouldNotRun != nil {
		// Failed run was could-not-run, but this run produced real blocking
		// findings: the operator has not reviewed them.
		return Decision{
			Proceed:     false,
			Reason:      "findings changed since the failed run; review them and re-run",
			NewFindings: blockingFindings(o.Report),
		}
	}

	failedKeys := make(map[findingKey]bool, len(g.FailedFindings))
	for _, f := range g.FailedFindings {
		if f.Severity == preflight.SeverityBlock {
			failedKeys[keyOfFinding(f)] = true
		}
	}
	var newFindings []preflight.Finding
	for _, f := range o.Report.Findings {
		if f.Severity != preflight.SeverityBlock {
			continue
		}
		if !failedKeys[keyOfFinding(f)] {
			newFindings = append(newFindings, f)
		}
	}
	if len(newFindings) == 0 {
		return Decision{Proceed: true, Forced: true}
	}
	codes := make([]string, 0, len(newFindings))
	for _, f := range newFindings {
		codes = append(codes, f.Code)
	}
	return Decision{
		Proceed:     false,
		Reason:      fmt.Sprintf("findings changed since the failed run; review them and re-run (new: %s)", strings.Join(codes, ", ")),
		NewFindings: newFindings,
	}
}

func couldNotRunMatches(cur *CouldNotRunDetail, prev *CouldNotRun) bool {
	if cur.Kind != prev.Kind {
		return false
	}
	switch cur.Kind {
	case KindFailed, KindCrashed, KindInvalidReport:
		return cur.Class == prev.Class
	default:
		// unsupported, not_ready, timed_out match on kind alone.
		return true
	}
}

func blockingFindings(rep *preflight.Report) []preflight.Finding {
	if rep == nil {
		return nil
	}
	var out []preflight.Finding
	for _, f := range rep.Findings {
		if f.Severity == preflight.SeverityBlock {
			out = append(out, f)
		}
	}
	return out
}
