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

package hub

import (
	"net"
	"net/http"
	"strings"
)

// trustedClientIP returns the client address for rate limiting and audit.
// Forwarded headers are honoured only when the immediate peer is in trusted.
// X-Forwarded-For is read right to left across all header lines; the first
// hop not in trusted is returned. A malformed entry stops the walk and
// falls back to the peer address.
func trustedClientIP(r *http.Request, trusted []*net.IPNet) string {
	rawPeer := remoteIP(r.RemoteAddr)
	peerIP := net.ParseIP(rawPeer)
	peer := normalizeIP(rawPeer)
	if len(trusted) == 0 || peerIP == nil || !ipIn(peerIP, trusted) {
		return peer
	}

	hops := splitXFF(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return peer
		}
		if !ipIn(ip, trusted) {
			return normalizeIP(hops[i])
		}
	}
	return peer
}

// clientIP returns the request's client address using the server's parsed
// trusted proxy list.
func (s *Server) clientIP(r *http.Request) string {
	return trustedClientIP(r, s.clientIPTrustedNets)
}

// splitXFF splits every X-Forwarded-For header line on commas and trims
// whitespace, preserving hop order across lines.
func splitXFF(lines []string) []string {
	var hops []string
	for _, line := range lines {
		for _, part := range strings.Split(line, ",") {
			hops = append(hops, strings.TrimSpace(part))
		}
	}
	return hops
}

// ipIn reports whether ip is contained in any network in nets.
func ipIn(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteIP extracts the host portion of r.RemoteAddr, returning remoteAddr
// unchanged when it has no port.
func remoteIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ip
}

// normalizeIP normalizes IPv4/IPv6 addresses to a canonical string form.
// IPv4-mapped IPv6 addresses (::ffff:1.2.3.4) are normalized to IPv4.
// Unparseable inputs are returned unchanged.
func normalizeIP(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
