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
	"net"
	"net/url"
	"regexp"
	"strings"
)

// NormalizeEndpoint returns the form of a hub endpoint used to identify a
// session locally: surrounding space and trailing slashes removed, scheme
// and host lowercased, and the scheme's default port dropped. It follows
// the CLI's rule for comparing hub URLs.
//
// Normalisation is for session reuse only. The credential lookup is exact
// by design: it receives the endpoint exactly as given.
func NormalizeEndpoint(endpoint string) string {
	s := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
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
