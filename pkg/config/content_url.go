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

	"golang.org/x/net/publicsuffix"
)

// ContentURLKey is the settings key of the hub's content host
// (ptone/scion#7107 phase A): a second public address, apart from the
// sign-in origin server.hub.public_url, on which shared artifacts are
// served. Every message, the token classification and the audit name the
// key through this constant.
const ContentURLKey = "server.hub.content_url"

// ValidateContentURL checks a server.hub.content_url value and returns it
// normalized, or "" for an empty value (no content host).
//
// A valid value is an origin, scheme://host[:port]: scheme https, or http
// only for a loopback host; a host name of letters, digits, '.' and '-', or
// an IPv4 address; no userinfo, path, query or fragment (a lone trailing
// "/" is accepted and dropped). The result has a lower-cased scheme and
// host and omits the scheme's default port, so two spellings of one origin
// normalize to the same string. IPv6 literals are refused: the view CSP
// names the origin as a host-source, which cannot carry one.
//
// This is the value's own shape only. Whether it shares a host name or a
// registrable domain with the hub's public URL is ContentURLHubConflict,
// which needs the hub's endpoint.
func ValidateContentURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL", ContentURLKey)
	}
	if u.Opaque != "" || u.Host == "" {
		return "", fmt.Errorf("%s: must be an origin of the form https://host[:port]", ContentURLKey)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%s: must be an https origin", ContentURLKey)
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", fmt.Errorf("%s: must not contain user credentials", ContentURLKey)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%s: must not contain a query string", ContentURLKey)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s: must not contain a fragment", ContentURLKey)
	}
	if (u.Path != "" && u.Path != "/") || (u.RawPath != "" && u.RawPath != "/") {
		return "", fmt.Errorf("%s: must not contain a path", ContentURLKey)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%s: must include a host", ContentURLKey)
	}
	if strings.Contains(host, ":") || strings.HasPrefix(u.Host, "[") {
		return "", fmt.Errorf("%s: an IPv6 address is not supported; use a host name", ContentURLKey)
	}
	if !validContentURLHost(host) {
		return "", fmt.Errorf("%s: host must be a host name of letters, digits, '.' and '-', or an IPv4 address", ContentURLKey)
	}
	if scheme == "http" && !IsLoopbackHost(host) {
		return "", fmt.Errorf("%s: must use https; http is accepted only for localhost or a loopback address", ContentURLKey)
	}
	authority := host
	if portStr := u.Port(); portStr != "" || strings.HasSuffix(u.Host, ":") {
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 || portStr != strconv.Itoa(port) {
			return "", fmt.Errorf("%s: port must be a number between 1 and 65535", ContentURLKey)
		}
		if port != contentURLDefaultPort(scheme) {
			authority = host + ":" + portStr
		}
	}
	return scheme + "://" + authority, nil
}

// validContentURLHost accepts a host name made of letters, digits, '.' and
// '-' (the characters the view CSP accepts in a host-source), with no empty
// label and no label starting or ending with '-'.
func validContentURLHost(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func contentURLDefaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}

// ContentURLParts is a valid content URL split for matching a request Host:
// the lower-cased host name, the effective port (explicit, or the scheme's
// default), and the scheme's default port.
type ContentURLParts struct {
	Host, Port, DefaultPort string
}

// ParseContentURL parses a content URL by the rules of ValidateContentURL
// and splits it. It is the one parse every reader of server.hub.content_url
// uses (validation, the hub conflict check and the request match), so they
// cannot disagree. An empty or invalid value yields ok false.
func ParseContentURL(raw string) (ContentURLParts, bool) {
	n, err := ValidateContentURL(raw)
	if err != nil || n == "" {
		return ContentURLParts{}, false
	}
	scheme, rest, _ := strings.Cut(n, "://")
	def := strconv.Itoa(contentURLDefaultPort(scheme))
	host, port, hasPort := strings.Cut(rest, ":")
	if !hasPort {
		port = def
	}
	return ContentURLParts{Host: host, Port: port, DefaultPort: def}, true
}

// ContentURLHubConflict compares a content URL with the hub's public URL.
// sameHost is true when the content URL's host name is the hub's own host
// name (case-insensitive, a trailing dot ignored), whatever the scheme and
// port: such a value is refused on save and ignored at runtime, since the
// hub would confine its own host to the content routes, and a second port
// on the same host gives no cookie isolation. sameSite is true when both
// hosts share a registrable domain (eTLD+1, best effort from the public
// suffix list): such a value is accepted with a warning, because a content
// host under the hub's site isolates little. An invalid content URL, or a
// hub URL with no host, conflicts with nothing.
func ContentURLHubConflict(contentURL, hubPublicURL string) (sameHost, sameSite bool) {
	c, ok := ParseContentURL(contentURL)
	if !ok {
		return false, false
	}
	hub := urlHostName(hubPublicURL)
	if hub == "" {
		return false, false
	}
	if c.Host == hub {
		return true, true
	}
	return false, sameRegistrableDomain(c.Host, hub)
}

// ContentURLSameHost reports whether a content URL's host name is the host
// name of otherURL (case-insensitive, a trailing dot ignored), whatever the
// scheme and port. The hub refuses, and ignores at runtime, a content URL on
// the host name of server.hub.agent_endpoint: the content host's guard
// would confine that host, so every agent API request there would answer
// 404. An invalid content URL, or an otherURL with no host, matches nothing.
func ContentURLSameHost(contentURL, otherURL string) bool {
	c, ok := ParseContentURL(contentURL)
	if !ok {
		return false
	}
	other := urlHostName(otherURL)
	return other != "" && c.Host == other
}

// RequestOnHubHost reports whether a request Host (host[:port], as the hub
// receives it) names the hub's own host name, whatever the port. A content
// URL on that host name is never used (ContentURLHubConflict), so such a
// request is never a content-host request.
func RequestOnHubHost(reqHost, hubPublicURL string) bool {
	hub := urlHostName(hubPublicURL)
	if hub == "" || reqHost == "" {
		return false
	}
	host := reqHost
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".") == hub
}

// urlHostName returns the lower-cased host name of an absolute URL, a
// trailing dot dropped, or "" when it has none.
func urlHostName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// sameRegistrableDomain reports whether two host names share an eTLD+1.
// IP addresses and names the public suffix list cannot place share none,
// except that identical hosts always do.
func sameRegistrableDomain(a, b string) bool {
	if a == b {
		return true
	}
	if net.ParseIP(a) != nil || net.ParseIP(b) != nil {
		return false
	}
	ra, errA := publicsuffix.EffectiveTLDPlusOne(a)
	rb, errB := publicsuffix.EffectiveTLDPlusOne(b)
	return errA == nil && errB == nil && ra == rb
}
