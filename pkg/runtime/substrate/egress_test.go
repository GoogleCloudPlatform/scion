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

package substrate

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestValidateEgressAllow_AcceptsPublicHostnames(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{""},
		{"api.example.com"},
		{"*.example.com"},
		{"registry.npmjs.org", "pypi.org", "*.pypi.org"},
		{"  api.example.com  "}, // surrounding whitespace trimmed
	}
	for _, entries := range cases {
		if err := ValidateEgressAllow(entries); err != nil {
			t.Errorf("ValidateEgressAllow(%v) = %v, want nil", entries, err)
		}
	}
}

func TestValidateEgressAllow_RejectsCatchAlls(t *testing.T) {
	cases := []string{
		"all", "ALL", "All",
		"*",
		"0.0.0.0/0",
		"::/0",
		"**",
		"*.*",
	}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want a catch-all rejection", entry)
			continue
		}
		if !strings.Contains(err.Error(), entry) {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to name the offending entry", entry, err)
		}
	}
}

// TestValidateEgressAllow_RejectsAllIPAndCIDR confirms ValidateEgressAllow
// rejects every IP address and CIDR, canonical or not, public or private,
// with no exceptions — Substrate's own HostnameRule.patterns (which is where
// every egress_allow entry that passes validation ends up) explicitly
// rejects IP addresses, so an entry that validated as an "allowed IP" could
// never actually be sent in the first place. CIDRRule support is future
// work.
func TestValidateEgressAllow_RejectsAllIPAndCIDR(t *testing.T) {
	cases := []string{
		// Accepted by an earlier, overlap-based version of this validator
		// that only rejected a CIDR/IP overlapping a private/in-cluster
		// range — this validator rejects them regardless, since there is no
		// "allowed IP" category at all.
		"35.190.0.0/16",
		"8.8.8.8",
		"2001:db8::/32",
		"2001:4860:4860::8888",
		"1.1.1.0/24",
		// Rejected under that earlier, overlap-based version for
		// overlapping a private/in-cluster range — rejected here too, for
		// the blanket reason.
		"10.0.0.0/8",
		"10.1.2.0/24",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"192.168.1.1",
		"100.64.0.0/10",
		"169.254.0.0/16",
		"127.0.0.1",
		"127.0.0.0/8",
		"fc00::/7",
		"fe80::/10",
		"::1",
		"::1/128",
		"8.0.0.0/6",
		"0.0.0.0/8",
		"::",
		"240.0.0.0/4",
		"2002::/16",
		"64:ff9b::/96",
		// Canonical per net/netip but still an IP address, including
		// IPv4-embedding IPv6 transition ranges.
		"::127.0.0.1",
		"::7f00:1",
		"64:ff9b:1::1",
		"64:ff9b:1::/48",
		"2001::1",
		"2001::/32",
		// IPv4-mapped IPv6, superset/subset forms — still IP-shaped and
		// still rejected, independent of any overlap between them.
		"10.0.0.0/7",
		"1.0.0.0/1",
		"::ffff:10.0.0.1",
		"::ffff:10.0.0.0/104",
		// inet_aton short, hex, octal and single-integer spellings that
		// net/netip does not parse but resolvers and URL parsers read as
		// IPv4. (A hostname whose LAST label is a number — "foo.123" — is
		// refused too, by the public-suffix rule; see KnownBypasses.)
		"127.1",
		"10.1",
		"0x7f000001",
		"0X7F000001",
		"0x7f.0.0.1",
		"0177.0.0.1",
		"017700000001",
		"2130706433",
		"127.1.",
	}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want an IP/CIDR rejection", entry)
			continue
		}
		if !strings.Contains(err.Error(), "IP/CIDR egress rules not currently supported") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to contain the exact IP/CIDR message", entry, err)
		}
	}
}

// TestNormalizeEgressAllowEntry_ZonedIPv6Refused covers the zoned IPv6
// forms on the tenant path (NormalizeEgressAllowEntry is what
// addTenantHost in pkg/runtime calls): bracketed with a percent-encoded
// zone, and bare with a raw zone. Both are refused, by the
// unsupported-character check ahead of the IP test.
func TestNormalizeEgressAllowEntry_ZonedIPv6Refused(t *testing.T) {
	for _, entry := range []string{"[fe80::1%25eth0]", "fe80::1%eth0"} {
		got, err := NormalizeEgressAllowEntry(entry)
		if err == nil {
			t.Errorf("NormalizeEgressAllowEntry(%q) = %q, nil; want a refusal", entry, got)
			continue
		}
		if got != "" {
			t.Errorf("NormalizeEgressAllowEntry(%q) returned %q alongside its error, want \"\"", entry, got)
		}
	}
}

