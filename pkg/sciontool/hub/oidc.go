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
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

const (
	// EnvHubOIDCAudience overrides the audience claim in the OIDC identity token.
	EnvHubOIDCAudience = transportauth.EnvHubOIDCAudience

	// EnvTransportToken is the env var for the hub-provided transport OIDC token.
	EnvTransportToken = transportauth.EnvTransportToken

	// EnvTransportAudience is the env var for the transport token audience.
	EnvTransportAudience = transportauth.EnvTransportAudience
)

// configureOIDCTransport sets up the OIDC transport layer on the client.
// Token source selection:
//  1. If SCION_TRANSPORT_TOKEN env var is set → injected mode (hub-provided token).
//  2. Else if running on GCP → metadata server mode (ambient SA identity).
//  3. Else → no OIDC transport (agent uses plain HTTP).
//
// Unlike the generic transportauth.FromEnv(), this method defaults the
// metadata-mode audience to the hub URL when no explicit audience env var
// is set, preserving the PR #307 behaviour for agents.
func (c *Client) configureOIDCTransport() {
	if tok := os.Getenv(transportauth.EnvTransportToken); tok != "" {
		source := transportauth.NewInjectedSource()
		source.WarnLog = log.Debug
		expiry, err := transportauth.ParseTokenExpiry(tok)
		if err != nil {
			expiry = time.Now().Add(transportauth.DefaultTTL)
		}
		source.SetToken(tok, expiry)
		c.oidcSource = source
		c.client.Transport = transportauth.Wrap(c.client.Transport, source, transportauth.HeaderAuthorization)
		log.Debug("Configured OIDC transport: injected mode (hub-provided token)")
		return
	}

	if !transportauth.IsOnGCEFunc() {
		return
	}
	if mode := os.Getenv(transportauth.EnvMetadataMode); mode != "" {
		log.Debug("Skipping OIDC metadata mode: scion metadata server active (mode=%s), GCE metadata IP is redirected", mode)
		return
	}

	audience := os.Getenv(transportauth.EnvHubOIDCAudience)
	if audience == "" {
		audience = c.hubURL
	}

	source := transportauth.NewMetadataSource(audience)
	c.oidcSource = source
	c.client.Transport = transportauth.Wrap(c.client.Transport, source, transportauth.HeaderAuthorization)
	log.Debug("Configured OIDC transport: metadata mode (audience=%s)", audience)
}
