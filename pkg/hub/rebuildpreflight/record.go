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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// OverrideFinding is one blocking finding captured in an Override record,
// including the current and failed-run agent counts (N3-d).
type OverrideFinding struct {
	Code            string         `json:"code"`
	Message         string         `json:"message"`
	SubjectCount    int            `json:"subjectCount,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
	Agents          int            `json:"agents,omitempty"`
	FailedRunAgents int            `json:"failedRunAgents,omitempty"`
}

// Override is the structured record persisted on both the forced run
// ("override") and the failed run ("forcedBy", verbatim plus "runId", R5).
type Override struct {
	ActorID           string             `json:"actorId"`
	ActorEmail        string             `json:"actorEmail"`
	At                string             `json:"at"`
	RerunOf           string             `json:"rerunOf"`
	Commit            string             `json:"commit"`
	Branch            string             `json:"branch"`
	CouldNotRun       *CouldNotRunDetail `json:"couldNotRun,omitempty"`
	Findings          []OverrideFinding  `json:"findings,omitempty"`
	FailedRunFindings int                `json:"failedRunFindings"`
}

// NewOverride builds an Override record from the grant, the forced run's
// outcome, and the override timestamp.
func NewOverride(g *ForceGrant, o Outcome, at time.Time) *Override {
	if g == nil {
		return nil
	}
	ov := &Override{
		ActorID:           g.ActorID,
		ActorEmail:        g.ActorEmail,
		At:                at.UTC().Format(time.RFC3339),
		RerunOf:           g.RerunOf,
		Commit:            g.PinnedCommit,
		Branch:            g.PinnedBranch,
		CouldNotRun:       o.CouldNotRun,
		FailedRunFindings: len(g.FailedFindings),
	}
	if o.Report != nil {
		prevAgents := make(map[findingKey]int, len(g.FailedFindings))
		for _, f := range g.FailedFindings {
			if f.Severity == preflight.SeverityBlock {
				prevAgents[keyOfFinding(f)] = findingAgentCount(f)
			}
		}
		for _, f := range o.Report.Findings {
			if f.Severity != preflight.SeverityBlock {
				continue
			}
			k := keyOfFinding(f)
			ov.Findings = append(ov.Findings, OverrideFinding{
				Code:            f.Code,
				Message:         f.Message,
				SubjectCount:    f.SubjectCount,
				Data:            f.Data,
				Agents:          findingAgentCount(f),
				FailedRunAgents: prevAgents[k],
			})
		}
	}
	return ov
}

func findingAgentCount(f preflight.Finding) int {
	if f.Data != nil {
		switch v := f.Data["agents"].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	if f.SubjectCount > 0 {
		return f.SubjectCount
	}
	return len(f.Subjects)
}

// ForcedByRecord builds the "forcedBy" map written to the failed run row:
// the Override record verbatim plus "runId" (R5, Guard A).
func ForcedByRecord(o *Override, runID string) map[string]any {
	if o == nil {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	m["runId"] = runID
	return m
}

// RunState holds the executor's structured result state for a rebuild-server
// run. RunResultMap converts it into the JSON-serializable Result map.
type RunState struct {
	Branch          string
	Detached        bool
	BranchPinnable  *bool
	Commit          string
	Preflight       *preflight.Report
	CouldNotRun     *CouldNotRunDetail
	Forced          *bool
	OverrideApplied *bool
	Installed       bool
	ForceRejected   string
	ForceNotNeeded  bool
	RerunOf         string
	Override        *Override
}

// RunResultMap builds the structured Result map for a rebuild-server run
// (design 1.0, CN4-a). The executor never sets "error"; MergeFinal owns it.
func RunResultMap(s RunState) map[string]any {
	out := map[string]any{}
	if s.Branch != "" {
		out["branch"] = s.Branch
	}
	if s.Detached {
		out["detached"] = true
	}
	if s.BranchPinnable != nil {
		out["branchPinnable"] = *s.BranchPinnable
	}
	if s.Commit != "" {
		out["commit"] = s.Commit
	}
	if s.Preflight != nil {
		out["preflight"] = s.Preflight
	}
	if s.CouldNotRun != nil {
		out["couldNotRun"] = s.CouldNotRun
	}
	if s.Forced != nil {
		out["forced"] = *s.Forced
	}
	if s.OverrideApplied != nil {
		out["overrideApplied"] = *s.OverrideApplied
	}
	if s.Installed {
		out["installed"] = true
	}
	if s.ForceRejected != "" {
		out["forceRejected"] = s.ForceRejected
	}
	if s.ForceNotNeeded {
		out["forceNotNeeded"] = true
	}
	if s.RerunOf != "" {
		out["rerunOf"] = s.RerunOf
	}
	if s.Override != nil {
		out["override"] = s.Override
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FormatPreflightText renders rep in the same human-readable format as
// cmd's writePreflightText (design 1.6).
func FormatPreflightText(rep preflight.Report) string {
	info := 0
	for _, f := range rep.Findings {
		if f.Severity == preflight.SeverityInfo {
			info++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preflight: %d blocking, %d warning(s), %d info (checks: %s)\n",
		rep.Blocking, rep.Warnings, info, strings.Join(rep.Checks, ", "))
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "[%s] %s: %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Message)
		if len(f.Subjects) > 0 {
			fmt.Fprintf(&b, "  subjects: %s", strings.Join(f.Subjects, ", "))
			if more := f.SubjectCount - len(f.Subjects); more > 0 {
				fmt.Fprintf(&b, " (+%d more)", more)
			}
			b.WriteString("\n")
		}
		if f.Remediation != "" {
			fmt.Fprintf(&b, "  fix: %s\n", f.Remediation)
		}
	}
	return b.String()
}