// TestIsIPLiteralHost pins the IP-literal predicate the trusted hub host
// check in pkg/runtime uses. The false rows are the positive control —
// hostnames with digits in a non-final label, or hex-alphabet labels
// without a "0x" prefix, are hostnames, because only the last label is
// examined.
func TestIsIPLiteralHost(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1", "::1", "fe80::1%eth0",
		"127.1", "10.1", "0x7f000001", "0X7F000001", "0x7f.0.0.1",
		"0177.0.0.1", "017700000001", "2130706433", "127.1.",
		"foo.123", "foo.0x7f", "foo.0x",
	} {
		if !IsIPLiteralHost(host) {
			t.Errorf("IsIPLiteralHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{
		"", ".", "example.com", "example.com.", "api2.example.com",
		"1.example.com", "10.0.0.1.example.com", "cafe.de", "dead.beef",
		"hub.scion-system.svc.cluster.local", "0x7f.example.com",
	} {
		if IsIPLiteralHost(host) {
			t.Errorf("IsIPLiteralHost(%q) = true, want false", host)
		}
	}
}

// TestValidateEgressAllow_IPRejectionNamesTheExactMessage double-checks that
// these three entries, which are public IPs (see
// TestValidateEgressAllow_NoFalsePositives, which does not include them),
// are rejected with this exact message.
func TestValidateEgressAllow_IPRejectionNamesTheExactMessage(t *testing.T) {
	const wantMsg = "IP/CIDR egress rules not currently supported"
	for _, entry := range []string{"8.8.8.8", "2001:4860:4860::8888", "1.1.1.0/24"} {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection", entry)
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to contain %q", entry, err, wantMsg)
		}
	}
}

func TestValidateEgressAllow_RejectsInClusterHostSuffixes(t *testing.T) {
	cases := []string{
		"atenet-router.ate-system.svc",
		"api.ate-system.svc.cluster.local",
		"foo.cluster.local",
		"metadata.internal",
		"ATENET-ROUTER.ATE-SYSTEM.SVC", // case-insensitive
	}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want an in-cluster-suffix rejection", entry)
			continue
		}
		if !strings.Contains(err.Error(), entry) {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to name the offending entry", entry, err)
		}
	}
}

func TestValidateEgressAllow_NamesTheOffendingEntryAmongValidOnes(t *testing.T) {
	entries := []string{"api.example.com", "registry.npmjs.org", "foo.pod", "pypi.org"}
	err := ValidateEgressAllow(entries)
	if err == nil {
		t.Fatal("ValidateEgressAllow() = nil, want an error for the embedded non-public-suffix entry")
	}
	if !strings.Contains(err.Error(), "foo.pod") {
		t.Errorf("error = %v, want it to name foo.pod specifically, not the whole list", err)
	}
}

func TestV1SubstrateConfig_Validate(t *testing.T) {
	var nilCfg *config.V1SubstrateConfig
	if err := Validate(nilCfg); err != nil {
		t.Errorf("Validate((*V1SubstrateConfig)(nil)) = %v, want nil", err)
	}

	ok := &config.V1SubstrateConfig{EgressAllow: []string{"api.example.com"}}
	if err := Validate(ok); err != nil {
		t.Errorf("Validate() with a clean egress_allow = %v, want nil", err)
	}

	bad := &config.V1SubstrateConfig{EgressAllow: []string{"all"}}
	err := Validate(bad)
	if err == nil {
		t.Fatal("Validate() with egress_allow: [all] = nil, want an error")
	}
	if !strings.Contains(err.Error(), "egress_allow") {
		t.Errorf("error = %v, want it to name the egress_allow field", err)
	}
}

// TestValidateEgressTrustBundle covers ValidateEgressTrustBundle's three
// required cases directly: empty OK, the one supported name OK, anything
// else rejected naming the supported value.
func TestValidateEgressTrustBundle(t *testing.T) {
	if err := ValidateEgressTrustBundle(""); err != nil {
		t.Errorf("ValidateEgressTrustBundle(\"\") = %v, want nil (off)", err)
	}
	if err := ValidateEgressTrustBundle("egress-mitm.ate.dev"); err != nil {
		t.Errorf("ValidateEgressTrustBundle(%q) = %v, want nil", "egress-mitm.ate.dev", err)
	}

	err := ValidateEgressTrustBundle("my-own-bundle")
	if err == nil {
		t.Fatal("ValidateEgressTrustBundle(\"my-own-bundle\") = nil, want an error")
	}
	if !strings.Contains(err.Error(), "my-own-bundle") {
		t.Errorf("error = %v, want it to name the offending value", err)
	}
	if !strings.Contains(err.Error(), "egress-mitm.ate.dev") {
		t.Errorf("error = %v, want it to name the supported value", err)
	}
}

// TestV1SubstrateConfig_Validate_EgressTrustBundle exercises the field
// through Validate (not just the standalone validator function), the same
// way TestV1SubstrateConfig_Validate does for egress_allow above.
func TestV1SubstrateConfig_Validate_EgressTrustBundle(t *testing.T) {
	empty := &config.V1SubstrateConfig{}
	if err := Validate(empty); err != nil {
		t.Errorf("Validate() with EgressTrustBundle unset = %v, want nil", err)
	}

	ok := &config.V1SubstrateConfig{EgressTrustBundle: "egress-mitm.ate.dev"}
	if err := Validate(ok); err != nil {
		t.Errorf("Validate() with EgressTrustBundle = %q, want nil, got %v", ok.EgressTrustBundle, err)
	}

	bad := &config.V1SubstrateConfig{EgressTrustBundle: "not-the-real-bundle"}
	err := Validate(bad)
	if err == nil {
		t.Fatal("Validate() with an unsupported EgressTrustBundle = nil, want an error")
	}
	if !strings.Contains(err.Error(), "egress_trust_bundle") {
		t.Errorf("error = %v, want it to name the egress_trust_bundle field", err)
	}
	if !strings.Contains(err.Error(), "egress-mitm.ate.dev") {
		t.Errorf("error = %v, want it to name the one supported value", err)
	}
}

