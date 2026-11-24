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

// Package firewallpolicy validates, normalizes, and encodes the operator-sourced
// egress firewall policy passed from the Runtime Broker to sciontool netns-setup.
//
// It intentionally depends only on the Go standard library so both the broker
// and sciontool can import it as a leaf package.
package firewallpolicy

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

const (
	// EnvAllowDomains carries the comma-separated canonical allowlist of
	// extra hostnames passed to sciontool netns-setup and init-firewall.sh.
	EnvAllowDomains = "SCION_INIT_FIREWALL_ALLOW_DOMAINS"
	// EnvAllowSSH carries "true" when outbound tcp/22 to any IPv4 host is
	// explicitly opted in by the operator, and is absent otherwise.
	EnvAllowSSH = "SCION_INIT_FIREWALL_ALLOW_SSH"
	// EnvPrefix is the reserved environment-variable prefix stripped from
	// agent container environments. It deliberately includes the trailing
	// underscore so bare SCION_INIT_FIREWALL remains user-settable.
	EnvPrefix = "SCION_INIT_FIREWALL_"

	// MaxDomainsPerSource is the maximum number of unique domains accepted
	// from a single policy source (broker config or hub admin config).
	MaxDomainsPerSource = 64
	// MaxDomainsTotal is the maximum number of unique domains accepted in a
	// combined policy (the union of broker and hub policies).
	MaxDomainsTotal = 128

	maxDomainLength = 253
	maxLabelLength  = 63
)

// Policy holds a canonical, validated egress firewall policy.
// AllowDomains is deduplicated and sorted in lexicographic order.
type Policy struct {
	AllowDomains []string
	AllowSSH     bool
}

// IsEmpty reports whether the policy adds neither extra domains nor SSH access.
func (p Policy) IsEmpty() bool {
	return len(p.AllowDomains) == 0 && !p.AllowSSH
}

// Normalize validates and canonicalizes a single-source domain list and SSH
// opt-in flag. The returned Policy contains the exact strings that passed
// validation, deduplicated and sorted, capped at MaxDomainsPerSource (64).
func Normalize(domains []string, allowSSH bool) (Policy, error) {
	return normalizeWithLimit(domains, allowSSH, MaxDomainsPerSource)
}

// Union combines two policies into a single canonical policy: domains are the
// deduplicated, sorted union, and AllowSSH is true if either source sets it.
// Every entry is re-validated, and the combined list is capped at
// MaxDomainsTotal (128).
func Union(a, b Policy) (Policy, error) {
	combined := make([]string, 0, len(a.AllowDomains)+len(b.AllowDomains))
	combined = append(combined, a.AllowDomains...)
	combined = append(combined, b.AllowDomains...)
	p, err := normalizeWithLimit(combined, a.AllowSSH || b.AllowSSH, MaxDomainsTotal)
	if err != nil {
		return Policy{}, fmt.Errorf("combined hub and broker init_firewall policy: %w", err)
	}
	return p, nil
}

// Env renders the policy as "KEY=VALUE" environment entries suitable for
// passing to sciontool netns-setup and init-firewall.sh. An empty policy
// returns nil.
func (p Policy) Env() []string {
	if p.IsEmpty() {
		return nil
	}
	env := make([]string, 0, 2)
	if len(p.AllowDomains) > 0 {
		env = append(env, EnvAllowDomains+"="+strings.Join(p.AllowDomains, ","))
	}
	if p.AllowSSH {
		env = append(env, EnvAllowSSH+"=true")
	}
	return env
}

// FromEnv parses and re-validates a Policy from environment variables using
// lookup (defaulting to os.LookupEnv when nil).
//
// EnvAllowDomains is split on "," and validated against MaxDomainsTotal (128).
// EnvAllowSSH accepts only "true" or absence/empty; any other value is an error.
func FromEnv(lookup func(string) (string, bool)) (Policy, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	var rawDomains []string
	if raw, ok := lookup(EnvAllowDomains); ok && raw != "" {
		rawDomains = strings.Split(raw, ",")
	}

	var allowSSH bool
	if raw, ok := lookup(EnvAllowSSH); ok && raw != "" {
		if raw != "true" {
			return Policy{}, fmt.Errorf("invalid %s value %q: must be \"true\" or unset", EnvAllowSSH, raw)
		}
		allowSSH = true
	}

	return normalizeWithLimit(rawDomains, allowSSH, MaxDomainsTotal)
}

func normalizeWithLimit(domains []string, allowSSH bool, maxDomains int) (Policy, error) {
	if len(domains) == 0 {
		return Policy{AllowSSH: allowSSH}, nil
	}

	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, raw := range domains {
		norm, err := normalizeDomain(raw)
		if err != nil {
			return Policy{}, err
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}

	sort.Strings(out)
	if len(out) > maxDomains {
		return Policy{}, fmt.Errorf("init_firewall allow_domains has %d unique entries, exceeding maximum of %d", len(out), maxDomains)
	}
	return Policy{
		AllowDomains: out,
		AllowSSH:     allowSSH,
	}, nil
}

func normalizeDomain(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	e = strings.ToLower(e)
	e = strings.TrimSuffix(e, ".")

	if e == "" {
		return "", fmt.Errorf("invalid init_firewall domain %q: entry is empty", raw)
	}
	if len(e) > maxDomainLength {
		return "", fmt.Errorf("invalid init_firewall domain %q: length %d exceeds %d", raw, len(e), maxDomainLength)
	}
	for i := 0; i < len(e); i++ {
		b := e[i]
		isLower := b >= 'a' && b <= 'z'
		isDigit := b >= '0' && b <= '9'
		if !isLower && !isDigit && b != '.' && b != '-' {
			return "", fmt.Errorf("invalid init_firewall domain %q: contains disallowed character", raw)
		}
	}
	if net.ParseIP(e) != nil {
		return "", fmt.Errorf("invalid init_firewall domain %q: IP literals are not allowed", raw)
	}

	labels := strings.Split(e, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("invalid init_firewall domain %q: must have at least two labels", raw)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > maxLabelLength {
			return "", fmt.Errorf("invalid init_firewall domain %q: label %q has invalid length", raw, label)
		}
		if !isASCIIAlnum(label[0]) || !isASCIIAlnum(label[len(label)-1]) {
			return "", fmt.Errorf("invalid init_firewall domain %q: label %q must start and end with an ASCII letter or digit", raw, label)
		}
	}
	if isAllDigits(labels[len(labels)-1]) {
		return "", fmt.Errorf("invalid init_firewall domain %q: numeric top-level label is not allowed", raw)
	}
	if e == "localhost" || strings.HasSuffix(e, ".localhost") {
		return "", fmt.Errorf("invalid init_firewall domain %q: localhost domains are not allowed", raw)
	}
	return e, nil
}

func isASCIIAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
