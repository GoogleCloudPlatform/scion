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

package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// DefaultAPIBaseURL is the GitHub.com REST API base URL.
const DefaultAPIBaseURL = "https://api.github.com"

// WebHostForAPIBaseURL returns the git/web host served by the GitHub instance
// whose REST API is at apiBaseURL, canonicalized for comparison: lowercased,
// trailing dot removed, port kept. An empty apiBaseURL means GitHub.com.
//
//   - "" or https://api.github.com           -> github.com
//   - https://ghe.example.com/api/v3         -> ghe.example.com (GHES)
//   - https://api.acme.ghe.com               -> acme.ghe.com (GHE.com)
//
// It returns "" when apiBaseURL does not parse to a host, so a caller
// comparing hosts fails closed.
func WebHostForAPIBaseURL(apiBaseURL string) string {
	raw := strings.TrimSpace(apiBaseURL)
	if raw == "" {
		raw = DefaultAPIBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	host := CanonicalHost(u.Host)
	if host == "" {
		return ""
	}
	// The API of GitHub.com and of GHE.com lives on an "api." subdomain of
	// the web host; GHES serves its API under /api/v3 on the web host itself.
	if rest, ok := strings.CutPrefix(host, "api."); ok && strings.Contains(rest, ".") {
		host = rest
	}
	return host
}

// CanonicalHost lowercases host and removes a trailing dot from its name part
// (keeping any port), so "GitHub.com." and "github.com" compare equal while
// "github.com:8443" stays distinct.
func CanonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	name, port := host, ""
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasSuffix(host, "]") {
		name, port = host[:i], host[i:]
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return ""
	}
	return name + port
}

// sshURLHostPath parses an ssh:// remote URL. It reports isSSH for any
// remote with the ssh:// scheme; host is then the canonical host (with any
// port, "" when the URL does not parse) and path the lowercased repository
// path without slashes at either end or a .git suffix. util.NormalizeGitRemote
// cannot be used for these: it turns "host:port/path" into "host/port/path".
// GHES commonly serves SSH on a non-default port (ssh://git@ghes:2222/o/r);
// the trust boundary is the hostname, so callers ignore the port. The host
// is the one ssh connects to: url.Parse takes userinfo up to the LAST "@" in
// the authority, as ssh does, so ssh://git@ghes@other/... yields "other".
func sshURLHostPath(remote string) (host, path string, isSSH bool) {
	raw := strings.TrimSpace(remote)
	if !strings.HasPrefix(strings.ToLower(raw), "ssh://") {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", true
	}
	path = strings.ToLower(strings.Trim(u.Path, "/"))
	path = strings.TrimSuffix(path, ".git")
	return CanonicalHost(u.Host), path, true
}

// hostWithoutPort strips a trailing :port from a canonical host, keeping a
// bracketed IPv6 literal intact.
func hostWithoutPort(host string) string {
	if strings.HasSuffix(host, "]") {
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		if strings.HasPrefix(host, "[") && !strings.Contains(host[:i], "]") {
			return host
		}
		return host[:i]
	}
	return host
}

// RepoAccess is the caller's permission set on a repository, as GitHub
// reports it in the "permissions" object of GET /repos/{owner}/{repo} for an
// authenticated request.
type RepoAccess struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// ErrRepoAccessDenied means GitHub answered definitively that the token's
// identity cannot see the repository (401, 404, or a 403 that is not a rate
// limit), or the answer carried no permission set.
var ErrRepoAccessDenied = errors.New("github: the user's identity has no access to the repository")

// userAccessHTTPClient is used for user-identity access checks. A short
// timeout keeps a slow GitHub from stalling a token mint; the caller treats
// the timeout as an inconclusive check and refuses (fails closed).
var userAccessHTTPClient = &http.Client{Timeout: 10 * time.Second}

// CheckUserRepoAccess asks GitHub, authenticated as the user who owns
// userToken (never an App installation token, which would always succeed),
// what that user may do on owner/repo.
//
// It returns ErrRepoAccessDenied for a definitive denial. Any other error
// (transport failure, timeout, rate limit, 5xx, an unparsable body) is an
// inconclusive check: the caller must not treat it as access.
func CheckUserRepoAccess(ctx context.Context, apiBaseURL, userToken, owner, repo string) (RepoAccess, error) {
	if userToken == "" || owner == "" || repo == "" {
		return RepoAccess{}, ErrRepoAccessDenied
	}
	base := strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if base == "" {
		base = DefaultAPIBaseURL
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s", base, url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RepoAccess{}, fmt.Errorf("github access check: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := userAccessHTTPClient.Do(req)
	if err != nil {
		return RepoAccess{}, fmt.Errorf("github access check: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RepoAccess{}, fmt.Errorf("github access check: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return RepoAccess{}, fmt.Errorf("github access check: rate limited (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound:
		return RepoAccess{}, ErrRepoAccessDenied
	default:
		return RepoAccess{}, fmt.Errorf("github access check: unexpected HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Permissions *RepoAccess `json:"permissions"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return RepoAccess{}, fmt.Errorf("github access check: parse response: %w", err)
	}
	if parsed.Permissions == nil {
		return RepoAccess{}, ErrRepoAccessDenied
	}
	return *parsed.Permissions, nil
}

// RemoteOwnerRepo returns the owner and repository named by a git remote URL
// when, and only when, the remote's host is wantHost (compared after
// util.NormalizeGitRemote and CanonicalHost, so letter case, a trailing dot,
// userinfo or a scheme cannot make a different host match). A port is part
// of the host, except that an ssh:// remote is compared by hostname with
// both ports ignored (see sshURLHostPath). The path must be exactly owner/repo. The returned names
// keep the remote's letter case; the host and path are validated on the
// normalized form, and the case-preserving names must equal it ignoring case.
//
// Anything else (another host, an empty wantHost, a nested path, a remote the
// normalizer leaves with userinfo) yields ok == false.
func RemoteOwnerRepo(remote, wantHost string) (owner, repo string, ok bool) {
	wantHost = CanonicalHost(wantHost)
	if wantHost == "" {
		return "", "", false
	}
	var path string
	if sshHost, sshPath, isSSH := sshURLHostPath(remote); isSSH {
		// ssh:// URL: the hostname is compared with the port ignored.
		name := hostWithoutPort(sshHost)
		if name == "" || name != hostWithoutPort(wantHost) {
			return "", "", false
		}
		path = sshPath
	} else {
		normalized := util.NormalizeGitRemote(strings.TrimSpace(remote))
		if normalized == "" || strings.Contains(normalized, "@") {
			return "", "", false
		}
		host, p, found := strings.Cut(normalized, "/")
		if !found || CanonicalHost(host) != wantHost {
			return "", "", false
		}
		path = p
	}
	segs := strings.Split(path, "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}

	// Recover the original letter case from the tail of the raw remote.
	raw := strings.TrimRight(strings.TrimSpace(remote), "/")
	raw = strings.TrimSuffix(raw, ".git")
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, ".GIT"), "/")
	rawSegs := strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == ':' })
	if len(rawSegs) >= 2 {
		o, r := rawSegs[len(rawSegs)-2], rawSegs[len(rawSegs)-1]
		if strings.EqualFold(o, segs[0]) && strings.EqualFold(r, segs[1]) {
			return o, r, true
		}
	}
	return segs[0], segs[1], true
}