// TestValidateEgressAllow_RejectsBypasses covers a set of bypasses a naive
// validator could miss: each of these must be rejected, never nil, from
// ValidateEgressAllow.
func TestValidateEgressAllow_RejectsBypasses(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		// Fix step 1: trailing-dot FQDNs defeat suffix matching.
		{"trailing dot on .svc.cluster.local", "atenet-router.ate-system.svc.cluster.local."},
		{"trailing dot, uppercase .SVC", "foo.SVC."},

		// Fix step 5: Kubernetes short names via the cluster DNS search path.
		{"router short name", "atenet-router.ate-system"},
		{"api short name", "api.ate-system"},
		{"kubernetes default short name", "kubernetes.default"},

		// Fix step 4: single-label hostnames.
		{"GCE metadata alias", "metadata"},
		{"bare cluster domain", "cluster.local"}, // also caught by the .local suffix

		// Fix step 6: unspecified addresses.
		{"0.0.0.0/8 range", "0.0.0.0/8"},
		{"IPv6 unspecified", "::"},

		// Fix step 2/3: non-canonical IP spellings.
		{"hex IP alias", "0x7f000001"},
		{"decimal IP alias", "2130706433"},
		{"partial dotted-quad", "127.1"},
		{"bracketed IPv6", "[::1]"},
		{"IPv6 with zone id", "fe80::1%eth0"},
		{"host:port", "10.0.0.1:443"},

		// Reserved / IPv4-in-IPv6 transition ranges.
		{"Class E reserved", "240.0.0.0/4"},
		{"6to4", "2002::/16"},
		{"NAT64", "64:ff9b::/96"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateEgressAllow([]string{tc.entry}); err == nil {
				t.Errorf("ValidateEgressAllow([%q]) = nil, want a rejection (%s)", tc.entry, tc.name)
			}
		})
	}
}

// TestValidateEgressAllow_TrailingDotNormalization confirms normalization
// doesn't over-reject: a trailing dot on an otherwise-fine public hostname
// is stripped, not treated as an error, and case is folded too.
func TestValidateEgressAllow_TrailingDotNormalization(t *testing.T) {
	cases := []string{
		"api.example.com.",
		"API.EXAMPLE.COM",
		"  api.example.com  ",
	}
	for _, entry := range cases {
		if err := ValidateEgressAllow([]string{entry}); err != nil {
			t.Errorf("ValidateEgressAllow([%q]) = %v, want nil", entry, err)
		}
	}
}

// TestValidateEgressAllow_PunycodeTLD confirms a legitimate IDNA punycode
// ccTLD (xn--p1ai is Russia's Cyrillic ".рф") passes the public-suffix
// check like any other real, ICANN-delegated TLD.
func TestValidateEgressAllow_PunycodeTLD(t *testing.T) {
	if err := ValidateEgressAllow([]string{"example.xn--p1ai"}); err != nil {
		t.Errorf("ValidateEgressAllow([example.xn--p1ai]) = %v, want nil (punycode TLD)", err)
	}
}

// TestValidateEgressAllow_RejectsWildcardOverPublicSuffix is the explicit
// wildcard rule: a leading "*." is allowed, but the remainder must
// independently pass the same public-suffix rule. "*.com", "*.co", "*.bd",
// and "*.co.uk" are rejected because their remainder is itself a public
// suffix with nothing beneath it; "*.example.com" passes because
// "example.com" has "example" beneath the "com" suffix.
//
// The single-label cases ("*.com", "*.co", "*.bd") also lock in the correct
// rejection reason: a naive implementation could intercept these earlier
// with the single-label "not fully qualified" check instead of ever
// reaching the public-suffix check, which would name the wrong reason.
// Asserting the "itself a public suffix" reason here (like the sibling
// TestValidateEgressAllow_PrivateSuffixPlatformRejectionReason already does
// for the bare, non-wildcard form) is what would catch that drift.
func TestValidateEgressAllow_RejectsWildcardOverPublicSuffix(t *testing.T) {
	for _, entry := range []string{"*.com", "*.co", "*.bd", "*.co.uk"} {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want a rejection (wildcard over a bare public suffix)", entry)
			continue
		}
		if !strings.Contains(err.Error(), "itself a public suffix") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to reject because the remainder IS its own suffix", entry, err)
		}
	}
	if err := ValidateEgressAllow([]string{"*.example.com"}); err != nil {
		t.Errorf("ValidateEgressAllow([*.example.com]) = %v, want nil", err)
	}
}

