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

package provider

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// ErrInvalidCanonicalOrigin indicates an endpoint URL failed canonical HTTPS
// origin normalization (PV3 Addendum F §F9).
var ErrInvalidCanonicalOrigin = errors.New("system_one: invalid canonical https endpoint origin")

// CanonicalOrigin parses and normalizes raw to a canonical HTTPS origin of the
// form "https://<lowercase-ascii-host>" per PV3 Addendum F §F9:
//   - scheme must be "https" (case-insensitive);
//   - host must be lowercase-normalizable ASCII DNS labels only (1..253 bytes;
//     rejects non-ASCII/Unicode, IDNA "xn--" punycode labels, IPv4/IPv6/numeric
//     IP literals, percent-escapes, leading/trailing dots, and empty labels);
//   - port must be empty or "443" (normalized to empty);
//   - path must be empty or "/" (normalized to empty);
//   - no userinfo, query parameters (including bare "?"), fragments (including
//     bare "#"), backslashes, or control/whitespace characters.
func CanonicalOrigin(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidCanonicalOrigin
	}
	for i := 0; i < len(trimmed); i++ {
		b := trimmed[i]
		if b <= 0x20 || b >= 0x7f || b == '?' || b == '#' || b == '\\' {
			return "", ErrInvalidCanonicalOrigin
		}
	}

	u, err := url.Parse(trimmed)
	if err != nil || u == nil {
		return "", ErrInvalidCanonicalOrigin
	}
	if u.Opaque != "" || !strings.EqualFold(u.Scheme, "https") {
		return "", ErrInvalidCanonicalOrigin
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrInvalidCanonicalOrigin
	}
	if (u.Path != "" && u.Path != "/") || (u.RawPath != "" && u.RawPath != "/") {
		return "", ErrInvalidCanonicalOrigin
	}
	if u.Host == "" || strings.ContainsAny(u.Host, "[]%") {
		return "", ErrInvalidCanonicalOrigin
	}

	hostname := u.Host
	if strings.Contains(u.Host, ":") {
		hostPart, portPart, splitErr := net.SplitHostPort(u.Host)
		if splitErr != nil || portPart != "443" {
			return "", ErrInvalidCanonicalOrigin
		}
		hostname = hostPart
	}

	if len(hostname) == 0 || len(hostname) > 253 {
		return "", ErrInvalidCanonicalOrigin
	}
	if strings.HasPrefix(hostname, ".") || strings.HasSuffix(hostname, ".") {
		return "", ErrInvalidCanonicalOrigin
	}

	hostLower := strings.ToLower(hostname)
	if net.ParseIP(hostLower) != nil {
		return "", ErrInvalidCanonicalOrigin
	}

	labels := strings.Split(hostLower, ".")
	for _, lbl := range labels {
		if len(lbl) == 0 || len(lbl) > 63 {
			return "", ErrInvalidCanonicalOrigin
		}
		if strings.HasPrefix(lbl, "-") || strings.HasSuffix(lbl, "-") {
			return "", ErrInvalidCanonicalOrigin
		}
		if strings.HasPrefix(lbl, "xn--") {
			return "", ErrInvalidCanonicalOrigin
		}
		for i := 0; i < len(lbl); i++ {
			b := lbl[i]
			if !((b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-') {
				return "", ErrInvalidCanonicalOrigin
			}
		}
	}
	// Reject numeric top-level labels (including decimal/octal IPv4 forms).
	if isAllDigitsASCII(labels[len(labels)-1]) {
		return "", ErrInvalidCanonicalOrigin
	}

	return "https://" + hostLower, nil
}

func isAllDigitsASCII(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// MatchesAdapterDefaultEndpoint checks that adapter implements EndpointDefaulter
// with a valid canonical HTTPS origin, and that configuredEndpoint is either
// empty (defaulted) or normalizes to the exact same canonical origin (§F9).
// Substring or prefix matches never succeed.
func MatchesAdapterDefaultEndpoint(adapter Adapter, configuredEndpoint string) (string, bool) {
	if adapter == nil {
		return "", false
	}
	defaulter, ok := adapter.(EndpointDefaulter)
	if !ok {
		return "", false
	}
	canonDefault, err := CanonicalOrigin(defaulter.DefaultEndpoint())
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(configuredEndpoint) == "" {
		return canonDefault, true
	}
	canonConfigured, err := CanonicalOrigin(configuredEndpoint)
	if err != nil || canonConfigured != canonDefault {
		return "", false
	}
	return canonDefault, true
}
