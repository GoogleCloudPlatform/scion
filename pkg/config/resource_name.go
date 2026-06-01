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
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// DeriveResourceName returns the intended resource name from a source URL or
// path: the last meaningful path segment. For example:
//
//	https://github.com/org/repo/tree/main/antigravity -> "antigravity"
//	:gcs:my-bucket/harness-configs/prod-claude        -> "prod-claude"
//	file:///path/to/my-config                         -> "my-config"
//
// It handles file:// URLs, rclone connection strings (":backend:path"), http(s)
// URLs (including bare "github.com/..." shorthands), and plain local paths.
// Returns "" if no name can be derived.
//
// This is the single shared rule used by both the CLI and the Hub import path so
// that a deep URL such as ".../tree/main/antigravity" yields "antigravity"
// rather than the cache-hash directory name.
func DeriveResourceName(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}

	if strings.HasPrefix(source, "file://") {
		localPath := strings.TrimPrefix(source, "file://")
		return sanitizeDerivedName(filepath.Base(filepath.Clean(localPath)))
	}

	// rclone connection string, e.g. ":gcs:bucket/path".
	if strings.HasPrefix(source, ":") {
		parts := strings.SplitN(source, ":", 3)
		if len(parts) == 3 {
			return sanitizeDerivedName(path.Base(strings.TrimRight(parts[2], "/")))
		}
		return ""
	}

	normalized := normalizeResourceSourceURL(source)
	u, err := url.Parse(normalized)
	if err != nil {
		return sanitizeDerivedName(filepath.Base(filepath.Clean(source)))
	}

	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath == "" {
		return sanitizeDerivedName(filepath.Base(filepath.Clean(source)))
	}
	return sanitizeDerivedName(path.Base(cleanPath))
}

// sanitizeDerivedName drops base-name results that carry no real name —
// filepath.Base/path.Base yield "." for empty/relative input and "/" (or the
// platform separator) for a root path. Returning those as a resource name would
// produce a nonsense slug, so collapse them to "" (callers treat "" as "no name
// derived" and fall back to the on-disk directory name).
func sanitizeDerivedName(name string) string {
	if name == "." || name == "/" || name == `\` {
		return ""
	}
	return name
}

// normalizeResourceSourceURL prepends "https://" for bare hostnames (e.g.
// "github.com/org/repo/...") so the leaf segment can be extracted via url.Parse.
// Unlike NormalizeTemplateSourceURL it does not append a default templates path;
// it only fixes the scheme so naming is consistent across sources.
func normalizeResourceSourceURL(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "file://") {
		return s
	}
	if !strings.HasPrefix(s, ":") && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		if strings.Contains(s, "github.com") || strings.Contains(s, "/") && strings.Contains(strings.SplitN(s, "/", 2)[0], ".") {
			s = "https://" + s
		}
	}
	return s
}