// TestValidateEgressAllow_RejectsWildcardOverWildcardSuffix: a wildcard
// entry is rejected not only when its remainder is itself a bare public
// suffix (the check above), but also when the remainder isn't itself a
// public suffix yet every single-label child of it IS one, via a PSL
// *wildcard* rule — "run.app", "compute.amazonaws.com",
// "compute-1.amazonaws.com", and "kawasaki.jp" are all real PSL wildcard
// rules, so "foo.run.app" (say) is a specific, single-tenant host, but
// "*.run.app" would grant every tenant on the platform, the same class of
// over-grant a bare "*.googleapis.com" would be if "googleapis.com" weren't
// already caught as a bare suffix. "*.github.io" and "*.googleapis.com"
// stay rejected too (via the bare-suffix check, not this one).
func TestValidateEgressAllow_RejectsWildcardOverWildcardSuffix(t *testing.T) {
	cases := []string{
		"*.run.app",
		"*.compute.amazonaws.com",
		"*.compute-1.amazonaws.com",
		"*.kawasaki.jp",
		"*.github.io",
		"*.googleapis.com",
	}
	for _, entry := range cases {
		if err := ValidateEgressAllow([]string{entry}); err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want a rejection (wildcard over a PSL wildcard rule)", entry)
		}
	}
}

// TestValidateEgressAllow_RejectsWildcardOverWildcardSuffixReason
// spot-checks the rejection reason for the entries that are rejected
// specifically because of the wildcard-over-wildcard-PSL-rule check (not
// because the remainder is itself a bare public suffix — that's a
// different message, covered by TestValidateEgressAllow_RejectsWildcardOverPublicSuffix).
func TestValidateEgressAllow_RejectsWildcardOverWildcardSuffixReason(t *testing.T) {
	cases := []string{"*.run.app", "*.compute.amazonaws.com", "*.compute-1.amazonaws.com", "*.kawasaki.jp"}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection", entry)
		}
		if !strings.Contains(err.Error(), "wildcard public-suffix rule") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to name the wildcard-PSL-rule reason", entry, err)
		}
	}
}

// TestValidateEgressAllow_AcceptsWildcardOnlyCcTLDs: several real ccTLDs
// ("ck", "er", "fk", "jm", "kh", "mm", "np", "pg") have only a wildcard
// rule in the PSL, no bare-TLD rule, so a naive
// publicsuffix.PublicSuffix(tld) check wrongly reports them as unmanaged.
// "www.ck" is a PSL *exception* to the "*.ck" wildcard rule (a normal,
// directly-registrable host, not itself a further public suffix);
// "foo.com.np" and "example.com.jm" are ordinary hosts one label under
// their platform's own wildcard-matched suffix ("com.np", "com.jm").
func TestValidateEgressAllow_AcceptsWildcardOnlyCcTLDs(t *testing.T) {
	cases := []string{"www.ck", "foo.com.np", "example.com.jm"}
	for _, entry := range cases {
		if err := ValidateEgressAllow([]string{entry}); err != nil {
			t.Errorf("ValidateEgressAllow([%q]) = %v, want nil (wildcard-only ccTLD)", entry, err)
		}
	}
}

// TestValidateEgressAllow_RejectsOnion: ".onion" (RFC 7686, Tor
// hidden-service addresses) is rejected the same way ".arpa" is, via
// egressAllowSpecialUseTLDs — "onion" is itself listed in the PSL's ICANN
// section, so the generic ICANN-suffix check alone would not catch it.
func TestValidateEgressAllow_RejectsOnion(t *testing.T) {
	for _, entry := range []string{"foo.onion", "*.onion"} {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want a rejection (special-use .onion)", entry)
			continue
		}
		// "onion" alone would trivially match: it's already in the quoted
		// entry itself ("foo.onion", "*.onion"), so it wouldn't actually
		// confirm the special-use check fired rather than some unrelated
		// rejection. Check for the reason text instead.
		if !strings.Contains(err.Error(), "Tor hidden services") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to name the onion special case", entry, err)
		}
	}
}

// buildHostnameOfLength returns a syntactically valid hostname (LDH labels,
// each at most 63 characters, ending in ".com") whose ASCII presentation
// form is exactly total characters long — used to probe
// egressAllowMaxLength's boundary precisely, rather than approximately.
func buildHostnameOfLength(total int) string {
	const tld = "com"
	remaining := total - len(tld) - 1 // -1 for the dot immediately before "com"
	var parts []string
	for remaining > 0 {
		n := remaining
		if n > 63 {
			n = 63
		}
		parts = append(parts, strings.Repeat("a", n))
		remaining -= n
		if remaining > 0 {
			remaining-- // the dot that will separate this label from the next
		}
	}
	parts = append(parts, tld)
	return strings.Join(parts, ".")
}

