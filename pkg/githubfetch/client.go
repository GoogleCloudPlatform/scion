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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// RefKind classifies what a ResolvedRef resolved from.
type RefKind string

const (
	RefBranch  RefKind = "branch"
	RefTag     RefKind = "tag"
	RefSHA     RefKind = "sha"
	RefDefault RefKind = "default"
)

// ResolvedRef is the result of Client.ResolveRef.
type ResolvedRef struct {
	CommitSHA string
	Ref       string
	Kind      RefKind
	ETag      string
}

// FolderResult describes a materialized folder returned by Client.FetchFolder.
type FolderResult struct {
	CommitSHA string
	TreeSHA   string
	Files     int
	Bytes     int64
	Skipped   SkipCounts
	Strategy  string
}

// Options configures a Client.
type Options struct {
	Endpoint Endpoint
	Doer     Doer
	Cooldown *Cooldown
	Clock    func() time.Time
	// MaxRetries caps transient 5xx and network retries per HTTP call (0 means
	// no retries; negative values are treated as 0).
	MaxRetries int
}

// Client resolves refs, lists trees, and fetches folders from GitHub.
type Client struct {
	endpoint   Endpoint
	doer       Doer
	cooldown   *Cooldown
	clock      func() time.Time
	maxRetries int
}

// New constructs a Client with defaults for omitted fields.
func New(opts Options) *Client {
	ep := opts.Endpoint.withDefaults()
	cd := opts.Cooldown
	if cd == nil {
		cd = SharedCooldown()
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	retries := opts.MaxRetries
	if retries < 0 {
		retries = 0
	}
	return &Client{
		endpoint:   ep,
		doer:       opts.Doer,
		cooldown:   cd,
		clock:      clk,
		maxRetries: retries,
	}
}

func nowUTC() time.Time { return time.Now().UTC() }

// Endpoint returns the client's effective Endpoint.
func (c *Client) Endpoint() Endpoint { return c.endpoint }

// ResolveRef resolves s to a 40-hex commit SHA and returns the disambiguated
// Source (with Ref and Path finalized and RefAmbiguous cleared).
//
//   - Full 40-hex SHA: no network call.
//   - Ambiguous /tree/a/b/c: queries git/matching-refs/heads/{a} then
//     git/matching-refs/tags/{a} and picks the longest matching segment prefix.
//   - Empty ref: queries /repos/{o}/{r} for default_branch, then resolves it.
//   - Unambiguous ref: queries /repos/{o}/{r}/commits/{PathEscape(ref)} with
//     Accept: application/vnd.github.sha.
func (c *Client) ResolveRef(ctx context.Context, s Source, cred Credential) (ResolvedRef, Source, error) {
	if s.Owner == "" || s.Repo == "" {
		return ResolvedRef{}, s, &Error{Kind: KindInvalidSource, Hint: "missing owner or repo"}
	}

	// Fast path: unambiguous full 40-hex commit SHA requires no network call.
	if len(s.RefAmbiguous) == 0 && IsFullCommitSHA(s.Ref) {
		return ResolvedRef{
			CommitSHA: s.Ref,
			Ref:       s.Ref,
			Kind:      RefSHA,
		}, s, nil
	}

	// Disambiguate multi-segment /tree/<rest> URLs via git/matching-refs.
	if len(s.RefAmbiguous) > 0 {
		matched, sha, kind, objType, rest, err := c.disambiguateRef(ctx, s, cred)
		if err != nil {
			return ResolvedRef{}, s, err
		}
		resolved := s
		resolved.Ref = matched
		resolved.Path = strings.Join(rest, "/")
		resolved.RefAmbiguous = nil
		if objType == "commit" && IsFullCommitSHA(sha) {
			return ResolvedRef{
				CommitSHA: sha,
				Ref:       matched,
				Kind:      kind,
			}, resolved, nil
		}
		// Annotated tags (object.type == "tag") or non-commit refs: dereference
		// to the commit SHA via commits/{ref}.
		res, err := c.resolveCommitSHA(ctx, resolved, matched, kind, cred)
		if err != nil {
			return ResolvedRef{}, s, err
		}
		return res, resolved, nil
	}

	// Empty ref: fetch repository metadata to discover default_branch.
	if s.Ref == "" {
		defBranch, err := c.fetchDefaultBranch(ctx, s, cred)
		if err != nil {
			return ResolvedRef{}, s, err
		}
		resolved := s
		resolved.Ref = defBranch
		res, err := c.resolveCommitSHA(ctx, resolved, defBranch, RefDefault, cred)
		if err != nil {
			return ResolvedRef{}, s, err
		}
		return res, resolved, nil
	}

	// Unambiguous non-empty ref.
	res, err := c.resolveCommitSHA(ctx, s, s.Ref, RefBranch, cred)
	if err != nil {
		return ResolvedRef{}, s, err
	}
	return res, s, nil
}

type matchingRefEntry struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func (c *Client) disambiguateRef(ctx context.Context, s Source, cred Credential) (refName, sha string, kind RefKind, objType string, pathRest []string, err error) {
	firstSeg := s.RefAmbiguous[0]

	// Try heads first (matches git ls-remote --heads precedence), then tags.
	for _, ns := range []struct {
		prefix string
		kind   RefKind
	}{
		{"refs/heads/", RefBranch},
		{"refs/tags/", RefTag},
	} {
		apiPath := "heads/" + url.PathEscape(firstSeg)
		if ns.kind == RefTag {
			apiPath = "tags/" + url.PathEscape(firstSeg)
		}
		u := c.endpoint.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
			"/git/matching-refs/" + apiPath
		hdr := http.Header{"Accept": {"application/vnd.github+json"}}
		resp, err := c.doAPI(ctx, Request{URL: u, Header: hdr, MaxBytes: maxTreeResponseBytes}, s.RepoSlug(), cred)
		if err != nil {
			var ghe *Error
			if errors.As(err, &ghe) && ghe.Kind == KindNotFound {
				continue
			}
			return "", "", "", "", nil, err
		}
		var entries []matchingRefEntry
		decErr := json.NewDecoder(io.LimitReader(resp.Body, maxTreeResponseBytes)).Decode(&entries)
		_ = resp.Body.Close()
		if decErr != nil {
			return "", "", "", "", nil, &Error{Kind: KindUpstream, Repo: s.RepoSlug(), Hint: "invalid matching-refs response", err: decErr}
		}
		bestSegs := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Ref, ns.prefix) {
				continue
			}
			name := strings.TrimPrefix(e.Ref, ns.prefix)
			nameSegs := strings.Split(name, "/")
			if len(nameSegs) > len(s.RefAmbiguous) || len(nameSegs) <= bestSegs {
				continue
			}
			if segmentsMatchPrefix(s.RefAmbiguous, nameSegs) {
				bestSegs = len(nameSegs)
				refName = name
				sha = e.Object.SHA
				kind = ns.kind
				objType = e.Object.Type
				pathRest = s.RefAmbiguous[len(nameSegs):]
			}
		}
		if bestSegs > 0 {
			return refName, sha, kind, objType, pathRest, nil
		}
	}

	return "", "", "", "", nil, &Error{
		Kind:     KindNotFound,
		Status:   http.StatusNotFound,
		Repo:     s.RepoSlug(),
		CredKind: cred.Kind,
		Hint:     cred.Hint,
	}
}

