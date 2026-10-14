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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hubAuthURLPrecedence documents the hub 'scion hub auth login' and
// 'logout' use. It is the order every hub command uses (invocationHub), plus
// the deprecated --hub-url alias of --hub.
const hubAuthURLPrecedence = `The hub is taken from, in order:
  1. the root --hub flag (a hub nickname or URL)
  2. the SCION_HUB_ENDPOINT environment variable, then SCION_HUB_URL
  3. the hub the current project directory is linked to ('scion hub link')
  4. the current hub ('scion use --hub <name>')
This is the order every hub command uses. --hub-url is a deprecated alias of --hub.`

// hubAuthURL returns the hub URL for 'hub auth login' and 'logout': the
// deprecated --hub-url (a URL or a hub nickname), else the hub of this
// invocation (invocationHub). An unknown nickname is an error that lists
// the registered names.
func hubAuthURL(hubURLFlag string) (string, error) {
	if hubURLFlag != "" {
		return hubAuthURLFlag(hubURLFlag)
	}
	return invocationHubEndpoint(), nil
}

// hubAuthURLFlag returns the hub URL a --hub-url value names: the value
// itself for a URL, or the endpoint of the hub entry for a nickname. An
// unknown nickname is an error that lists the registered names. The root
// --hub flag is resolved the same way by resolveInvocationTarget;
// --hub-url is a subcommand flag, so it is resolved here.
func hubAuthURLFlag(v string) (string, error) {
	if !isHubNickname(v) || contextDisabled(resolveMode()) {
		return v, nil
	}
	c, err := config.LoadCLIContext()
	if err != nil {
		return "", fmt.Errorf("cannot look up hub '%s': %w", v, err)
	}
	h := c.Hubs[v]
	if h == nil {
		return "", unknownHubError(v, c)
	}
	if h.Endpoint == "" {
		return "", hubWithoutEndpointError(v)
	}
	return h.Endpoint, nil
}

// sameHubURL reports whether a and b name the same hub: equal after
// config.NormalizeEndpoint (trailing slash, letter case in the scheme and
// host, and the default port do not matter).
func sameHubURL(a, b string) bool {
	return config.NormalizeEndpoint(a) == config.NormalizeEndpoint(b)
}
