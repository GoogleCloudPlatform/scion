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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// isLocalWorkstationEndpoint reports whether endpoint is the local
// workstation hub: a loopback host that the CLI reaches with the dev token.
// See hubsync.IsLocalWorkstationEndpoint.
func isLocalWorkstationEndpoint(endpoint string) bool {
	return hubsync.IsLocalWorkstationEndpoint(endpoint)
}

// errHubLinkNoEndpoint is returned by 'scion hub link' when no hub endpoint
// is configured: the same not-configured error every hub command returns.
var errHubLinkNoEndpoint = hubsync.ErrHubNotConfigured

// hubConfigured reports whether a hub is configured for this invocation:
// an endpoint resolves from --hub, the settings or the environment (see
// GetHubEndpoint). Local-only mode was removed, so a configured endpoint is
// what makes the CLI use a hub; the deprecated hub.enabled is not read.
// Every cmd "is a hub configured?" check goes through this one function, so
// that a later change to where hub configuration comes from replaces only
// its body.
func hubConfigured(settings interface{ GetHubEndpoint() string }) bool {
	return GetHubEndpoint(settings) != ""
}

// deprecatedHubModeKeys lists the removed mode switches that settings still
// set: hub.enabled and hub.local_only are deprecated and ignored since
// local-only mode was removed. 'scion hub status' notes them.
func deprecatedHubModeKeys(settings *config.Settings) []string {
	if settings == nil || settings.Hub == nil {
		return nil
	}
	var keys []string
	if settings.Hub.Enabled != nil {
		keys = append(keys, "hub.enabled")
	}
	if settings.Hub.LocalOnly != nil {
		keys = append(keys, "hub.local_only")
	}
	return keys
}

// yesNo renders b as "yes" or "no".
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
