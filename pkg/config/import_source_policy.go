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
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Hub import source policy.
//
// A hosted Hub fetches template and harness-config sources (import, discover,
// harness-config reimport, skill discovery) on its own host, with its own
// network position and service account. FetchRemoteTemplate accepts any
// rclone connection string and any http(s) URL, which is right for a CLI
// user fetching onto their own machine (and for a local workstation Hub) but
// not for a hosted server that fetches on behalf of project members.
//
// CheckHubImportSource is the gate for hosted server-side fetches. By default
// it accepts only https://github.com/... URLs. An operator can allow more
// sources with server.hub.allowed_import_sources (AllowedImportSourcesKey):
// a list of https:// URL prefixes and Google Cloud Storage prefixes
// (gs://<bucket>/<prefix> or :gcs:<bucket>/<prefix>). Nothing else can be
// allowed: plain http, the rclone local backend, other rclone backends, and
// rclone connection strings that carry inline backend options (":gcs,k=v:").
//
// The CLI does not use this gate: `scion templates import` and
// `scion harness-config install` fetch on the user's own machine.

// AllowedImportSourcesKey is the settings key of
// HubServerConfig.AllowedImportSources. It is set only in the settings file
// (no environment variable; the server-config API refuses it in every mode),
// so a web session or env override cannot widen what a hosted Hub fetches.
const AllowedImportSourcesKey = "server.hub.allowed_import_sources"

// ErrImportSourceNotAllowed is returned by CheckHubImportSource for a source
// a hosted Hub does not fetch. Its message never includes the source.
var ErrImportSourceNotAllowed = errors.New(
	"the Hub imports only from https://github.com/... URLs; " +
		"other sources must be allowed by a Hub administrator (" + AllowedImportSourcesKey + ")")

// gcsBucketRe matches a Google Cloud Storage bucket name: 3-222 characters
// of lowercase letters, digits, '-', '_' and '.', starting and ending with a
// letter or digit.
var gcsBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// importSource is a parsed, canonical import source.
type importSource struct {
	// canon is the form compared with allowlist entries: "https://host/path"
	// (lowercase scheme and host, no port, query or fragment) or
	// "gs://bucket/path".
	canon string
	// fetch is the URI handed to FetchRemoteTemplate.
	fetch string
	// github is true for an https://github.com URL.
	github bool
}

// CheckHubImportSource reports whether a hosted Hub may fetch raw, a template
// or harness-config source URL, under allowlist (the normalized
// server.hub.allowed_import_sources). It returns the URI to fetch: raw with
// its scheme and host lowercased, or the :gcs: form of a gs:// URL. A refused
// source yields an error wrapping ErrImportSourceNotAllowed that never
// repeats raw.
func CheckHubImportSource(raw string, allowlist []string) (string, error) {
	src, err := parseImportSource(raw)
	if err != nil {
		return "", err
	}
	if src.github {
		return src.fetch, nil
	}
	for _, entry := range allowlist {
		if importPrefixMatches(src.canon, entry) {
			return src.fetch, nil
		}
	}
	return "", ErrImportSourceNotAllowed
}

// NormalizeAllowedImportSources checks server.hub.allowed_import_sources
// and returns its entries in canonical form. Each entry is an https:// URL
// prefix (scheme and host, optionally a path; no username, password, port
// other than 443, query or fragment) or a Google Cloud Storage prefix
// (gs://<bucket>[/<prefix>] or :gcs:<bucket>[/<prefix>]). Entries are
// matched on whole path segments: "gs://b/templates" allows
// "gs://b/templates/x", not "gs://b/templates-old". Errors name the entry by
// position only, since an entry could hold a credential.
func NormalizeAllowedImportSources(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(entries))
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		src, err := parseImportSource(e)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] must be an https:// URL prefix or a gs://<bucket>/<prefix> (or :gcs:<bucket>/<prefix>) prefix "+
				"with no username, password, port, query, fragment, '.' or '..' segment, or rclone options",
				AllowedImportSourcesKey, i)
		}
		out = append(out, src.canon)
	}
	return out, nil
}

// importPrefixMatches reports whether canon is entry or lies below it, on
// whole path segments.
func importPrefixMatches(canon, entry string) bool {
	entry = strings.TrimRight(entry, "/")
	if entry == "" {
		return false
	}
	return canon == entry || strings.HasPrefix(canon, entry+"/")
}

// parseImportSource parses raw as one of the source forms a hosted Hub can be
// allowed to fetch. Any other form, and any form with an ambiguous path,
// wraps ErrImportSourceNotAllowed.
func parseImportSource(raw string) (*importSource, error) {
	if raw == "" || strings.ContainsFunc(raw, unicode.IsControl) {
		return nil, ErrImportSourceNotAllowed
	}
	switch {
	case strings.HasPrefix(raw, ":"):
		return parseGCSImportSource(raw, ":gcs:")
	case len(raw) >= len("gs://") && strings.EqualFold(raw[:len("gs://")], "gs://"):
		return parseGCSImportSource(raw, raw[:len("gs://")])
	default:
		return parseHTTPSImportSource(raw)
	}
}

// parseGCSImportSource parses a ":gcs:<bucket>/<path>" or
// "gs://<bucket>/<path>" source; prefix is the form's scheme part as it
// appears in raw. Any other rclone connection string (another backend, a
// backend with inline options such as ":gcs,endpoint=...:", a named remote)
// is refused.
func parseGCSImportSource(raw, prefix string) (*importSource, error) {
	if !strings.HasPrefix(raw, prefix) {
		return nil, ErrImportSourceNotAllowed
	}
	rest := raw[len(prefix):]
	bucket, objPath, _ := strings.Cut(rest, "/")
	if !gcsBucketRe.MatchString(bucket) || strings.Contains(bucket, "..") {
		return nil, ErrImportSourceNotAllowed
	}
	// The path is taken literally by rclone; refuse characters rclone or a
	// shell-style reader could treat specially, and ambiguous segments.
	if strings.ContainsAny(objPath, `:,\"'{}*?[]`) || !cleanImportPath(objPath) {
		return nil, ErrImportSourceNotAllowed
	}
	canon := "gs://" + bucket
	fetch := ":gcs:" + bucket
	if objPath != "" {
		canon += "/" + objPath
		fetch += "/" + objPath
	}
	return &importSource{canon: canon, fetch: fetch}, nil
}

// parseHTTPSImportSource parses an https:// URL source. github is set for
// https://github.com. Query strings and fragments are refused on all HTTPS
// sources (including github.com) so only clean paths are accepted.
func parseHTTPSImportSource(raw string) (*importSource, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return nil, ErrImportSourceNotAllowed
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || (u.Port() != "" && u.Port() != "443") {
		return nil, ErrImportSourceNotAllowed
	}
	if strings.ContainsAny(u.Path, `\`) || !cleanImportPath(strings.TrimPrefix(u.Path, "/")) {
		return nil, ErrImportSourceNotAllowed
	}
	// An encoded '/' or '\' would make the decoded path, which is what is
	// compared, differ from what the server sees.
	if lower := strings.ToLower(u.RawPath); strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return nil, ErrImportSourceNotAllowed
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, ErrImportSourceNotAllowed
	}
	u.Scheme = "https"
	u.Host = host
	isGitHub := host == "github.com" && u.Port() == ""
	canon := "https://" + host + u.Path
	return &importSource{canon: canon, fetch: u.String(), github: isGitHub}, nil
}

// cleanImportPath reports whether p (a path with no leading '/') has no "."
// or ".." segment and no empty segment except a trailing one.
func cleanImportPath(p string) bool {
	if p == "" {
		return true
	}
	segs := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}
