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
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
)

// Credentials for hubs that the CLI picks itself ('scion use <project>'
// looking a project up across hubs or checking a known project,
// 'scion project list --refresh'). Such a hub gets only a credential bound
// to it: its own OAuth login, or dev auth on a local workstation hub.
// Credentials that are not tied to one hub (SCION_HUB_TOKEN,
// SCION_AUTH_TOKEN, the agent token file, the dev token for a remote hub,
// transport auth from the environment or from hub.transport in settings)
// are never sent to it; only the transport of its own hub list entry
// applies. A hub without a bound credential counts as not logged in and is
// not contacted. Only the hub the user chose (--hub, SCION_HUB_ENDPOINT or
// SCION_HUB_URL) or the effective hub (see GetHubEndpoint) keeps the full
// credential chain of getHubClient: see hubGetsFullChain.

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

// hubGetsFullChain reports whether the hub at endpoint may be sent every
// credential getHubClient would use: when the user chose it (explicit), or
// when it is the effective hub of this invocation.
func hubGetsFullChain(settings *config.Settings, endpoint string, explicit bool) bool {
	if explicit {
		return true
	}
	effective := GetHubEndpoint(settings)
	return effective != "" && sameHubURL(endpoint, effective)
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

// fanOutADCSource builds a transport token source from application default
// credentials. It is a variable so tests can substitute it.
var fanOutADCSource = adcsource.New

// fanOutTransport returns the transport auth for the hub at endpoint in a
// fan-out: only that hub's own hub list entry (its audience and mode),
// never the environment's transport token or the settings' hub.transport,
// which belong to the effective hub. It returns a nil source when the
// entry sets no audience.
func fanOutTransport(endpoint string) (transportauth.TokenSource, transportauth.HeaderMode, error) {
	t := hubEntryTransport(endpoint)
	if t == nil || t.Audience == "" {
		return nil, transportauth.HeaderAuthorization, nil
	}
	mode := transportauth.ModeFromString(t.Mode)
	if transportauth.IsOnGCEFunc() && !transportauth.IsMetadataRedirected(os.Getenv(transportauth.EnvMetadataMode)) {
		return transportauth.NewMetadataSource(t.Audience), mode, nil
	}
	src, err := fanOutADCSource(t.Audience)
	if err != nil {
		return nil, transportauth.HeaderAuthorization, err
	}
	return src, mode, nil
}

// hubClientFor returns a client for the hub at endpoint. In a fan-out it
// carries only the credential bound to that hub and only the transport of
// that hub's entry (see fanOutTransport), so no other credential can reach
// it; otherwise it is hubClientForEndpoint.
func hubClientFor(settings *config.Settings, endpoint string, fanOut bool) (hubclient.Client, error) {
	if !fanOut {
		return hubClientForEndpoint(settings, endpoint)
	}
	token := fanOutCredential(endpoint)
	if token == "" {
		return nil, errNoHubBoundCredential
	}
	src, mode, err := fanOutTransport(endpoint)
	if err != nil {
		return nil, err
	}
	return hubclient.New(endpoint,
		hubclient.WithBearerToken(token),
		hubclient.WithTransportAuth(src, mode),
		hubclient.WithTimeout(30*time.Second))
}
