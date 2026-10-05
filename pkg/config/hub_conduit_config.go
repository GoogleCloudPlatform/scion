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
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Conduit relay-peer authentication modes (server.hub.conduit.peer_auth).
const (
	ConduitPeerAuthAuto = "auto"
	ConduitPeerAuthOIDC = "oidc"
	ConduitPeerAuthHMAC = "hmac"
)

const (
	// ConduitGrantKeyMinActivation is the lowest accepted
	// grant_key_activation.
	ConduitGrantKeyMinActivation = time.Minute
	// ConduitGrantKeyNodeRefresh is the interval at which a hub node
	// reloads the grant key ring. grant_key_activation must be at least
	// this long, or a node could sign with a key other nodes' targets have
	// not been sent yet. It mirrors the hub's ring cache interval (a hub
	// test pins the two together).
	ConduitGrantKeyNodeRefresh = time.Minute
	// ConduitMaxReconnectWindow is the highest accepted reconnect_window.
	ConduitMaxReconnectWindow = 5 * time.Minute
)

// ConduitMaxInstanceIDLen is the longest accepted instance_id.
const ConduitMaxInstanceIDLen = 128

// HubConduitConfig holds the conduit settings of the hub server
// (server.hub.conduit). The values are kept as configured and checked by
// Validate at startup, so a malformed value is a startup error rather than
// a silently ignored one.
type HubConduitConfig struct {
	// GrantKeyActivation is the publish-before-sign delay of a new grant
	// key ("" = the hub default, 15m).
	GrantKeyActivation string `json:"grantKeyActivation,omitempty" yaml:"grantKeyActivation,omitempty" koanf:"grantKeyActivation"`
	// TCPAllowedPorts lists additional agent-local ports a TCP stream
	// grant may target besides the agent's exposed ports. The reserved
	// ports (9810, 18380) are always refused. Empty: exposed ports only.
	TCPAllowedPorts []int `json:"tcpAllowedPorts,omitempty" yaml:"tcpAllowedPorts,omitempty" koanf:"tcpAllowedPorts"`
	// InternalListen is the host:port of the internal relay API listener
	// ("" = none). It must be reachable only inside the cluster/VPC; TLS
	// on the internal hop is recommended (http:// is accepted).
	InternalListen string `json:"internalListen,omitempty" yaml:"internalListen,omitempty" koanf:"internalListen"`
	// InternalAdvertise is the base URL other nodes use to reach the
	// internal listener ("" = derived).
	InternalAdvertise string `json:"internalAdvertise,omitempty" yaml:"internalAdvertise,omitempty" koanf:"internalAdvertise"`
	// PeerAuth is "auto" (or ""), "oidc" or "hmac". Requests are signed in
	// every mode; oidc (and auto on GCP) also requires an OIDC ID token.
	PeerAuth string `json:"peerAuth,omitempty" yaml:"peerAuth,omitempty" koanf:"peerAuth"`
	// PeerServiceAccounts is the OIDC caller allow-list ("" = own SA).
	PeerServiceAccounts []string `json:"peerServiceAccounts,omitempty" yaml:"peerServiceAccounts,omitempty" koanf:"peerServiceAccounts"`
	// PeerAudience is the OIDC audience ("" = the fixed default).
	PeerAudience string `json:"peerAudience,omitempty" yaml:"peerAudience,omitempty" koanf:"peerAudience"`
	// ReconnectWindow is the jitter window sent as
	// GoAway.reconnect_after_ms on a planned close ("" = the relay
	// default, 5s; design v2.6 §3.3).
	ReconnectWindow string `json:"reconnectWindow,omitempty" yaml:"reconnectWindow,omitempty" koanf:"reconnectWindow"`
	// InstanceID is this node's relay instance id ("" = POD_NAME, else
	// the host name plus a random per-process suffix). It must be unique
	// among live hub processes.
	InstanceID string `json:"instanceId,omitempty" yaml:"instanceId,omitempty" koanf:"instanceId"`
}

