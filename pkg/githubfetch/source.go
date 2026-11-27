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

package githubfetch

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Source is a canonical GitHub repository folder location.
//
// Ref may be "" (default branch), a branch name (which may contain '/'), a tag,
// or a full 40-character lowercase hex commit SHA.
//
// When a /tree/<rest> URL contains multiple segments and the first segment is
// not a full 40-hex commit SHA, RefAmbiguous holds the raw segments of <rest>
// so Client.ResolveRef can disambiguate slashed branches (such as feat/x/y)
// against repository subdirectories without shelling out to git.
type Source struct {
	Owner        string
	Repo         string
	Ref          string
	Path         string
	RefAmbiguous []string
}

// RepoSlug returns "owner/repo".
func (s Source) RepoSlug() string {
	if s.Owner == "" || s.Repo == "" {
		return ""
	}
	return s.Owner + "/" + s.Repo
}

var (
	githubOwnerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	githubRepoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	fullSHARe     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// IsFullCommitSHA reports whether s is a 40-character lowercase hex git commit
// SHA.
func IsFullCommitSHA(s string) bool {
	return fullSHARe.MatchString(s)
}

// ParseURL parses and validates a GitHub folder URL of the form
// https://github.com/<owner>/<repo>[/tree/<ref>[/<path>]].
//
// It rejects non-HTTPS schemes, userinfo, non-443 ports, hosts other than
// github.com, query strings, fragments, non-folder GitHub URLs (archive, blob,
// releases), and unsafe path segments (., .., \, NUL). Error messages never
// echo the raw URL or any credentials.
func ParseURL(raw string) (Source, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	}
	if u.User != nil {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "source URL must not include a username or password"}
	}
	if !strings.EqualFold(u.Hostname(), defaultWebHost) || (u.Port() != "" && u.Port() != "443") {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	}

	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	}

	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if !githubOwnerRe.MatchString(owner) || !githubRepoRe.MatchString(repo) || repo == "." || repo == ".." {
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	}

	src := Source{
		Owner: owner,
		Repo:  repo,
	}

	rest := parts[2:]
	if len(rest) == 0 {
		return src, nil
	}
	switch rest[0] {
	case "tree":
		if len(rest) < 2 {
			return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
		}
		treeSegs := rest[1:]
		for _, seg := range treeSegs {
			if !validSourceSegment(seg) {
				return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
			}
		}
		if len(treeSegs) == 1 {
			src.Ref = treeSegs[0]
			return src, nil
		}
		// Multiple segments after /tree/. A full 40-hex SHA cannot contain '/',
		// so treat it as unambiguous; otherwise record RefAmbiguous for
		// ResolveRef while setting the 1-segment split as the tentative default.
		src.Ref = treeSegs[0]
		src.Path = strings.Join(treeSegs[1:], "/")
		if !IsFullCommitSHA(treeSegs[0]) {
			amb := make([]string, len(treeSegs))
			copy(amb, treeSegs)
			src.RefAmbiguous = amb
		}
		return src, nil
	case "archive", "blob", "releases":
		return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
	default:
		// Bare GitHub folder URL without /tree/ (e.g.
		// https://github.com/<owner>/<repo>/<path...>). Match the legacy
		// parseTemplateGitHubSource and pkg/config parseGitHubURL behaviour:
		// treat the remaining segments as a folder path on branch "main".
		for _, seg := range rest {
			if !validSourceSegment(seg) {
				return Source{}, &Error{Kind: KindInvalidSource, Hint: "unsupported GitHub source URL"}
			}
		}
		src.Ref = "main"
		src.Path = strings.Join(rest, "/")
		return src, nil
	}
}

func validSourceSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." || len(seg) > 255 {
		return false
	}
	if strings.Contains(seg, "..") || strings.ContainsAny(seg, "\\\x00") {
		return false
	}
	return true
}

// RefCacheKey returns the canonical cache key for a resolved ref.
// Owner and repo are lowercased; ref preserves case.
func RefCacheKey(apiHost, owner, repo, ref, scope string) string {
	if scope == "" {
		scope = AnonScope
	}
	return fmt.Sprintf("ref:%s/%s/%s@%s#%s",
		strings.ToLower(apiHost), strings.ToLower(owner), strings.ToLower(repo), ref, scope)
}

// TreeCacheKey returns the canonical cache key for a tree listing at commitSHA.
// Owner and repo are lowercased; path preserves case.
func TreeCacheKey(apiHost, owner, repo, commitSHA, subPath, scope string) string {
	if scope == "" {
		scope = AnonScope
	}
	return fmt.Sprintf("tree:%s/%s/%s@%s:%s#%s",
		strings.ToLower(apiHost), strings.ToLower(owner), strings.ToLower(repo), commitSHA, strings.Trim(subPath, "/"), scope)
}
