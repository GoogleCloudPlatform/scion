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
	"fmt"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// The CLI's transport rule for user hub connections. Every hub client the
// CLI builds for a user credential (OAuth login, SCION_HUB_TOKEN, dev token,
// agent token, transport credential) is built by newUserHubClient, and every
// WebSocket or raw hub connection is preceded by checkUserHubTransport. Both
// apply config.CheckHubTransport (through pkg/hubsync) before any credential
// is read or attached and before anything is sent. The only exception is
// hubsync.NewInjectedAgentHubClient (see hubClientAndAuthSourceForEndpoint).
// TestHubClientConstructionGoesThroughTransportRule keeps new hub clients in
// cmd from bypassing this.

// newUserHubClient is hubsync.NewUserHubClient with a refusal that names
// where the endpoint came from.
func newUserHubClient(endpoint string, opts ...hubclient.Option) (hubclient.Client, error) {
	c, err := hubsync.NewUserHubClient(endpoint, opts...)
	if err != nil {
		return nil, hubTransportRefusal(endpoint, err)
	}
	return c, nil
}

// checkUserHubTransport applies the transport rule to endpoint for a hub
// connection that does not go through a hub client (WebSocket attach and
// tunnel dials, raw requests). Call it before resolving any credential.
func checkUserHubTransport(endpoint string) error {
	if err := hubsync.CheckUserHubTransport(endpoint); err != nil {
		return hubTransportRefusal(endpoint, err)
	}
	return nil
}

// hubTransportRefusal adds the origin of the refused endpoint to a transport
// refusal; any other error is returned unchanged. The refused endpoint is the
// one the refusal carries (hubsync.TransportRefusalError), else endpoint.
func hubTransportRefusal(endpoint string, err error) error {
	if err == nil || !errors.Is(err, config.ErrPlaintextNonLoopback) {
		return err
	}
	var refused *hubsync.TransportRefusalError
	if errors.As(err, &refused) && refused.Endpoint != "" {
		endpoint = refused.Endpoint
	}
	return fmt.Errorf("hub endpoint from %s: %w", hubEndpointOrigin(endpoint), err)
}

// hubEndpointOrigin names the setting endpoint came from: the source of
// this invocation's hub (--hub flag, SCION_HUB_ENDPOINT or SCION_HUB_URL,
// directory link, current context) when it is that hub, else the hub list
// entry with that endpoint, else a generic label. endpoint may be redacted
// (hubsync.TransportRefusalError carries the redacted form), so both sides are
// compared redacted; hub list entries hold no user info or query.
func hubEndpointOrigin(endpoint string) string {
	endpoint = config.RedactURL(endpoint)
	if h := resolveCLIHub(); h.Endpoint != "" && config.RedactURL(h.Endpoint) == endpoint {
		if h.Source == useSourceContext || h.Source == useSourceDirectory {
			if name := hubListEntryName(endpoint); name != "" {
				return h.Label + " (hub list entry " + strconv.Quote(name) + ")"
			}
		}
		return h.Label
	}
	if name := hubListEntryName(endpoint); name != "" {
		return "hub list entry " + strconv.Quote(name)
	}
	return "the hub selected for this command"
}

// hubListEntryName returns the name of the hub list entry for endpoint, or
// "" when there is none or the hub list cannot be read.
func hubListEntryName(endpoint string) string {
	c, err := config.LoadCLIContext()
	if err != nil || c == nil {
		return ""
	}
	name, h := c.HubByEndpoint(endpoint)
	if h == nil {
		return ""
	}
	return name
}
