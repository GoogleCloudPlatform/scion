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

// Narrowest prefix lengths ParseTrustedProxies accepts. No single proxy tier
// needs a range wider than an IPv4 /8 or an IPv6 /16; a wider entry comes
// close to trusting every peer, which would let anyone claim
// X-Forwarded-Proto: https.
const (
	minTrustedProxyPrefixV4 = 8
	minTrustedProxyPrefixV6 = 16
)

// ParseTrustedProxies validates server.trusted_proxies (TrustedProxiesKey)
// and returns its networks. Each entry is an IP (taken as /32 or /128) or a
// CIDR. It refuses, naming the setting and the entry:
//   - an entry that is neither an IP nor a CIDR (an empty entry included),
//   - an IPv4 range wider than /8 or an IPv6 range wider than /16, so
//     0.0.0.0/0 and ::/0 are refused,
//   - an IPv4-mapped IPv6 range (::ffff:0:0/96 and its subranges), which can
//     never match a peer; the IPv4 form is required.
//
// A nil or empty list returns no networks and no error: the hub then never
// trusts a forwarded protocol.
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		n, err := parseTrustedProxy(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: entry %q %v", TrustedProxiesKey, entry, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func parseTrustedProxy(entry string) (*net.IPNet, error) {
	if strings.Contains(entry, "/") {
		ip, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("is not an IP address or CIDR range")
		}
		ones, bits := n.Mask.Size()
		switch {
		case bits == 32:
			if ones < minTrustedProxyPrefixV4 {
				return nil, fmt.Errorf("is wider than an IPv4 /%d; list the proxy's own addresses", minTrustedProxyPrefixV4)
			}
		case ip.To4() != nil:
			// An IPv4-mapped IPv6 range (::ffff:a.b.c.d/n). Go keeps a
			// 128-bit mask, and such a network never contains a peer
			// address (IPNet.Contains compares a 4-byte peer with a
			// 16-byte network), so the entry would silently match nothing.
			return nil, fmt.Errorf("is an IPv4-mapped IPv6 range, which never matches a peer; use the IPv4 form (for example %s)", ipv4MappedHint(ip, ones))
		default:
			if ones < minTrustedProxyPrefixV6 {
				return nil, fmt.Errorf("is wider than an IPv6 /%d; list the proxy's own addresses", minTrustedProxyPrefixV6)
			}
		}
		return n, nil
	}
	ip := net.ParseIP(entry)
	if ip == nil {
		return nil, fmt.Errorf("is not an IP address or CIDR range")
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}

// ipv4MappedHint is the IPv4 form of an IPv4-mapped IPv6 range, for the
// refusal message.
func ipv4MappedHint(ip net.IP, ones int) string {
	v4 := ip.To4()
	if ones < 96 {
		return v4.String() + "/0"
	}
	return (&net.IPNet{IP: v4.Mask(net.CIDRMask(ones-96, 32)), Mask: net.CIDRMask(ones-96, 32)}).String()
}
