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

package cmd

import (
	"fmt"
	"os"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// hubProjectID returns the project ID of hubCtx, or "" without a Hub.
func hubProjectID(hubCtx *HubContext) string {
	if hubCtx == nil {
		return ""
	}
	return hubCtx.ProjectID
}

// currentAgentTarget returns the first of targets that is the current
// agent, or "" if none is (or the CLI is not running inside an agent). A
// target is the current agent when it matches the agent's name, slug or ID
// (from SCION_AGENT_NAME, SCION_AGENT_SLUG and SCION_AGENT_ID), compared as
// slugs, and the targets are in the agent's own project. targetProjectID is
// the project the targets belong to; they count as in another project only
// when it and SCION_PROJECT_ID are both known and differ, so an unknown
// project never hides a match.
func currentAgentTarget(targets []string, targetProjectID string) string {
	var selfRefs []string
	for _, ref := range []string{os.Getenv("SCION_AGENT_NAME"), os.Getenv("SCION_AGENT_SLUG"), os.Getenv("SCION_AGENT_ID")} {
		if slug := api.Slugify(ref); slug != "" {
			selfRefs = append(selfRefs, slug)
		}
	}
	if len(selfRefs) == 0 {
		return "" // not inside an agent
	}
	if ownProjectID := projectkeys.ProjectIDFromEnv(os.Getenv); ownProjectID != "" && targetProjectID != "" && ownProjectID != targetProjectID {
		return ""
	}
	for _, target := range targets {
		if slices.Contains(selfRefs, api.Slugify(target)) {
			return target
		}
	}
	return ""
}

// refuseCurrentAgentTarget returns an error if one of targets is the
// current agent and allowSelf (the command's --self flag) is false
// (ptone/scion#4843). Removing the current agent ends its own session, so
// it has to be asked for explicitly. By design the error names the flag
// but does not print a command to run: a rebuilt command could drop other
// targets or flags (such as --project) and so remove something other than
// what was asked for.
func refuseCurrentAgentTarget(targets []string, targetProjectID string, allowSelf bool) error {
	if allowSelf {
		return nil
	}
	if name := currentAgentTarget(targets, targetProjectID); name != "" {
		return fmt.Errorf("agent %q is the current agent; --self is required to remove it (nothing was changed; re-run the same command with --self added)", name)
	}
	return nil
}

// withoutCurrentAgent returns items without the current agent, for a
// command that acts on many agents and skips the current one (stop --all
// --rm). refs returns the references of an item (name, slug, ID), which
// are matched as in currentAgentTarget; the first must be the name to show.
// skipped holds that first reference (the name) of each item left out,
// whichever reference matched, so an agent matched only by its ID is still
// reported by name.
func withoutCurrentAgent[T any](items []T, targetProjectID string, refs func(T) []string) (kept []T, skipped []string) {
	kept = make([]T, 0, len(items))
	for _, item := range items {
		itemRefs := refs(item)
		if currentAgentTarget(itemRefs, targetProjectID) != "" {
			skipped = append(skipped, itemRefs[0])
			continue
		}
		kept = append(kept, item)
	}
	return kept, skipped
}
