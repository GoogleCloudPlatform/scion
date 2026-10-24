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

package hub

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// previousProjectIDs returns the earlier project IDs a create, start or
// restart dispatch sends (wire key previousProjectIds), so that the broker
// can carry a git project's agent over to the current project ID after the
// project was re-linked (ptone/scion#4476). This is a targeted fix: the Hub
// keeps no history of project IDs, so the only earlier ID it can name is the
// one a git-backed project had before project IDs became random
// (util.LegacyGitProjectID of its stored remote).
//
// It returns that ID only when all of these hold, and nil otherwise:
//   - the project is a shared-workspace project (the only dispatch whose
//     broker re-links), which implies it has a git remote;
//   - the stored remote passes util.LegacyGitProjectID's guard;
//   - the derived ID differs from the project's ID;
//   - the project is the only live project on that remote;
//   - no live project has the derived ID.
//
// Any store error also gives nil: the dispatch then goes on without the
// list, and the broker refuses the earlier marker as it does today. See
// util.LegacyGitProjectID for the trust assumption this relies on.
func (d *HTTPAgentDispatcher) previousProjectIDs(ctx context.Context, info projectDispatchInfo) []string {
	if d.store == nil || !info.sharedWorkspace || info.projectID == "" {
		return nil
	}
	legacyID, ok := util.LegacyGitProjectID(info.gitRemote)
	if !ok || legacyID == info.projectID {
		return nil
	}
	sameRemote, err := d.store.GetProjectsByGitRemote(ctx, info.gitRemote)
	if err != nil || len(sameRemote) != 1 || sameRemote[0] == nil || sameRemote[0].ID != info.projectID {
		return nil
	}
	if _, err := d.store.GetProject(ctx, legacyID); !errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return []string{legacyID}
}
