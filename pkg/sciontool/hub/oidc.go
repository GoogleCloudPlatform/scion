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
	"net/http"
	"os"

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
//  1. If SCION_TRANSPORT_TOKEN_FILE or SCION_TRANSPORT_TOKEN is set (the
//     agent was given a hub-provided transport token) → file-backed mode.
//     The file (SCION_TRANSPORT_TOKEN_FILE, else the scion user's
//     ~/.scion/transport-token) is re-read when it changes, with the env
//     value as bootstrap fallback; whichever expires last is used, so
//     short-lived clients see the value the long-lived client in sciontool
//     init last refreshed. The file alone does not select this mode.
//  2. Else if running on GCP → metadata server mode (ambient SA identity).
//  3. Else → no OIDC transport (agent uses plain HTTP).
//
// The header carrying the token follows SCION_TRANSPORT_MODE (see
// transportauth.ModeFromEnv), the same as hubclient: "iap" uses
// Proxy-Authorization, "cloudrun_invoker" X-Serverless-Authorization, and
// anything else Authorization.
//
// Unlike the generic transportauth.FromEnv(), this method defaults the
// metadata-mode audience to the hub URL when no explicit audience env var
// is set, preserving the PR #307 behaviour for agents.
func (c *Client) configureOIDCTransport() {
	mode := transportauth.ModeFromEnv()
	if src := newTransportFileSource(); src != nil {
		src.WarnLog = log.Debug
		c.oidcSource = src
		c.oidcMode = mode
		c.client.Transport = transportauth.Wrap(c.client.Transport, src, mode)
		log.Debug("Configured OIDC transport: injected mode (hub-provided token, file-backed)")
		return
	}

	if !transportauth.IsOnGCEFunc() {
		return
	}
	if mdMode := os.Getenv(transportauth.EnvMetadataMode); transportauth.IsMetadataRedirected(mdMode) {
		log.Debug("Skipping OIDC metadata mode: scion metadata server active (mode=%s), GCE metadata IP is redirected", mdMode)
		return
	}

	audience := os.Getenv(transportauth.EnvHubOIDCAudience)
	if audience == "" {
		audience = c.hubURL
	}

	source := transportauth.NewMetadataSource(audience)
	c.oidcSource = source
	c.oidcMode = mode
	c.client.Transport = transportauth.Wrap(c.client.Transport, source, mode)
	log.Debug("Configured OIDC transport: metadata mode (audience=%s)", audience)
}

// newTransportFileSource returns a file-backed transport source when this
// agent was given a hub-provided transport token, signalled the same way
// transportauth.FromEnv detects it: SCION_TRANSPORT_TOKEN_FILE (set by
// sciontool init for itself and its children) or the bootstrap
// SCION_TRANSPORT_TOKEN value. The file path is SCION_TRANSPORT_TOKEN_FILE
// when set, else the default under the agent home. Returns nil otherwise,
// so a transport token file left over from an earlier configuration is
// not used once the hub stops providing one.
func newTransportFileSource() *transportauth.FileSource {
	envTok := os.Getenv(transportauth.EnvTransportToken)
	path := os.Getenv(transportauth.EnvTransportTokenFile)
	if envTok == "" && path == "" {
		return nil
	}
	if path == "" {
		path = TransportTokenFilePath()
	}
	src := transportauth.NewFileSource(path, readTransportTokenFile)
	src.SetBootstrap(envTok)
	return src
}

// TransportSourceStatus reports the state of the client's file-backed
// transport source for diagnostics. ok is false when the client is not
// using a hub-provided transport token. Never includes token values.
func (c *Client) TransportSourceStatus() (transportauth.FileSourceStatus, bool) {
	if c == nil {
		return transportauth.FileSourceStatus{}, false
	}
	fs, ok := c.oidcSource.(*transportauth.FileSource)
	if !ok {
		return transportauth.FileSourceStatus{}, false
	}
	return fs.Status(), true
}

// ApplyTransportHeaders sets the transport credential header on h, for
// connections that do not go through the client's http.Transport (such as
// WebSocket dials). It is a no-op when no transport source is configured.
func (c *Client) ApplyTransportHeaders(h http.Header) error {
	if c == nil || c.oidcSource == nil {
		return nil
	}
	return transportauth.ApplyHeaders(h, c.oidcSource, c.oidcMode)
}

// SetTransportAuth installs src as the client's transport credential
// source, sent in the header selected by mode. NewClient configures this
// automatically; it is for clients built with NewClientWithConfig.
func (c *Client) SetTransportAuth(src transportauth.TokenSource, mode transportauth.HeaderMode) {
	c.oidcSource = src
	c.oidcMode = mode
	c.client.Transport = transportauth.Wrap(c.client.Transport, src, mode)
}