func segmentsMatchPrefix(full, prefix []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if full[i] != prefix[i] {
			return false
		}
	}
	return true
}

func (c *Client) fetchDefaultBranch(ctx context.Context, s Source, cred Credential) (string, error) {
	u := c.endpoint.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo)
	hdr := http.Header{"Accept": {"application/vnd.github+json"}}
	resp, err := c.doAPI(ctx, Request{URL: u, Header: hdr, MaxBytes: 1 << 20}, s.RepoSlug(), cred)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return "", &Error{Kind: KindUpstream, Repo: s.RepoSlug(), Hint: "invalid repository metadata response", err: err}
	}
	if meta.DefaultBranch == "" {
		return "", &Error{Kind: KindUpstream, Repo: s.RepoSlug(), Hint: "repository metadata missing default_branch"}
	}
	return meta.DefaultBranch, nil
}

func (c *Client) resolveCommitSHA(ctx context.Context, s Source, ref string, kind RefKind, cred Credential) (ResolvedRef, error) {
	u := c.endpoint.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
		"/commits/" + url.PathEscape(ref)
	hdr := http.Header{"Accept": {"application/vnd.github.sha"}}
	resp, err := c.doAPI(ctx, Request{URL: u, Header: hdr, MaxBytes: 1024}, s.RepoSlug(), cred)
	if err != nil {
		return ResolvedRef{}, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	if readErr != nil {
		return ResolvedRef{}, ClassifyRemoteFetchError(readErr, s.RepoSlug(), cred, c.clock())
	}
	sha := strings.TrimSpace(string(body))
	if !IsFullCommitSHA(sha) {
		return ResolvedRef{}, &Error{Kind: KindUpstream, Repo: s.RepoSlug(), Hint: "invalid commit SHA response"}
	}
	var etag string
	if resp.Header != nil {
		etag = resp.Header.Get("ETag")
	}
	return ResolvedRef{
		CommitSHA: sha,
		Ref:       ref,
		Kind:      kind,
		ETag:      etag,
	}, nil
}

// ListTree lists the recursive git tree for s.Path at commitSHA and validates
// all entries against l before any file bytes are downloaded.
func (c *Client) ListTree(ctx context.Context, s Source, commitSHA string, cred Credential, l Limits) (Tree, error) {
	treeish := url.PathEscape(commitSHA)
	if trimmed := strings.Trim(s.Path, "/"); trimmed != "" {
		treeish += ":" + escapePathSegments(trimmed)
	}
	u := c.endpoint.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
		"/git/trees/" + treeish + "?recursive=1"
	hdr := http.Header{"Accept": {"application/vnd.github+json"}}
	resp, err := c.doAPI(ctx, Request{URL: u, Header: hdr, MaxBytes: maxTreeResponseBytes}, s.RepoSlug(), cred)
	if err != nil {
		return Tree{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeTree(resp.Body, s.RepoSlug(), l)
}

// FetchFolder resolves s, lists and validates its git tree, and materializes
// its files into dst (*os.Root). Phase 1 implements the streamed single-pass
// tarball branch (Phase 3 adds the small-folder raw branch without changing
// this signature).
func (c *Client) FetchFolder(ctx context.Context, s Source, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	l = l.withDefaults()
	if l.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.Timeout)
		defer cancel()
	}

	resolved, resolvedSrc, err := c.ResolveRef(ctx, s, cred)
	if err != nil {
		return FolderResult{}, err
	}

	tree, err := c.ListTree(ctx, resolvedSrc, resolved.CommitSHA, cred, l)
	if err != nil {
		return FolderResult{}, err
	}

	return c.fetchFolderTarball(ctx, resolvedSrc, resolved.CommitSHA, &tree, cred, dst, l)
}

// FetchFolderResolved materializes a folder whose commitSHA and tree have
// already been resolved and validated by the caller (e.g., when a caller checks
// tree.TreeSHA for a no-op before downloading).
func (c *Client) FetchFolderResolved(ctx context.Context, s Source, commitSHA string, tree *Tree, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	l = l.withDefaults()
	if l.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.Timeout)
		defer cancel()
	}
	return c.fetchFolderTarball(ctx, s, commitSHA, tree, cred, dst, l)
}

