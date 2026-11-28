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

// Package githubfetch owns GitHub URL parsing, ref resolution, tree listing,
// folder materialization, transport policy, error classification, rate-limit
// cooldown and cache keys. It is a leaf package that imports only
// pkg/artifacts/remotefetch, pkg/ghauth, pkg/util/* and the standard library.
package githubfetch

import (
	"net/url"
	"strings"
)

const (
	defaultAPIBase  = "https://api.github.com"
	defaultRawBase  = "https://raw.githubusercontent.com"
	defaultWebHost  = "github.com"
	defaultCodeload = "codeload.github.com"
)

// Endpoint identifies the GitHub hosts a Client talks to. Zero fields fall
// back to public github.com defaults. Tests and GitHub Enterprise callers
// override individual fields.
type Endpoint struct {
	APIBase  string // default: https://api.github.com
	RawBase  string // default: https://raw.githubusercontent.com
	WebHost  string // default: github.com
	Codeload string // default: codeload.github.com
}

// DefaultEndpoint returns the public github.com endpoint.
func DefaultEndpoint() Endpoint {
	return Endpoint{
		APIBase:  defaultAPIBase,
		RawBase:  defaultRawBase,
		WebHost:  defaultWebHost,
		Codeload: defaultCodeload,
	}
}

func (e Endpoint) withDefaults() Endpoint {
	if e.APIBase == "" {
		e.APIBase = defaultAPIBase
	}
	e.APIBase = strings.TrimRight(e.APIBase, "/")
	if e.RawBase == "" {
		e.RawBase = defaultRawBase
	}
	e.RawBase = strings.TrimRight(e.RawBase, "/")
	if e.WebHost == "" {
		e.WebHost = defaultWebHost
	}
	if e.Codeload == "" {
		e.Codeload = defaultCodeload
	}
	return e
}

// AllowedHosts returns the deduplicated set of hostnames this endpoint may
// contact across API, raw, web archive, and codeload redirect hops.
func (e Endpoint) AllowedHosts() []string {
	e = e.withDefaults()
	seen := make(map[string]struct{}, 4)
	var out []string
	add := func(raw string) {
		h := extractHostname(raw)
		if h == "" {
			return
		}
		h = strings.ToLower(h)
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	add(e.APIBase)
	add(e.WebHost)
	add(e.Codeload)
	add(e.RawBase)
	return out
}

// APIHost returns the lowercased hostname of APIBase (used for cache keys).
func (e Endpoint) APIHost() string {
	e = e.withDefaults()
	return strings.ToLower(extractHostname(e.APIBase))
}

// RawURL builds the raw file URL for filePath at commitSHA.
func (e Endpoint) RawURL(s Source, commitSHA, filePath string) string {
	e = e.withDefaults()
	fullPath := filePath
	if s.Path != "" {
		if fullPath != "" {
			fullPath = strings.Trim(s.Path, "/") + "/" + strings.Trim(fullPath, "/")
		} else {
			fullPath = strings.Trim(s.Path, "/")
		}
	}
	return e.RawBase + "/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) + "/" +
		url.PathEscape(commitSHA) + "/" + escapePathSegments(fullPath)
}

// TarballURL returns the GitHub REST API tarball URL for commitSHA.
func (e Endpoint) TarballURL(s Source, commitSHA string) string {
	e = e.withDefaults()
	return e.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
		"/tarball/" + url.PathEscape(commitSHA)
}

// ArchiveURL returns the public web archive URL for commitSHA (used for
// anonymous fetches so they do not spend REST API quota).
func (e Endpoint) ArchiveURL(s Source, commitSHA string) string {
	e = e.withDefaults()
	webBase := e.WebHost
	if !strings.Contains(webBase, "://") {
		webBase = "https://" + webBase
	}
	webBase = strings.TrimRight(webBase, "/")
	return webBase + "/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
		"/archive/" + url.PathEscape(commitSHA) + ".tar.gz"
}

func extractHostname(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// escapePathSegments escapes each slash-separated segment of p individually.
func escapePathSegments(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}
