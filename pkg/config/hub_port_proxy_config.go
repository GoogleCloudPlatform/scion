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
	"time"
)

const (
	// PortProxyDefaultResponseHeaderTimeout is the default bound on the
	// wait for an agent port's response headers on the hub agent port
	// proxy (server.hub.port_proxy.response_header_timeout).
	PortProxyDefaultResponseHeaderTimeout = 60 * time.Second
	// PortProxyMinResponseHeaderTimeout and PortProxyMaxResponseHeaderTimeout
	// bound response_header_timeout.
	PortProxyMinResponseHeaderTimeout = 5 * time.Second
	PortProxyMaxResponseHeaderTimeout = 10 * time.Minute
)

// HubPortProxyConfig holds the agent port proxy settings of the hub server
// (server.hub.port_proxy). The values are kept as configured and checked by
// Validate at startup; an invalid value is a startup error.
type HubPortProxyConfig struct {
	// ResponseHeaderTimeout bounds the wait for the upstream response
	// headers of a proxied request, the WebSocket handshake included
	// ("" = PortProxyDefaultResponseHeaderTimeout). The body is not
	// time-limited.
	ResponseHeaderTimeout string `json:"responseHeaderTimeout,omitempty" yaml:"responseHeaderTimeout,omitempty" koanf:"responseHeaderTimeout"`
}

// IsZero reports whether no port proxy setting is configured.
func (c HubPortProxyConfig) IsZero() bool { return c.ResponseHeaderTimeout == "" }

// ResponseHeaderTimeoutDuration parses ResponseHeaderTimeout
// ("" = PortProxyDefaultResponseHeaderTimeout). Zero is not accepted.
func (c HubPortProxyConfig) ResponseHeaderTimeoutDuration() (time.Duration, error) {
	if c.ResponseHeaderTimeout == "" {
		return PortProxyDefaultResponseHeaderTimeout, nil
	}
	d, err := time.ParseDuration(c.ResponseHeaderTimeout)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.port_proxy.response_header_timeout %q: %w", c.ResponseHeaderTimeout, err)
	}
	if d < PortProxyMinResponseHeaderTimeout || d > PortProxyMaxResponseHeaderTimeout {
		return 0, fmt.Errorf("invalid server.hub.port_proxy.response_header_timeout %q: must be between %s and %s", c.ResponseHeaderTimeout, PortProxyMinResponseHeaderTimeout, PortProxyMaxResponseHeaderTimeout)
	}
	return d, nil
}

// Validate checks every port proxy setting.
func (c HubPortProxyConfig) Validate() error {
	_, err := c.ResponseHeaderTimeoutDuration()
	return err
}
