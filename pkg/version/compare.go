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

package version

// BuildMatch is the result of comparing a Runtime Broker's build token with
// the hub's (CompareBuild). Its values are the wire strings used by the hub
// API (version_status / versionStatus).
type BuildMatch string

const (
	// BuildMatchSame means both sides report the same build token.
	BuildMatchSame BuildMatch = "match"
	// BuildMatchDifferent means both sides report a build token and the
	// tokens differ.
	BuildMatchDifferent BuildMatch = "mismatch"
	// BuildMatchUnknown means at least one side has no usable token.
	BuildMatchUnknown BuildMatch = "unknown"
)

// unknownToken is what Short returns when the binary carries no build
// information.
const unknownToken = "unknown"

// CompareBuild compares a hub build token with a Runtime Broker build token,
// both as produced by Short. The comparison is exact: no trimming, no
// leading "v" handling and no case folding, so a differently formatted token
// is a mismatch. An empty or "unknown" token on either side gives
// BuildMatchUnknown, never BuildMatchSame.
func CompareBuild(hub, broker string) BuildMatch {
	if hub == "" || broker == "" || hub == unknownToken || broker == unknownToken {
		return BuildMatchUnknown
	}
	if hub == broker {
		return BuildMatchSame
	}
	return BuildMatchDifferent
}
