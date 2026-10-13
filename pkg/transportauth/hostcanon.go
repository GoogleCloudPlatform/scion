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

package transportauth

import (
	"net"
	"strings"
)

// CanonicalHost returns the canonical form of a URL host name (as returned
// by url.URL.Hostname, without brackets or port): lowercase, one trailing
// "." removed, and an IP literal in its canonical text form (an
// IPv4-mapped IPv6 address becomes dotted IPv4). Callers parse first and
// canonicalize the parsed host, never a raw string.
func CanonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// IsLoopbackHost reports whether host, a parsed host name (no brackets or
// port), is a loopback host: exactly "localhost", or an IP for which
// net.IP.IsLoopback is true (127.0.0.0/8, ::1, an IPv4-mapped loopback). It
// is false for everything else, including "0.0.0.0", "::", "hub.localhost"
// and "localhost." (a caller that accepts a trailing dot canonicalizes with
// CanonicalHost first, as a separate step).
//
// It mirrors pkg/hub.IsLoopbackHost and the config.IsLoopbackHost helper of
// the W4b settings work, which is not on upstream main yet. TODO: replace
// this copy with config.IsLoopbackHost once it lands, so there is one rule.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
