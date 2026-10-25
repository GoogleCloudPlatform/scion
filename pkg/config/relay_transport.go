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
	"fmt"
	"net"
	"strings"
)

// relayTransportFix is the remedy every relay internal hop refusal names.
const relayTransportFix = "serve the internal listener with TLS (server.hub.conduit.internal_tls_cert_file and " +
	"internal_tls_key_file; a private or internal CA also REQUIRES server.hub.conduit.internal_tls_ca_file on every node) " +
	"and use https://, or use a loopback address"

// CheckRelayTransport is the transport rule for the conduit relay internal
// hop: hub to hub, hub to relay, relay to hub and relay to relay, including
// the hub ingress upgrade. That hop carries user and agent credentials and
// session content, so it must be encrypted whenever it leaves the host.
//
// It applies the same accept rule as CheckHubTransport (https or wss, or
// http or ws to a literal loopback host; names are never resolved) and
// differs only in its message. The error wraps ErrPlaintextNonLoopback.
// There is no override.
func CheckRelayTransport(rawURL string) error {
	if err := CheckHubTransport(rawURL); err != nil {
		return fmt.Errorf("%w: conduit relay internal endpoint %s is not https:// and not loopback, so relay "+
			"traffic would cross the network unencrypted; %s",
			ErrPlaintextNonLoopback, RedactURL(rawURL), relayTransportFix)
	}
	return nil
}

// CheckRelayInternalListen is the listener half of the relay internal hop
// rule: a listener that does not serve TLS must bind a literal loopback
// host (127.0.0.0/8, ::1 or "localhost"). A wildcard bind (":p",
// "0.0.0.0:p", "[::]:p") or any other host is refused. A TLS listener may
// bind any address. key names the setting in the error. The error wraps
// ErrPlaintextNonLoopback.
func CheckRelayInternalListen(key, listen string, servesTLS bool) error {
	if servesTLS {
		return nil
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err == nil && host != "" && IsLoopbackHost(strings.ToLower(host)) {
		return nil
	}
	return fmt.Errorf("%w: %s %q is not a loopback address and the listener has no TLS certificate, so relay "+
		"traffic would cross the network unencrypted; %s (for example 127.0.0.1:<port> behind a same-host "+
		"TLS terminator advertised as https://)", ErrPlaintextNonLoopback, key, listen, relayTransportFix)
}
