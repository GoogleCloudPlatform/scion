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

// Package hubname provides hub endpoint normalization and nickname derivation
// shared by pkg/config and sciontool without pulling pkg/config into the
// container init binary.
package hubname

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// hubNameRe is the format of a hub nickname. It never contains ':' or '/',
// so a nickname can always be told apart from a URL.
var hubNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidHubName reports whether name is a valid hub nickname.
func ValidHubName(name string) bool {
	return hubNameRe.MatchString(name)
}

// NormalizeEndpoint returns endpoint in the form in which two spellings of
// one hub URL compare equal: spaces and trailing slashes trimmed, the ASCII
// letters of the scheme and host lowercased, and the scheme's default port
// (80 for http, 443 for https) dropped. A value that does not parse as a
// URL with a host is only trimmed. It is the key of hub entries and of
// stored logins, so two different hubs must never get the same key:
//
//   - only ASCII letters are lowercased; a non-ASCII host is kept as
//     written, because Unicode case mapping folds some characters into
//     ASCII ones (U+212A KELVIN SIGN becomes "k");
//   - an IDN host and its punycode form ("xn--...") are different keys;
//   - a host with a trailing dot ("hub.example.com.") is a different key
//     from the one without;
//   - any non-default port is kept as written ("0443" stays "0443");
//   - the path keeps its case and its escaping.
func NormalizeEndpoint(endpoint string) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return e
	}
	u.Scheme = asciiLower(u.Scheme)
	host := asciiLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	return strings.TrimRight(u.String(), "/")
}

// DeriveHubName returns a nickname for endpoint: the first DNS label of its
// host (an IP address with '.' and ':' replaced by '-'), reduced to the
// nickname format. It returns "hub" when nothing usable is left. Callers
// name the local workstation hub "local" themselves.
func DeriveHubName(endpoint string) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(endpoint)); err == nil {
		host = u.Hostname()
	}
	host = strings.ToLower(host)
	label := host
	if strings.ContainsAny(host, ":") || isDottedIPv4(host) {
		label = strings.NewReplacer(".", "-", ":", "-").Replace(host)
	} else if i := strings.IndexByte(host, '.'); i >= 0 {
		label = host[:i]
	}
	var b strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if !ValidHubName(name) {
		return "hub"
	}
	return name
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func isDottedIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}
