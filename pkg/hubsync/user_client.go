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

package hubsync

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// CheckUserHubTransport applies the hub transport rule
// (config.CheckHubTransport) to a hub URL the CLI sends a credential to, or
// receives one from: an OAuth login, SCION_HUB_TOKEN, a dev token, an agent
// token or a transport (proxy) credential. The rule accepts https and wss,
// and http or ws to a literal loopback host only. The error wraps
// config.ErrPlaintextNonLoopback and shows the URL with its credentials
// redacted.
func CheckUserHubTransport(endpoint string) error {
	if err := config.CheckHubTransport(endpoint); err != nil {
		return &TransportRefusalError{Endpoint: config.RedactURL(endpoint), Err: err}
	}
	return nil
}

// TransportRefusalError is the refusal CheckUserHubTransport returns. Endpoint
// is the refused hub URL with its user info and credential query parameters
// redacted (config.RedactURL), so a caller can name where it came from and no
// later log or serialisation of the error can show a secret from the URL.
type TransportRefusalError struct {
	Endpoint string
	Err      error
}

func (e *TransportRefusalError) Error() string {
	return fmt.Sprintf("refusing to send hub credentials: %v", e.Err)
}

func (e *TransportRefusalError) Unwrap() error { return e.Err }

// NewUserHubClient is the hub client constructor of the CLI's user paths.
// It applies CheckUserHubTransport before anything else: a refused endpoint
// returns before any option is applied, so no credential header is built,
// no transport credential is fetched and no request is sent. There is no
// fallback to an unauthenticated client.
func NewUserHubClient(endpoint string, opts ...hubclient.Option) (hubclient.Client, error) {
	if err := CheckUserHubTransport(endpoint); err != nil {
		return nil, err
	}
	return hubclient.New(endpoint, opts...)
}

// injectedHubEndpointVars are the variables through which the Runtime Broker
// gives an agent container its hub URL.
var injectedHubEndpointVars = []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"}

// isInjectedHubEndpoint reports whether endpoint is exactly the hub URL the
// Runtime Broker injected into this agent container. No normalisation is
// done: another spelling of the same host does not match.
func isInjectedHubEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	for _, name := range injectedHubEndpointVars {
		if v := os.Getenv(name); v != "" && v == endpoint {
			return true
		}
	}
	return false
}

// isAgentHostAddress reports whether endpoint's host is an address through
// which an agent container reaches its own host: a literal loopback
// address, a host alias (config.IsAgentHostAliasHost) or an IPv4
// link-local address (169.254.0.0/16, as on cloudrun-sandbox). Names are
// never resolved. The loopback case never decides the exception for http or
// ws, which the transport rule already accepts for a loopback host (so
// NewInjectedAgentHubClient returns before calling this); it is kept so the
// list matches the broker side's container-to-host address set.
func isAgentHostAddress(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if config.IsLoopbackHost(host) || config.IsAgentHostAliasHost(host) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil && ip.IsLinkLocalUnicast()
}

// injectedAgentToken returns the agent token the Runtime Broker gave this
// container: the canonical token file, else SCION_AUTH_TOKEN (the order
// selectHubCredential uses).
func injectedAgentToken() string {
	if token := ReadAgentTokenFile(); token != "" {
		return token
	}
	return os.Getenv("SCION_AUTH_TOKEN")
}

// NewInjectedAgentHubClient returns a client that sends only the agent
// token, for the one case the transport rule does not cover: inside an agent
// container, the Runtime Broker chooses both the hub URL it injects (for
// example http://host.docker.internal:<port> on a workstation) and the
// agent token it mints for that hub, so the transport policy of that pair
// belongs to the broker.
//
// ok is true only when ALL of these hold:
//   - the transport rule refuses endpoint (otherwise use NewUserHubClient);
//   - this process runs in a hub-managed agent (config.IsHubManagedAgent);
//   - endpoint is exactly the injected SCION_HUB_ENDPOINT or SCION_HUB_URL
//     value;
//   - its host is a container-to-host address (isAgentHostAddress: a
//     loopback address, a host alias such as host.docker.internal, or an
//     IPv4 link-local address), never any other injected host;
//   - an agent token is present in the injected source (token file or
//     SCION_AUTH_TOKEN).
//
// The client sends the agent token and nothing else: the token is read here,
// not passed in, and the transport credential hubclient.New would add from
// the environment is suppressed. User credentials (OAuth login,
// SCION_HUB_TOKEN, dev token, transport credential, hub profile) never use
// this path. When ok is false the caller must refuse, normally through
// NewUserHubClient.
func NewInjectedAgentHubClient(endpoint string, timeout time.Duration) (c hubclient.Client, ok bool) {
	if config.CheckHubTransport(endpoint) == nil {
		return nil, false
	}
	if !config.IsHubManagedAgent() || !isInjectedHubEndpoint(endpoint) || !isAgentHostAddress(endpoint) {
		return nil, false
	}
	token := injectedAgentToken()
	if token == "" {
		return nil, false
	}
	c, err := hubclient.New(endpoint,
		hubclient.WithAgentToken(token),
		hubclient.WithTimeout(timeout),
		// Opt out of the transport credential New adds from the
		// environment: only the agent token crosses this connection.
		hubclient.WithTransportAuth(nil, transportauth.ModeFromEnv()),
	)
	if err != nil {
		return nil, false
	}
	return c, true
}