// IsZero reports whether nothing is configured.
func (c HubConduitConfig) IsZero() bool {
	return c.GrantKeyActivation == "" && len(c.TCPAllowedPorts) == 0 && c.InternalListen == "" &&
		c.InternalAdvertise == "" && c.PeerAuth == "" && len(c.PeerServiceAccounts) == 0 && c.PeerAudience == "" &&
		c.ReconnectWindow == "" && c.InstanceID == ""
}

// GrantKeyActivationDuration parses GrantKeyActivation ("" = 0, meaning
// the hub default). It applies the same bounds as Validate.
func (c HubConduitConfig) GrantKeyActivationDuration() (time.Duration, error) {
	if c.GrantKeyActivation == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.GrantKeyActivation)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.conduit.grant_key_activation %q: %w", c.GrantKeyActivation, err)
	}
	minimum := ConduitGrantKeyMinActivation
	if ConduitGrantKeyNodeRefresh > minimum {
		minimum = ConduitGrantKeyNodeRefresh
	}
	if d < minimum {
		return 0, fmt.Errorf("invalid server.hub.conduit.grant_key_activation %q: must be at least %s (the minimum activation and the grant key refresh interval)", c.GrantKeyActivation, minimum)
	}
	return d, nil
}

// ReconnectWindowDuration parses ReconnectWindow ("" = 0, meaning the
// relay default). It applies the same bounds as Validate.
func (c HubConduitConfig) ReconnectWindowDuration() (time.Duration, error) {
	if c.ReconnectWindow == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.ReconnectWindow)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.conduit.reconnect_window %q: %w", c.ReconnectWindow, err)
	}
	if d < 0 || d > ConduitMaxReconnectWindow {
		return 0, fmt.Errorf("invalid server.hub.conduit.reconnect_window %q: must be between 0s and %s", c.ReconnectWindow, ConduitMaxReconnectWindow)
	}
	return d, nil
}

// PeerAuthMode returns the normalized peer-auth mode ("" = auto).
func (c HubConduitConfig) PeerAuthMode() string {
	if c.PeerAuth == "" {
		return ConduitPeerAuthAuto
	}
	return strings.ToLower(c.PeerAuth)
}

// Validate checks every conduit setting and returns all problems found.
func (c HubConduitConfig) Validate() error {
	var errs []error
	if _, err := c.GrantKeyActivationDuration(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.ReconnectWindowDuration(); err != nil {
		errs = append(errs, err)
	}
	seen := map[int]bool{}
	for _, p := range c.TCPAllowedPorts {
		switch {
		case p < 1 || p > 65535:
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.tcp_allowed_ports: port %d is outside 1-65535", p))
		case seen[p]:
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.tcp_allowed_ports: port %d is listed twice", p))
		}
		seen[p] = true
	}
	if c.InternalListen != "" {
		_, port, err := net.SplitHostPort(c.InternalListen)
		if err == nil {
			var n int
			n, err = strconv.Atoi(port)
			if err == nil && (n < 0 || n > 65535) {
				err = errors.New("port out of range")
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.internal_listen %q: want host:port: %v", c.InternalListen, err))
		}
	}
	if c.InternalAdvertise != "" {
		u, err := url.Parse(c.InternalAdvertise)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.internal_advertise %q: want http(s)://host:port", c.InternalAdvertise))
		}
	}
	switch c.PeerAuthMode() {
	case ConduitPeerAuthAuto, ConduitPeerAuthOIDC, ConduitPeerAuthHMAC:
	default:
		errs = append(errs, fmt.Errorf("invalid server.hub.conduit.peer_auth %q: want auto, oidc or hmac", c.PeerAuth))
	}
	for _, sa := range c.PeerServiceAccounts {
		if !strings.Contains(sa, "@") || strings.TrimSpace(sa) != sa || strings.ContainsAny(sa, ", \t") {
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.peer_service_accounts entry %q: want a service-account email", sa))
		}
	}
	if id := c.InstanceID; id != "" {
		if len(id) > ConduitMaxInstanceIDLen || strings.IndexFunc(id, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
			errs = append(errs, fmt.Errorf("invalid server.hub.conduit.instance_id %q: want at most %d printable ASCII characters without spaces", id, ConduitMaxInstanceIDLen))
		}
	}
	return errors.Join(errs...)
}