func (c *Client) fetchFolderTarball(ctx context.Context, s Source, commitSHA string, tree *Tree, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	var req Request
	checkCooldown := false
	if !cred.IsAnonymous() {
		req = Request{
			URL:           c.endpoint.TarballURL(s, commitSHA),
			Authorization: "Bearer " + cred.Token,
			MaxBytes:      l.MaxDownload,
		}
		checkCooldown = true
	} else {
		req = Request{
			URL:      c.endpoint.ArchiveURL(s, commitSHA),
			MaxBytes: l.MaxDownload,
		}
	}

	resp, err := c.do(ctx, req, s.RepoSlug(), cred, checkCooldown)
	if err != nil {
		return FolderResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	files, written, skipped, err := extractTarGz(resp.Body, s.RepoSlug(), s.Path, commitSHA, tree, dst, l)
	if err != nil {
		return FolderResult{}, err
	}

	treeSHA := ""
	if tree != nil {
		treeSHA = tree.TreeSHA
	}
	return FolderResult{
		CommitSHA: commitSHA,
		TreeSHA:   treeSHA,
		Files:     files,
		Bytes:     written,
		Skipped:   skipped,
		Strategy:  "tarball",
	}, nil
}

func (c *Client) doAPI(ctx context.Context, req Request, repo string, cred Credential) (*Response, error) {
	if !cred.IsAnonymous() {
		req.Authorization = "Bearer " + cred.Token
	}
	return c.do(ctx, req, repo, cred, true)
}

func (c *Client) do(ctx context.Context, req Request, repo string, cred Credential, checkCooldown bool) (*Response, error) {
	if c.doer == nil {
		return nil, &Error{Kind: KindUnreachable, Repo: repo, Hint: "no HTTP transport configured"}
	}
	scope := cred.EffectiveScope()
	if checkCooldown {
		if until, active := c.cooldown.Active(scope); active {
			ra := until.Sub(c.clock())
			if ra < 0 {
				ra = 0
			}
			return nil, &Error{
				Kind:       KindRateLimited,
				Status:     http.StatusTooManyRequests,
				RetryAfter: ra,
				Repo:       repo,
				CredKind:   cred.Kind,
				Hint:       cred.Hint,
			}
		}
	}

	attempts := c.maxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var lastErr *Error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * 200 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, &Error{Kind: KindTimeout, Repo: repo, CredKind: cred.Kind}
			case <-time.After(backoff):
			}
		}

		resp, err := c.doer.Open(ctx, req)
		if err != nil {
			ghe := ClassifyRemoteFetchError(err, repo, cred, c.clock())
			if ghe.Kind == KindRateLimited {
				d := ghe.RetryAfter
				if d <= 0 {
					d = CooldownDefault
				}
				c.cooldown.Record(scope, d)
				return nil, ghe
			}
			lastErr = ghe
			if isRetryableError(ghe) && attempt+1 < attempts {
				continue
			}
			return nil, ghe
		}

		if resp.Status < 200 || resp.Status >= 300 {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, secondaryLimitPeek))
			_ = resp.Body.Close()
			ghe := ClassifyStatus(resp.Status, resp.Header, snippet, repo, cred, c.clock())
			if ghe.Kind == KindRateLimited {
				d := ghe.RetryAfter
				if d <= 0 {
					d = CooldownDefault
				}
				c.cooldown.Record(scope, d)
				return nil, ghe
			}
			lastErr = ghe
			if isRetryableError(ghe) && attempt+1 < attempts {
				continue
			}
			return nil, ghe
		}

		c.cooldown.ClearIfElapsed(scope)
		return resp, nil
	}
	return nil, lastErr
}

func isRetryableError(e *Error) bool {
	if e == nil {
		return false
	}
	if e.transient {
		return true
	}
	if e.Kind == KindUpstream && e.Status >= 500 && e.Status <= 599 {
		return true
	}
	return false
}