// TestValidateEgressAllow_LengthLimit: the total length of the exact string
// that would be sent (including a wildcard prefix, if any) is capped at
// egressAllowMaxLength (253) characters, the standard DNS presentation-form
// limit. idna.Lookup does not enforce this on its own (it doesn't set
// VerifyDNSLength), so nothing else in this validator does either — an
// over-length entry would otherwise pass local validation and only fail
// later, inside Substrate's own CreateActorEgressPolicy call.
func TestValidateEgressAllow_LengthLimit(t *testing.T) {
	at := buildHostnameOfLength(253)
	if got := len(at); got != 253 {
		t.Fatalf("buildHostnameOfLength(253) has length %d, want 253 (test bug)", got)
	}
	if err := ValidateEgressAllow([]string{at}); err != nil {
		t.Errorf("ValidateEgressAllow([<253-char hostname>]) = %v, want nil (at the limit)", err)
	}

	over := buildHostnameOfLength(256)
	if got := len(over); got != 256 {
		t.Fatalf("buildHostnameOfLength(256) has length %d, want 256 (test bug)", got)
	}
	err := ValidateEgressAllow([]string{over})
	if err == nil {
		t.Fatal("ValidateEgressAllow([<256-char hostname>]) = nil, want a rejection (over the limit)")
	}
	if !strings.Contains(err.Error(), "253-character limit") {
		t.Errorf("error = %v, want it to name the length limit", err)
	}

	// A wildcard prefix counts toward the limit too: a 252-char hostname is
	// fine bare (253 total isn't reached), but "*." pushes a 252-char
	// remainder's total to 254 — over the limit — since the length check
	// is against the string actually sent, wildcard prefix included.
	remainder := buildHostnameOfLength(252)
	wildcardEntry := "*." + remainder
	if got := len(wildcardEntry); got != 254 {
		t.Fatalf("len(%q) = %d, want 254 (test bug)", wildcardEntry, got)
	}
	if err := ValidateEgressAllow([]string{wildcardEntry}); err == nil {
		t.Error("ValidateEgressAllow([<252-char remainder with *. prefix>]) = nil, want a rejection (254 chars sent, over the limit)")
	}
}

