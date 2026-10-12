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
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Credentials for contacting several hubs at once ('scion use <project>'
// looking a project up across hubs, 'scion project list --refresh'). Such a
// fan-out sends each hub only a credential bound to that hub: its own OAuth
// login, or dev auth on a local workstation hub. Tokens that are not tied
// to one hub (SCION_HUB_TOKEN, SCION_AUTH_TOKEN, the agent token file, the
// dev token for a remote hub) are never sent in a fan-out; a hub without a
// bound credential counts as not logged in and is not contacted. A single
// hub (--hub, SCION_HUB_ENDPOINT, the effective hub) keeps the full
// credential chain of getHubClient.

// fanOutLocalHub reports whether endpoint is a local workstation hub, where
// dev auth applies. It is a variable so tests can substitute it: their fake
// hubs are all local.
var fanOutLocalHub = isLocalhostEndpoint

// errNoHubBoundCredential is returned for a hub that has no credential
// bound to it in a fan-out.
var errNoHubBoundCredential = errors.New("no login for this hub")

// fanOutCredential returns the credential bound to the hub at endpoint: its
// OAuth login, else the dev token for a local workstation hub, else "".
func fanOutCredential(endpoint string) string {
	if token := credentials.GetAccessToken(endpoint); token != "" {
		return token
	}
	if fanOutLocalHub(endpoint) {
		return apiclient.ResolveDevToken()
	}
	return ""
}

// hubUsable reports whether the hub at endpoint can be contacted: in a
// fan-out only with a credential bound to it, otherwise with any credential
// getHubClient would use.
func hubUsable(settings *config.Settings, endpoint string, fanOut bool) bool {
	if fanOut {
		return fanOutCredential(endpoint) != ""
	}
	return getAuthInfo(settings, endpoint).MethodType != "none"
}

// hubClientFor returns a client for the hub at endpoint. In a fan-out it
// carries only the credential bound to that hub, so no other credential
// can reach it; otherwise it is hubClientForEndpoint.
func hubClientFor(settings *config.Settings, endpoint string, fanOut bool) (hubclient.Client, error) {
	if !fanOut {
		return hubClientForEndpoint(settings, endpoint)
	}
	token := fanOutCredential(endpoint)
	if token == "" {
		return nil, errNoHubBoundCredential
	}
	return hubclient.New(endpoint, hubclient.WithBearerToken(token), hubclient.WithTimeout(30*time.Second))
}
