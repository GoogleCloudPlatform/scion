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
	"net/url"
	"strconv"
	"strings"
)

// PublicURLKey is the settings key of the hub's public URL.
const PublicURLKey = "server.hub.public_url"

// ValidatePublicURL checks a server.hub.public_url value saved in the
// settings database and returns its normalized form. An empty string is
// valid (unset) and returns "".
//
// A non-empty value must be an absolute URL of the form scheme://host[:port]:
//   - scheme https, or http only when the host is a loopback address or
//     localhost (IsLoopbackHost; http on any other host is rejected);
//   - a host that is an IP literal or a hostname of letters, digits, '_',
//     '-' and '.', with no IPv6 zone;
//   - no user credentials, query or fragment;
//   - no path other than "/" (the hub serves from the root of its URL);
//   - a port, if any, between 1 and 65535.
//
// The normalized form has a lower-case scheme and no trailing slash.
//
// This rule applies to the database value only. Values from the environment,
// startup flags or settings.yaml keep their existing handling. No error
// message echoes any part of raw.
func ValidatePublicURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL", PublicURLKey)
	}
	if u.Opaque != "" || u.Host == "" {
		return "", fmt.Errorf("%s: must be an absolute URL of the form scheme://host[:port]", PublicURLKey)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%s: must be an https URL", PublicURLKey)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s: must not contain user credentials", PublicURLKey)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%s: must not contain a query string", PublicURLKey)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s: must not contain a fragment", PublicURLKey)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%s: must not contain a path", PublicURLKey)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("%s: must include a host", PublicURLKey)
	}
	if strings.Contains(host, "%") {
		return "", fmt.Errorf("%s: host must not contain an IPv6 zone", PublicURLKey)
	}
	if net.ParseIP(host) == nil && !isValidAgentEndpointHostname(host) {
		return "", fmt.Errorf("%s: host must be an IP address or a hostname of letters, digits, '_', '-', and '.'", PublicURLKey)
	}
	if scheme == "http" && !IsLoopbackHost(host) {
		return "", fmt.Errorf("%s: must use https; http is accepted only for localhost or a loopback address", PublicURLKey)
	}
	authority := host
	if portStr := u.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("%s: port must be between 1 and 65535", PublicURLKey)
		}
		authority = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return scheme + "://" + authority, nil
}

// IsLoopbackHost reports whether host is a loopback address (127.0.0.0/8,
// ::1, or "localhost"). It returns false for non-loopback IPs (including
// "0.0.0.0" and "::") and for unresolvable hostnames. It is the one loopback
// rule shared by the public URL check and the dev-auth bind check
// (hub.IsLoopbackHost).
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
