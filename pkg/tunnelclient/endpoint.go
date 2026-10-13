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

package tunnelclient

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// RedactEndpoint returns the hub endpoint as it is shown in status
// records, status output and error lines: scheme, host, port and path
// only, never userinfo, a query or a fragment, with trailing slashes
// removed. An endpoint that does not parse as a URL with a host is shown
// as "the configured hub endpoint".
func RedactEndpoint(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "the configured hub endpoint"
	}
	r := url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimRight(u.Path, "/")}
	return r.String()
}

// NormalizeEndpoint returns the form of a hub endpoint used to identify a
// session locally: the redacted endpoint (RedactEndpoint) with scheme and
// host lowercased and the scheme's default port dropped. It follows the
// CLI's rule for comparing hub URLs. Input that is not a URL with a host
// gives "invalid:" and a short digest of the input, so different invalid
// inputs never compare equal and no input text is kept.
//
// Normalisation here is for session reuse and comparison. The CLI's
// credential lookup normalises endpoints with config.NormalizeEndpoint;
// under it a different scheme, host, port or path never matches a stored
// login. The connection uses the endpoint as configured.
func NormalizeEndpoint(endpoint string) string {
	s := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Not a URL with a host: a key distinct per input that holds
		// none of the input's text.
		sum := sha256.Sum256([]byte(strings.ToLower(s)))
		return "invalid:" + hex.EncodeToString(sum[:8])
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	r := url.URL{Scheme: u.Scheme, Path: strings.TrimRight(u.Path, "/")}
	switch {
	case port != "":
		r.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		r.Host = "[" + host + "]"
	default:
		r.Host = host
	}
	return r.String()
}

// scrubEndpoint removes the endpoint's userinfo, query and fragment from
// text (for example, a transport error that quotes the URL).
func scrubEndpoint(text, endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return text
	}
	if u.User != nil {
		name := u.User.Username()
		text = strings.ReplaceAll(text, u.User.String()+"@", "")
		text = strings.ReplaceAll(text, name+":***@", "")
		if name != "" {
			text = strings.ReplaceAll(text, name+"@", "")
		}
		if pw, ok := u.User.Password(); ok && pw != "" {
			text = strings.ReplaceAll(text, pw, "***")
		}
	}
	if u.RawQuery != "" {
		text = strings.ReplaceAll(text, "?"+u.RawQuery, "")
		text = strings.ReplaceAll(text, u.RawQuery, "")
		for _, vs := range u.Query() {
			for _, v := range vs {
				if len(v) >= 3 {
					text = strings.ReplaceAll(text, v, "***")
				}
			}
		}
	}
	if u.Fragment != "" {
		text = strings.ReplaceAll(text, "#"+u.EscapedFragment(), "")
	}
	return text
}

// SessionKey identifies a session locally: the normalised endpoint and the
// credential identity. Two invocations share a session only when both
// match.
type SessionKey struct {
	Endpoint string
	Kind     string
	Subject  string
}

// KeyFor returns the session key for endpoint and id.
func KeyFor(endpoint string, id Identity) SessionKey {
	return SessionKey{Endpoint: NormalizeEndpoint(endpoint), Kind: id.Kind, Subject: id.Subject}
}

var (
	// agentRefPattern is an agent slug ([a-z0-9-], as pkg/api slugs) or
	// an agent id.
	agentRefPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	// projectRefPattern is a project slug or a project id (hosted ids
	// join a uuid and a name with "__").
	projectRefPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,198}[a-z0-9])?$`)
)

// validAgentRef reports whether s has the form of an agent slug or id.
func validAgentRef(s string) bool { return agentRefPattern.MatchString(s) }

// validProjectRef reports whether s has the form of a project slug or id.
func validProjectRef(s string) bool { return projectRefPattern.MatchString(s) }
