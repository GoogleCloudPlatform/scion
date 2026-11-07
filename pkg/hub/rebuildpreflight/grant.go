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

import "github.com/GoogleCloudPlatform/scion/pkg/preflight"

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
	// PinnedBranch is the branch the failed run built.
	PinnedBranch string
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
	Kind  string
	Class string
}
