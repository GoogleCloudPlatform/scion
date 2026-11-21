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
	"net/http"
	"strings"
)

// The maintenance operation keys of the Rebuild from git actions.
const (
	RebuildServerKey = "rebuild-server"
	RebuildWebKey    = "rebuild-web"
)

// branchParamKeys are the operations whose "branch" parameter names the
// branch to check out, so CheckRunRequest validates it.
var branchParamKeys = map[string]bool{
	RebuildServerKey: true,
	RebuildWebKey:    true,
}

// Reject is a request the hub refuses before it creates a run: the HTTP
// status and the message for the response.
type Reject struct {
	Status  int
	Message string
}

// ForceRequest is what a forced run request asks for beyond an ordinary run.
// When RerunOf is non-empty, the request is a forced re-run of that run ID.
type ForceRequest struct {
	// RerunOf is the failed run a forced request re-runs.
	RerunOf string
}

// CheckRunRequest checks a run request for the operation key, given its
// decoded JSON body as sent (before the hub's parameter parser trims or
// drops anything). It returns a Reject for a request the hub must refuse.
//
// Force rules (3.1 step 1):
//   - "force" and "rerunOf" are accepted only for rebuild-server;
//   - "force" must be the exact string "true" (not boolean true or "yes");
//   - "rerunOf" must be a non-empty string;
//   - "force" and "rerunOf" must appear together;
//   - no other parameter (including "branch") may accompany a forced request.
//
// Branch rules:
//   - For rebuild-server and rebuild-web, a "branch" parameter must be a
//     string, and, unless it is blank (the configured branch is used), a
//     valid branch name (ValidBranch). Duplicate keys that trim to "branch"
//     are rejected with 400.
func CheckRunRequest(key string, rawBody map[string]any) (ForceRequest, *Reject) {
	params, _ := rawBody["params"].(map[string]any)

	var (
		forceCount  int
		rawForce    any
		rerunCount  int
		rawRerun    any
		branchCount int
		rawBranch   any
		otherKeys   int
	)
	for k, v := range params {
		switch strings.TrimSpace(k) {
		case "force":
			rawForce = v
			forceCount++
		case "rerunOf":
			rawRerun = v
			rerunCount++
		case "branch":
			rawBranch = v
			branchCount++
		default:
			otherKeys++
		}
	}

	if forceCount > 0 || rerunCount > 0 {
		if key != RebuildServerKey {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "force re-run is only supported for rebuild-server"}
		}
		if forceCount > 1 || rerunCount > 1 {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "force and rerunOf may each be given at most once"}
		}
		if forceCount == 0 {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "rerunOf requires force=\"true\""}
		}
		if rerunCount == 0 {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "force requires rerunOf"}
		}
		forceStr, ok := rawForce.(string)
		if !ok || forceStr != "true" {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "force must be the string \"true\""}
		}
		rerunStr, ok := rawRerun.(string)
		if !ok || strings.TrimSpace(rerunStr) == "" {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "rerunOf must be a non-empty string"}
		}
		if branchCount > 0 || otherKeys > 0 {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "a forced re-run accepts only force and rerunOf"}
		}
		return ForceRequest{RerunOf: strings.TrimSpace(rerunStr)}, nil
	}

	if !branchParamKeys[key] {
		return ForceRequest{}, nil
	}
	if branchCount > 1 {
		return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "branch is given more than once; " + BranchRule}
	}
	if branchCount == 0 {
		return ForceRequest{}, nil
	}
	branch, isString := rawBranch.(string)
	if !isString {
		return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "branch must be a string; " + BranchRule}
	}
	if b := strings.TrimSpace(branch); b != "" {
		if err := ValidBranch(b); err != nil {
			return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: err.Error()}
		}
	}
	return ForceRequest{}, nil
}
