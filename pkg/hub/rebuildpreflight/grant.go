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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// DetachedSentinel is the recorded branch name for a detached-HEAD rebuild.
// A real branch literally named "detached" has Detached == false and is
// unpinnable (ValidBranch reserves the name).
const DetachedSentinel = "detached"

// ForceGrant is the hub's permission for one forced re-run of a failed
// rebuild-server run, handed to the executor directly (never through the
// request parameters). The handler builds it from the stored failed run and
// the authenticated session only: the pins come from the failed run's own
// record, and the actor from the session identity.
type ForceGrant struct {
	// RerunOf is the ID of the failed run being re-run.
	RerunOf string
	// ActorID is the stable user ID of the admin who forced the run, and
	// ActorEmail that user's email.
	ActorID    string
	ActorEmail string
	// PinnedBranch is the branch the failed run built, or DetachedSentinel
	// when Detached is true.
	PinnedBranch string
	// Detached is true only when the failed run recorded detached: true.
	Detached bool
	// PinnedCommit is the commit the failed run built.
	PinnedCommit string
	// FailedFindings are the failed run's blocking preflight findings.
	FailedFindings []preflight.Finding
	// FailedCouldNotRun is set when the failed run's preflight could not
	// run.
	FailedCouldNotRun *CouldNotRun
}

// CouldNotRun identifies why a preflight could not run: its kind and its
// reason class.
type CouldNotRun struct {
	Kind  string `json:"kind"`
	Class string `json:"class"`
}

// RunView is the pure-logic view of a stored maintenance run, used by
// CheckRerunTarget and NewGrant without importing pkg/store.
type RunView struct {
	Exists       bool
	ID           string
	OperationKey string
	Status       string
	Result       map[string]any
}

// ParseRunView builds a RunView from stored run fields, decoding resultJSON
// when it is a JSON object.
func ParseRunView(exists bool, id, opKey, status, resultJSON string) RunView {
	v := RunView{
		Exists:       exists,
		ID:           id,
		OperationKey: opKey,
		Status:       status,
	}
	if resultJSON != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(resultJSON), &m); err == nil {
			v.Result = m
		}
	}
	return v
}

// CheckRerunTarget verifies steps 2-5 of the force request rules:
//   - target exists, belongs to rebuild-server, and has status "failed" (400);
//   - target has blocking preflight findings or a couldNotRun record (409);
//   - target has no forcedBy and is not itself forced: true (409);
//   - target is the latest rebuild-server run (409, S1).
func CheckRerunTarget(target RunView, latestRunID string) *Reject {
	if !target.Exists {
		return &Reject{Status: http.StatusBadRequest, Message: "rerunOf run not found"}
	}
	if target.OperationKey != RebuildServerKey {
		return &Reject{Status: http.StatusBadRequest, Message: "rerunOf must name a rebuild-server run"}
	}
	if target.Status != "failed" {
		return &Reject{Status: http.StatusBadRequest, Message: "rerunOf must name a failed run"}
	}
	hasBlocks := blockingCount(target.Result) > 0
	hasCouldNotRun := parseCouldNotRun(target.Result) != nil
	if !hasBlocks && !hasCouldNotRun {
		return &Reject{Status: http.StatusConflict, Message: "the target run has no blocking preflight findings to force"}
	}
	if target.Result["forcedBy"] != nil {
		return &Reject{Status: http.StatusConflict, Message: "the target run has already been forced"}
	}
	if forced, _ := target.Result["forced"].(bool); forced {
		return &Reject{Status: http.StatusConflict, Message: "a forced run cannot be forced again"}
	}
	if latestRunID == "" || target.ID != latestRunID {
		return &Reject{Status: http.StatusConflict, Message: "a newer run exists; review and force that one"}
	}
	return nil
}

// NewGrant constructs a ForceGrant from a verified failed run and the
// authenticated session identity (actorID, actorEmail). It re-validates the
// stored branch and commit pins (use-site R1); detached pinning is accepted
// only when the stored result has detached: true and branch == "detached".
func NewGrant(target RunView, actorID, actorEmail string) (*ForceGrant, *Reject) {
	commit, _ := target.Result["commit"].(string)
	if err := ValidCommit(commit); err != nil {
		return nil, &Reject{
			Status:  http.StatusConflict,
			Message: "the failed run's recorded branch or commit is invalid; start a fresh run",
		}
	}
	branch, _ := target.Result["branch"].(string)
	detached, _ := target.Result["detached"].(bool)
	if detached {
		if branch != DetachedSentinel {
			return nil, &Reject{
				Status:  http.StatusConflict,
				Message: "the failed run's recorded branch or commit is invalid; start a fresh run",
			}
		}
	} else {
		if branch == DetachedSentinel || ValidBranch(branch) != nil {
			return nil, &Reject{
				Status:  http.StatusConflict,
				Message: "the failed run's recorded branch or commit is invalid; start a fresh run (" + BranchRule + ")",
			}
		}
	}
	return &ForceGrant{
		RerunOf:           target.ID,
		ActorID:           actorID,
		ActorEmail:        actorEmail,
		PinnedBranch:      branch,
		Detached:          detached,
		PinnedCommit:      commit,
		FailedFindings:    parseBlockingFindings(target.Result),
		FailedCouldNotRun: parseCouldNotRun(target.Result),
	}, nil
}

func blockingCount(result map[string]any) int {
	if result == nil {
		return 0
	}
	pf, ok := result["preflight"].(map[string]any)
	if !ok {
		return 0
	}
	switch n := pf["blocking"].(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func parseCouldNotRun(result map[string]any) *CouldNotRun {
	if result == nil {
		return nil
	}
	raw, ok := result["couldNotRun"]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	kind, _ := m["kind"].(string)
	if kind == "" {
		return nil
	}
	class, _ := m["class"].(string)
	return &CouldNotRun{Kind: kind, Class: class}
}

func parseBlockingFindings(result map[string]any) []preflight.Finding {
	if result == nil {
		return nil
	}
	rawPF, ok := result["preflight"]
	if !ok || rawPF == nil {
		return nil
	}
	b, err := json.Marshal(rawPF)
	if err != nil {
		return nil
	}
	var rep preflight.Report
	if err := json.Unmarshal(b, &rep); err != nil {
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
