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

package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrPlaintextNonLoopback is returned (wrapped) by CheckHubTransport for a
// hub or Runtime Broker URL that would carry traffic unencrypted to a host
// that is not loopback.
var ErrPlaintextNonLoopback = errors.New("unencrypted connection to a non-loopback host")

// CheckHubTransport is the transport rule for the channels between a hub and
// a Runtime Broker, in both directions: the hub URL a broker dials (join,
// register, heartbeat, control channel, conduit) and the broker endpoint the
// hub sends requests to. Those channels carry the broker's HMAC secret and
// the secrets of the agents it runs, so they must be encrypted whenever they
// leave the host.
//
// A URL is accepted when its scheme is https or wss, or when its scheme is
// http or ws and its host is loopback. Loopback is decided from the literal
// host only (IsLoopbackHost, after lower-casing the host: 127.0.0.0/8, ::1
// and the name "localhost"), as
// the web base URL check does (checkWebBaseURLListener in cmd): a name is
// never resolved, so a hosts-file or DNS entry cannot turn a network host
// into a loopback one. Every other URL is refused, including any other
// scheme, a URL that does not parse and a URL without a host. The error
// wraps ErrPlaintextNonLoopback and names the URL with its credentials
// redacted. There is no override: put the hub behind a TLS-terminating proxy
// and use its https URL, or reach it through a loopback port forward.
func CheckHubTransport(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		// A URL that does not parse is not echoed when it holds an '@':
		// RedactURL would mask only the part before the last '@', and what
		// is left (for example "@127.0.0.1") would read as the host.
		shown := RedactURL(rawURL)
		if strings.Contains(rawURL, "@") {
			shown = "the URL (not shown: it contains '@')"
		}
		return fmt.Errorf("%w: %s is not a valid URL with a host; use an https:// URL", ErrPlaintextNonLoopback, shown)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return nil
	case "http", "ws":
		if IsLoopbackHost(strings.ToLower(u.Hostname())) {
			return nil
		}
		return fmt.Errorf("%w: %s uses %s to host %q, which is not a loopback address, so broker credentials and agent secrets "+
			"would cross the network unencrypted; use an https:// URL (a TLS-terminating proxy in front of the hub), "+
			"or a loopback address such as an SSH port forward to http://127.0.0.1:<port>",
			ErrPlaintextNonLoopback, RedactURL(rawURL), strings.ToLower(u.Scheme), u.Hostname())
	default:
		return fmt.Errorf("%w: %s has scheme %q; use an https:// URL", ErrPlaintextNonLoopback, RedactURL(rawURL), u.Scheme)
	}
}
