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

import "github.com/GoogleCloudPlatform/scion/pkg/conduit/router"

// Values of the route log field.
const (
	// conduitRouteLocal: the target session is held by this node's relay.
	conduitRouteLocal = "local"
	// conduitRouteRemote: the target session is held by another relay,
	// reached over the internal relay API.
	conduitRouteRemote = "remote"
)

// conduitRouteLogAttrs returns the log fields that show how a stream
// opened on res was routed: route (local or remote) and owner_instance,
// the relay instance that holds the target session. Both are identifiers
// only.
func conduitRouteLogAttrs(res router.Resolved) []any {
	route := conduitRouteRemote
	if res.Local {
		route = conduitRouteLocal
	}
	return []any{"route", route, "owner_instance", res.Record.RelayInstanceID}
}
