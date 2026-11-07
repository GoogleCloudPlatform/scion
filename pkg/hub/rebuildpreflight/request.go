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

// ForceRequest is what a run request asks for beyond an ordinary run. It is
// empty for every request today: force re-runs are not accepted yet.
type ForceRequest struct {
	// RerunOf is the failed run a forced request re-runs.
	RerunOf string
}

// CheckRunRequest checks a run request for the operation key, given its
// decoded JSON body as sent (before the hub's parameter parser trims or
// drops anything). It returns a Reject for a request the hub must refuse.
//
// For rebuild-server and rebuild-web, a "branch" parameter must be a
// string, and, unless it is blank (the configured branch is used), a valid
// branch name (ValidBranch). The value is checked as the executor will use
// it, with surrounding space trimmed.
func CheckRunRequest(key string, rawBody map[string]any) (ForceRequest, *Reject) {
	params, _ := rawBody["params"].(map[string]any)
	if branchParamKeys[key] {
		if raw, ok := params["branch"]; ok {
			branch, isString := raw.(string)
			if !isString {
				return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: "branch must be a string; " + BranchRule}
			}
			if b := strings.TrimSpace(branch); b != "" {
				if err := ValidBranch(b); err != nil {
					return ForceRequest{}, &Reject{Status: http.StatusBadRequest, Message: err.Error()}
				}
			}
		}
	}
	return ForceRequest{}, nil
}
