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

import "github.com/GoogleCloudPlatform/scion/pkg/hubsync"

// isLocalWorkstationEndpoint reports whether endpoint is the local
// workstation hub: a loopback host that the CLI reaches with the dev token.
// See hubsync.IsLocalWorkstationEndpoint.
func isLocalWorkstationEndpoint(endpoint string) bool {
	return hubsync.IsLocalWorkstationEndpoint(endpoint)
}

// hubLinkNoEndpointError is returned by 'scion hub link' when no hub
// endpoint is configured. It is a type rather than errors.New so that the
// user-facing sentence can keep its capital letter and full stop.
type hubLinkNoEndpointError struct{}

func (hubLinkNoEndpointError) Error() string {
	return "No hub configured. Start the local hub with 'scion server start' (first run opens setup), " +
		"or set a remote one with 'scion config set hub.endpoint <url>'."
}

// errHubLinkNoEndpoint is the hubLinkNoEndpointError value.
var errHubLinkNoEndpoint error = hubLinkNoEndpointError{}
