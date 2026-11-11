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
	"strings"
)

// Settings keys of the proxy driver settings (ptone/scion#4240, phase WS2).
// Both are startup config only: every settings write path refuses them, in
// every mode, and the server-config GET does not show them.
const (
	// ProxyDriverPrincipalsKey is auth.proxy.driverPrincipals: the exact
	// emails of the browser drivers (for example a test-UI service account)
	// that may carry test identity web sessions through the proxy. A driver
	// principal never gets a hub account of its own.
	ProxyDriverPrincipalsKey = "server.auth.proxy.driver_principals"
	// ProxyDriverTokenAudienceKey is auth.proxy.driverTokenAudience: the
	// audience (the IAP OAuth client ID) a driver's Google ID token must
	// carry before a test identity web session may take the place of that
	// token on an API request.
	ProxyDriverTokenAudienceKey = "server.auth.proxy.driver_token_audience"
)

// ErrInvalidProxyDriverPrincipal marks a malformed driver_principals entry.
var ErrInvalidProxyDriverPrincipal = errors.New("invalid auth.proxy.driverPrincipals entry")

// ErrProxyDriverPrincipalsWithoutProxyAuth refuses a non-empty
// driver_principals list on a hub that is not in proxy auth mode, where it
// has no meaning.
var ErrProxyDriverPrincipalsWithoutProxyAuth = errors.New("auth.proxy.driverPrincipals is set but auth.mode is not \"proxy\"; driver principals apply only behind an authenticating proxy")

// NormalizeProxyDriverPrincipals checks the driver_principals list and
// returns it trimmed and lower-cased. Every entry must be one exact email
// address: no wildcards or patterns, no whitespace, exactly one "@" with a
// non-empty local part and domain, and no entry may appear twice (after
// lower-casing). Any problem returns ErrInvalidProxyDriverPrincipal; the
// hub refuses to start on it.
func NormalizeProxyDriverPrincipals(principals []string) ([]string, error) {
	if len(principals) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(principals))
	seen := make(map[string]bool, len(principals))
	for i, p := range principals {
		email := strings.ToLower(strings.TrimSpace(p))
		if reason := proxyDriverPrincipalProblem(email); reason != "" {
			return nil, fmt.Errorf("%w: %s[%d] %s", ErrInvalidProxyDriverPrincipal, ProxyDriverPrincipalsKey, i, reason)
		}
		if seen[email] {
			return nil, fmt.Errorf("%w: %s[%d] repeats an earlier entry", ErrInvalidProxyDriverPrincipal, ProxyDriverPrincipalsKey, i)
		}
		seen[email] = true
		out = append(out, email)
	}
	return out, nil
}

// proxyDriverPrincipalProblem returns why email is not one exact address,
// or "".
func proxyDriverPrincipalProblem(email string) string {
	if email == "" {
		return "is empty"
	}
	if strings.ContainsAny(email, "*?[]{}") {
		return "must be an exact email address (wildcards and patterns are not accepted)"
	}
	for _, r := range email {
		if r <= ' ' || r == 0x7f {
			return "must not contain whitespace or control characters"
		}
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') || at == len(email)-1 {
		return "must be an exact email address with one @, a local part and a domain"
	}
	return ""
}

// ValidateProxyDriverConfig checks the proxy driver settings against the
// auth mode and returns the normalized principal list. A non-empty list on
// a hub whose auth mode is not "proxy" returns
// ErrProxyDriverPrincipalsWithoutProxyAuth. An empty list in proxy mode is
// accepted: test identity web sessions are then unavailable on the hub,
// which the hub logs and reports in its status.
func ValidateProxyDriverConfig(authMode string, principals []string) ([]string, error) {
	norm, err := NormalizeProxyDriverPrincipals(principals)
	if err != nil {
		return nil, err
	}
	if len(norm) > 0 && authMode != "proxy" {
		return nil, ErrProxyDriverPrincipalsWithoutProxyAuth
	}
	return norm, nil
}

// ProxyDriverPrincipals returns the configured driver_principals list
// (unvalidated), or nil.
func (c *ProxyAuthConfig) ProxyDriverPrincipals() []string {
	if c == nil {
		return nil
	}
	return c.DriverPrincipals
}

// ProxyDriverTokenAudience returns the configured driver_token_audience
// (trimmed), or "".
func (c *ProxyAuthConfig) ProxyDriverTokenAudience() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.DriverTokenAudience)
}