// TestValidateEgressAllow_KnownBypasses is the consolidated regression
// suite: every bypass this validator has ever needed to close, in one
// table, so the full history stays locked in against whatever the
// validator becomes next rather than being scattered across many small test
// functions.
//
// Each row also asserts wantReason, a substring of the actual rejection
// message: checking only err != nil would not catch a row silently
// drifting onto a different rejection rule than the one it's meant to
// exercise — a wildcard's single-label remainder, for instance, could be
// rejected correctly but by the wrong rule, with a misleading message.
// Every wantReason below is the reason the code actually produces today,
// confirmed against a real run of the validator, not assumed from the
// row's own label.
func TestValidateEgressAllow_KnownBypasses(t *testing.T) {
	const (
		reasonCatchAll        = "catch-all"
		reasonIPCIDR          = "IP/CIDR"
		reasonUnsupportedChar = "unsupported character"
		reasonNotICANN        = "ICANN-managed public suffix"
		reasonSingleLabel     = "single-label hostname"
		reasonInvalidLabel    = "invalid DNS label"
		reasonNotValidHost    = "valid hostname"
		// reasonArpa and reasonOnion are message-specific, not just the bare
		// TLD name: "arpa"/"onion" alone would trivially match, since
		// they're already part of the quoted entry itself (e.g.
		// "home.arpa", "foo.onion"), which wouldn't actually confirm the
		// special-use check fired rather than some unrelated rejection.
		reasonArpa           = "reserved for special-use"
		reasonItselfSuffix   = "itself a public suffix"
		reasonWildcardSuffix = "wildcard public-suffix rule"
		reasonOnion          = "Tor hidden services"
		reasonTooLong        = "253-character limit"
	)
	cases := []struct {
		name       string
		entry      string
		wantReason string
	}{
		// --- Catch-alls and in-cluster suffixes ---
		{"catch-all: all", "all", reasonCatchAll},
		{"catch-all: bare wildcard *", "*", reasonCatchAll},
		{"catch-all: 0.0.0.0/0", "0.0.0.0/0", reasonCatchAll},
		{"catch-all: ::/0", "::/0", reasonCatchAll},
		{"IP/CIDR: 10.0.0.0/8", "10.0.0.0/8", reasonIPCIDR},
		{"IP/CIDR: bare IP in 192.168/16", "192.168.1.1", reasonIPCIDR},
		{"in-cluster suffix: .svc", "atenet-router.ate-system.svc", reasonNotICANN},
		{"in-cluster suffix: .cluster.local", "api.ate-system.svc.cluster.local", reasonNotICANN},
		{"in-cluster suffix: .internal", "metadata.internal", reasonNotICANN},

		// --- Trailing dots, Kubernetes short names via the cluster DNS
		// search path, non-canonical IP spellings ---
		{"trailing dot on .svc.cluster.local", "atenet-router.ate-system.svc.cluster.local.", reasonNotICANN},
		{"trailing dot, uppercase .SVC", "foo.SVC.", reasonNotICANN},
		{"Kubernetes short name: router", "atenet-router.ate-system", reasonNotICANN},
		{"Kubernetes short name: api", "api.ate-system", reasonNotICANN},
		{"Kubernetes short name: kubernetes.default", "kubernetes.default", reasonNotICANN},
		{"single-label GCE metadata alias", "metadata", reasonSingleLabel},
		{"bare cluster.local domain", "cluster.local", reasonNotICANN},
		{"IP/CIDR: 0.0.0.0/8", "0.0.0.0/8", reasonIPCIDR},
		{"IP/CIDR: IPv6 unspecified ::", "::", reasonIPCIDR},
		{"IP/CIDR: hex IP alias", "0x7f000001", reasonIPCIDR},
		{"IP/CIDR: decimal IP alias", "2130706433", reasonIPCIDR},
		{"IP/CIDR: partial dotted-quad", "127.1", reasonIPCIDR},
		{"unsupported character: bracketed IPv6", "[::1]", reasonUnsupportedChar},
		{"unsupported character: IPv6 zone id", "fe80::1%eth0", reasonUnsupportedChar},
		{"invalid hostname: host:port", "10.0.0.1:443", reasonNotValidHost},
		{"IP/CIDR: Class E reserved", "240.0.0.0/4", reasonIPCIDR},
		{"IP/CIDR: 6to4", "2002::/16", reasonIPCIDR},
		{"IP/CIDR: NAT64 (well-known prefix)", "64:ff9b::/96", reasonIPCIDR},

		// --- Double trailing dots, mixed-radix IP spellings, IDN
		// look-alikes, .localhost/localdomain ---
		{"double trailing dot: .svc", "foo.svc..", reasonInvalidLabel},
		{"double trailing dot: full router FQDN", "atenet-router.ate-system.svc.cluster.local..", reasonInvalidLabel},
		{"double trailing dot: kubernetes.default", "kubernetes.default..", reasonInvalidLabel},
		{"IP/CIDR: mixed hex/decimal loopback a", "0x7f.0.0.1", reasonIPCIDR},
		{"IP/CIDR: mixed hex/decimal loopback b", "0x7f.1", reasonIPCIDR},
		{"IP/CIDR: mixed hex/decimal loopback c", "127.0.0.0x1", reasonIPCIDR},
		{"IP/CIDR: mixed hex/decimal private", "10.0x0.0.1", reasonIPCIDR},
		{"IP/CIDR: all-hex-per-octet unspecified", "0x0.0x0.0x0.0x0", reasonIPCIDR},
		{"IDN look-alike: ideographic full stop", "kubernetes.default。svc", reasonNotICANN},
		{"IDN look-alike: fullwidth letters", "foo.ＳＶＣ", reasonNotICANN},
		{"in-cluster/local suffix: .localhost", "foo.localhost", reasonNotICANN},
		{"exact name: localhost.localdomain", "localhost.localdomain", reasonNotICANN},
		{"IP/CIDR: too many dotted parts, all numeric", "1.2.3.4.5", reasonIPCIDR},
		{"numeric last label (not a real TLD)", "foo.123", reasonNotICANN},
		{"hex-shaped last label (not a real TLD)", "foo.0x7f", reasonNotICANN},
		{"invalid hostname: space in label", "git hub.com", reasonNotValidHost},

		// --- Kubernetes pod-IP DNS names and other TLD-shaped-but-not-public
		// zones; IP/CIDR entirely ---
		{"pod DNS name encoding the GCE metadata IP", "169-254-169-254.default.pod", reasonNotICANN},
		{"pod DNS name encoding loopback", "127-0-0-1.default.pod", reasonNotICANN},
		{"pod DNS name in a real namespace", "10-0-0-1.kube-system.pod", reasonNotICANN},
		{"wildcard pod DNS name (every IPv4 via default ns)", "*.default.pod", reasonNotICANN},
		{"RFC 8375 home.arpa local zone", "foo.home.arpa", reasonArpa},
		{".lan local zone (not a real TLD)", "foo.lan", reasonNotICANN},
		{".corp made-up zone (not a real TLD)", "foo.corp", reasonNotICANN},
		// Rejected by the TLD check, not the suffix blocklist: the
		// ICANN-public-suffix check (rule 1) always fires first for these,
		// so the blocklist below it is never actually reached.
		{".local zone (rejected by the TLD check, not the suffix blocklist)", "foo.local", reasonNotICANN},
		{".internal zone (rejected by the TLD check, not the suffix blocklist)", "foo.internal", reasonNotICANN},
		{"typo TLD: .kom (not a real TLD)", "example.kom", reasonNotICANN},
		{"IP/CIDR: bare public IP", "8.8.8.8", reasonIPCIDR},
		{"IP/CIDR: bare public IPv6", "2001:4860:4860::8888", reasonIPCIDR},
		{"IP/CIDR: public CIDR", "1.1.1.0/24", reasonIPCIDR},

		// --- The whole .arpa TLD, and a bare/wildcarded PRIVATE-suffix
		// platform domain with nothing beneath it ---
		{"special-use TLD: bare home.arpa", "home.arpa", reasonArpa},
		{"special-use TLD: foo.home.arpa", "foo.home.arpa", reasonArpa},
		{"special-use TLD: reverse DNS in-addr.arpa", "1.0.0.10.in-addr.arpa", reasonArpa},
		{"PRIVATE-suffix platform domain, bare: googleapis.com", "googleapis.com", reasonItselfSuffix},
		{"PRIVATE-suffix platform domain, wildcard: *.googleapis.com", "*.googleapis.com", reasonItselfSuffix},
		{"PRIVATE-suffix platform domain, wildcard: *.github.io", "*.github.io", reasonItselfSuffix},

		// --- Wildcard over a PSL wildcard rule; special-use .onion; length
		// cap. (The wildcard-only-ccTLD false-positive fix is an accept,
		// not a reject, so it's covered separately in
		// TestValidateEgressAllow_AcceptsWildcardOnlyCcTLDs; the
		// single-label wildcard message fix is covered in
		// TestValidateEgressAllow_RejectsWildcardOverPublicSuffix.) ---
		{"wildcard over a PSL wildcard rule: *.run.app", "*.run.app", reasonWildcardSuffix},
		{"wildcard over a PSL wildcard rule: *.compute.amazonaws.com", "*.compute.amazonaws.com", reasonWildcardSuffix},
		{"wildcard over a PSL wildcard rule: *.compute-1.amazonaws.com", "*.compute-1.amazonaws.com", reasonWildcardSuffix},
		{"wildcard over a PSL wildcard rule: *.kawasaki.jp", "*.kawasaki.jp", reasonWildcardSuffix},
		{"special-use TLD: onion hidden service", "foo.onion", reasonOnion},
		{"special-use TLD: wildcard over onion", "*.onion", reasonOnion},
		{"256-character hostname, over the DNS length limit", buildHostnameOfLength(256), reasonTooLong},

		// --- A single-label wildcard over a bare in-cluster/local label is
		// rejected structurally (today, via rule 1 — none of these is any
		// kind of PSL TLD at all), not merely because the suffix blocklist
		// happens to also list it. The blocklist's
		// HasSuffix(ascii, ".svc") check would not, on its own, match a
		// bare "svc" (no leading dot's worth of a suffix to match
		// against); rule 2 (ascii == its own matched suffix) would still
		// catch it even if rule 1 didn't, since a single label is always
		// its own public suffix — see egressAllowSuffixOK's doc. ---
		{"wildcard over a bare in-cluster label: *.svc", "*.svc", reasonNotICANN},
		{"wildcard over a bare in-cluster label: *.local", "*.local", reasonNotICANN},
		{"wildcard over a bare in-cluster label: *.localhost", "*.localhost", reasonNotICANN},
		{"wildcard over a bare in-cluster label: *.internal", "*.internal", reasonNotICANN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEgressAllow([]string{tc.entry})
			if err == nil {
				t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection (%s)", tc.entry, tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to contain %q", tc.entry, err, tc.wantReason)
			}
		})
	}
}

