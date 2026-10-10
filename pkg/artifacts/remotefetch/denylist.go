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

package remotefetch

import "net/netip"

// cloudMetadata is the link-local address cloud providers serve instance
// metadata and credentials on. The link-local rule already covers it; it is
// named on its own so the intent survives any change to the general rules.
var cloudMetadata = netip.MustParseAddr("169.254.169.254")

// deniedPrefixes are address ranges the fetcher never connects to, beyond
// the classes netip reports directly (loopback, private, link-local,
// multicast, unspecified). They cover special-purpose IPv4 ranges and the
// IPv6 ranges that embed or translate an IPv4 address, so an IPv6 spelling
// cannot reach a denied IPv4 destination.
var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4 special purpose (RFC 6890 and successors).
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // shared address space (carrier-grade NAT)
		"169.254.0.0/16",  // link-local, including cloud metadata
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, including broadcast
		// IPv6 special purpose. Everything outside 2000::/3 is already
		// denied by globalUnicast; these are listed too so the intent is
		// explicit, and the ones inside 2000::/3 are what the general rule
		// does not cover.
		"::/96",          // IPv4-compatible (deprecated) and unspecified
		"64:ff9b::/96",   // NAT64 well-known prefix
		"64:ff9b:1::/48", // NAT64 local use
		"100::/64",       // discard
		"2001::/32",      // Teredo
		"2001:10::/28",   // ORCHID (deprecated)
		"2001:20::/28",   // ORCHIDv2
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation
		"fec0::/10",      // site-local (deprecated)
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// globalUnicast is the only IPv6 space the fetcher may connect to; any
// other IPv6 address is denied before any other check.
var globalUnicast = netip.MustParsePrefix("2000::/3")

// ipv4Translated is the IPv4-translated (SIIT) prefix ::ffff:0:0:0/96; its
// low 32 bits are an IPv4 address.
var ipv4Translated = netip.MustParsePrefix("::ffff:0:0:0/96")

// isDenied reports whether the fetcher must refuse to connect to addr.
// IPv4-mapped (::ffff:a.b.c.d) and IPv4-translated (::ffff:0:a.b.c.d)
// addresses are judged as the IPv4 address they carry. Any other IPv6
// address outside 2000::/3 is denied; inside it, the ranges that embed or
// translate IPv4, and other non-global ranges, are denied too.
func isDenied(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	if ipv4Translated.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if addr.Is6() && !globalUnicast.Contains(addr) {
		return true
	}
	if addr == cloudMetadata {
		return true
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return true
	}
	for _, p := range deniedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