// TestValidateEgressAllow_TLDNotICANNRejectionReason spot-checks the
// rejection REASON (not just that an error occurred) for a sample of the
// Kubernetes pod-DNS-name and made-up-zone rows. These must be rejected
// specifically because their top-level domain fails the ICANN-public-suffix
// check, not for some unrelated reason (e.g. the hyphens in the
// pod-IP-encoding labels must not trip the LDH grammar check instead —
// hyphens are valid mid-label).
func TestValidateEgressAllow_TLDNotICANNRejectionReason(t *testing.T) {
	cases := []string{
		"169-254-169-254.default.pod",
		"10-0-0-1.kube-system.pod",
		"foo.lan",
		"foo.corp",
		"example.kom",
	}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection", entry)
		}
		if !strings.Contains(err.Error(), "ICANN-managed public suffix") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to reject for failing the public-suffix check specifically", entry, err)
		}
	}
}

// TestValidateEgressAllow_ArpaRejectionReason spot-checks the rejection
// reason for the ".arpa" special case, which is rejected before the
// generic ICANN-suffix check ever runs (see egressAllowSuffixOK's doc
// comment: "arpa" IS itself ICANN-listed, so the generic check alone would
// not catch it).
func TestValidateEgressAllow_ArpaRejectionReason(t *testing.T) {
	cases := []string{"home.arpa", "foo.home.arpa", "1.0.0.10.in-addr.arpa"}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection", entry)
		}
		// "arpa" alone would trivially match: it's already in the quoted
		// entry itself (every case here ends in ".arpa"), so it wouldn't
		// actually confirm the special-use check fired rather than some
		// unrelated rejection. Check for the reason text instead.
		if !strings.Contains(err.Error(), "reserved for special-use") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to name the arpa special case", entry, err)
		}
	}
}

// TestValidateEgressAllow_PrivateSuffixPlatformRejectionReason spot-checks
// the rejection reason for a bare or wildcarded PRIVATE-suffix platform
// domain (googleapis.com, github.io): rejected because the domain IS its
// own matched suffix, with nothing beneath it — not because the suffix
// isn't ICANN-managed (it doesn't need to be).
func TestValidateEgressAllow_PrivateSuffixPlatformRejectionReason(t *testing.T) {
	cases := []string{"googleapis.com", "*.googleapis.com", "*.github.io"}
	for _, entry := range cases {
		err := ValidateEgressAllow([]string{entry})
		if err == nil {
			t.Fatalf("ValidateEgressAllow([%q]) = nil, want a rejection", entry)
		}
		if !strings.Contains(err.Error(), "itself a public suffix") {
			t.Errorf("ValidateEgressAllow([%q]) error = %v, want it to reject because the entry IS its own suffix", entry, err)
		}
	}
}

// TestValidateEgressAllow_NoFalsePositives is the explicit "no false
// positives" list: real, unrelated public hostnames that must not be
// mistaken for anything this validator rejects — hex-alphabet domains,
// PRIVATE-PSL-suffix platform hostnames with a label beneath the platform's
// own suffix, wildcard-only ccTLDs, and the length boundary. The three
// IP/CIDR entries (8.8.8.8, 2001:4860:4860::8888, 1.1.1.0/24) are
// deliberately absent from this list: this validator rejects bare IPs and
// CIDRs outright — see TestValidateEgressAllow_IPRejectionNamesTheExactMessage,
// which asserts the rejection.
func TestValidateEgressAllow_NoFalsePositives(t *testing.T) {
	cases := []string{
		"api.anthropic.com",
		"github.com",
		"registry.npmjs.org",
		"my-host.example.com",
		"*.github.com",
		"GitHub.COM.", // uppercase + trailing dot
		"xn--80ak6aa92e.com",
		"foo.xn--p1ai", // IDNA punycode TLD
		// Real hostnames on PRIVATE-PSL-suffix multi-tenant platforms, with
		// a label beneath the platform's own suffix.
		"storage.googleapis.com",
		"foo.github.io",
		// Real, unrelated domains whose labels happen to spell
		// hex-alphabet words. Must not be mistaken for an IP address.
		"cafe.de",
		"dead.beef.com",
		"abc.de",
		"fab.be",
		"adcb.ae",
		// Real ccTLDs whose only PSL rule is a wildcard, not a bare-TLD
		// rule.
		"www.ck",
		"foo.com.np",
		"example.com.jm",
		// Right at the length limit, not over it.
		buildHostnameOfLength(253),
	}
	for _, entry := range cases {
		if err := ValidateEgressAllow([]string{entry}); err != nil {
			t.Errorf("ValidateEgressAllow([%q]) = %v, want nil (no false positive)", entry, err)
		}
	}
}

// TestValidateEgressAllow_LocalhostForms dedicates coverage to every
// "localhost"-shaped rejection, beyond what the combined bypass table
// already exercises.
func TestValidateEgressAllow_LocalhostForms(t *testing.T) {
	cases := []string{
		"localhost",
		"LOCALHOST",
		"localhost.",
		"foo.localhost",
		"FOO.LOCALHOST",
		"localhost.localdomain",
		"LOCALHOST.LOCALDOMAIN",
	}
	for _, entry := range cases {
		if err := ValidateEgressAllow([]string{entry}); err == nil {
			t.Errorf("ValidateEgressAllow([%q]) = nil, want a rejection (localhost form)", entry)
		}
	}
}

// TestEgressAllowCovers exercises the exact-match and single-level wildcard
// coverage rule directly, independent of substrateEgressHostnames' own
// caller.
func TestEgressAllowCovers(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		host    string
		want    bool
	}{
		{"exact match", []string{"example.com"}, "example.com", true},
		{"no match", []string{"example.com"}, "other.com", false},
		{"wildcard covers one label under", []string{"*.example.com"}, "api.example.com", true},
		{"wildcard does not cover the bare suffix itself", []string{"*.example.com"}, "example.com", false},
		{"wildcard does not cover two labels under", []string{"*.example.com"}, "a.b.example.com", false},
		{"wildcard does not cover an unrelated suffix", []string{"*.example.com"}, "api.example.org", false},
		// Label-boundary regression cases: egressAllowEntryCovers checks
		// strings.CutSuffix(host, "."+remainder), not a bare
		// strings.HasSuffix(host, remainder) — the leading dot is what
		// makes "evilexample.com" (which ends in the literal characters
		// "example.com" but has no label boundary before them) distinct
		// from "api.example.com". Without the dot, a mutation that drops
		// it would let a wildcard or exact entry cover a same-suffix
		// domain it was never meant to.
		{"wildcard does not cover a suffix look-alike with no label boundary", []string{"*.example.com"}, "evilexample.com", false},
		{"exact entry does not cover a suffix look-alike with no label boundary", []string{"hub.example.com"}, "evilhub.example.com", false},
		{"first of several entries matches", []string{"example.com", "other.com"}, "example.com", true},
		{"second of several entries matches", []string{"other.com", "example.com"}, "example.com", true},
		{"empty entry list covers nothing", nil, "example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EgressAllowCovers(tc.entries, tc.host); got != tc.want {
				t.Errorf("EgressAllowCovers(%v, %q) = %v, want %v", tc.entries, tc.host, got, tc.want)
			}
		})
	}
}
