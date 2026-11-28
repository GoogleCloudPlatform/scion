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
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// Folder materialization strategy names reported in FolderResult.Strategy.
const (
	StrategyRaw     = "raw"
	StrategyTarball = "tarball"
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
	// Skipped records non-fatal skipped entries. Counts are populated by the
	// tarball strategy only (the raw strategy leaves Skipped zero).
	Skipped  SkipCounts
	Strategy string
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
	// RawMaxFiles sets the default maximum file count for the raw strategy in
	// FetchFolder when Limits.RawMaxFiles is zero (0 defaults to
	// DefaultRawMaxFiles; negative disables raw and forces tarball).
	RawMaxFiles int
	// RawMaxBytes sets the default maximum total bytes for the raw strategy in
	// FetchFolder when Limits.RawMaxBytes is zero (0 defaults to
	// DefaultRawMaxBytes; negative disables raw and forces tarball).
	RawMaxBytes int64
	// RawConcurrency bounds concurrent per-file raw downloads in FetchFolder
	// (<= 0 defaults to DefaultRawConcurrency).
	RawConcurrency int
}

// Client resolves refs, lists trees, and fetches folders from GitHub.
type Client struct {
	endpoint       Endpoint
	doer           Doer
	cooldown       *Cooldown
	clock          func() time.Time
	maxRetries     int
	rawMaxFiles    int
	rawMaxBytes    int64
	rawConcurrency int
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
	concurrency := opts.RawConcurrency
	if concurrency <= 0 {
		concurrency = DefaultRawConcurrency
	}
	return &Client{
		endpoint:       ep,
		doer:           opts.Doer,
		cooldown:       cd,
		clock:          clk,
		maxRetries:     retries,
		rawMaxFiles:    opts.RawMaxFiles,
		rawMaxBytes:    opts.RawMaxBytes,
		rawConcurrency: concurrency,
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
	return c.listTree(ctx, s, commitSHA, cred, l, true)
}

// ListTreeShallow lists the non-recursive git tree for s.Path at commitSHA
// (without ?recursive=1). Its top-level TreeSHA is the folder's git tree SHA,
// suitable for lightweight drift checks without enumerating nested subtrees.
func (c *Client) ListTreeShallow(ctx context.Context, s Source, commitSHA string, cred Credential, l Limits) (Tree, error) {
	return c.listTree(ctx, s, commitSHA, cred, l, false)
}

func (c *Client) listTree(ctx context.Context, s Source, commitSHA string, cred Credential, l Limits, recursive bool) (Tree, error) {
	treeish := url.PathEscape(commitSHA)
	if trimmed := strings.Trim(s.Path, "/"); trimmed != "" {
		treeish += ":" + escapePathSegments(trimmed)
	}
	u := c.endpoint.APIBase + "/repos/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Repo) +
		"/git/trees/" + treeish
	if recursive {
		u += "?recursive=1"
	}
	hdr := http.Header{"Accept": {"application/vnd.github+json"}}
	resp, err := c.doAPI(ctx, Request{URL: u, Header: hdr, MaxBytes: maxTreeResponseBytes}, s.RepoSlug(), cred)
	if err != nil {
		return Tree{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeTree(resp.Body, s.RepoSlug(), l)
}

// FetchFile downloads a single raw file at commitSHA into w, enforcing size
// limits mid-stream and verifying the git blob SHA ("blob <n>\0"+bytes) when
// f.BlobSHA is non-empty. Authorization is sent only for non-anonymous
// credentials under the Doer's hop-0 rules. A 429 or rate-limit response
// records into the shared cooldown for cred's scope.
//
// Because bytes are streamed directly to w as they arrive, w may hold partial
// or unverified content on any error; callers must discard it when FetchFile
// returns a non-nil error.
func (c *Client) FetchFile(ctx context.Context, s Source, commitSHA string, f File, cred Credential, w io.Writer) error {
	_, err := c.fetchFile(ctx, s, commitSHA, f, cred, w, 0)
	return err
}

var errRawStreamTooLarge = errors.New("stream exceeds byte limit")

type streamCapReader struct {
	r           io.Reader
	remaining   int64
	tooLargeErr error
}

func (c *streamCapReader) errLimit() error {
	if c.tooLargeErr != nil {
		return c.tooLargeErr
	}
	return errRawStreamTooLarge
}

func (c *streamCapReader) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return 0, c.errLimit()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.remaining {
		allowed := int(c.remaining)
		c.remaining = -1
		return allowed, c.errLimit()
	}
	c.remaining -= int64(n)
	return n, err
}

type writeErrTracker struct {
	w   io.Writer
	err error
}

func (t *writeErrTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

func (c *Client) fetchFile(ctx context.Context, s Source, commitSHA string, f File, cred Credential, w io.Writer, maxFileBytes int64) (int64, error) {
	if s.Owner == "" || s.Repo == "" {
		return 0, &Error{Kind: KindInvalidSource, Hint: "missing owner or repo"}
	}
	if commitSHA == "" {
		return 0, &Error{Kind: KindInvalidSource, Repo: s.RepoSlug(), Hint: "missing commit SHA"}
	}
	if !validRelativePath(f.Path) {
		return 0, &Error{Kind: KindUnsupportedContent, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "unsafe file path"}
	}
	if f.Size < 0 {
		return 0, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "invalid file size"}
	}
	if maxFileBytes > 0 && f.Size > maxFileBytes {
		return 0, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "file exceeds MaxFileBytes"}
	}

	expectExactSize := f.Size > 0 || f.BlobSHA != ""
	byteCap := f.Size
	if !expectExactSize {
		byteCap = maxFileBytes
		if byteCap <= 0 {
			byteCap = DefaultFetchFileMaxBytes
		}
	}
	reqMaxBytes := byteCap
	if reqMaxBytes <= 0 {
		// remotefetch treats MaxBytes == 0 as "fall back to fetcher default";
		// 1 byte caps Content-Length early while streamCapReader enforces 0.
		reqMaxBytes = 1
	}

	req := Request{
		URL:      c.endpoint.RawURL(s, commitSHA, f.Path),
		MaxBytes: reqMaxBytes,
	}
	if !cred.IsAnonymous() {
		req.Authorization = "Bearer " + cred.Token
	}

	// Raw fetches do not consume REST API quota so they do not pre-block on
	// REST cooldown, but a 429/rate-limit response still records into cooldown.
	resp, err := c.do(ctx, req, s.RepoSlug(), cred, false)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	capR := &streamCapReader{r: resp.Body, remaining: byteCap}
	wt := &writeErrTracker{w: w}
	var dst io.Writer = wt
	var h io.Writer
	var blobHash interface{ Sum([]byte) []byte }
	if f.BlobSHA != "" {
		sh := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
		_, _ = fmt.Fprintf(sh, "blob %d\x00", f.Size)
		h = sh
		blobHash = sh
		dst = io.MultiWriter(wt, h)
	}

	n, copyErr := io.Copy(dst, capR)
	if copyErr != nil {
		if wt.err != nil {
			return n, wt.err
		}
		if errors.Is(copyErr, errRawStreamTooLarge) {
			return n, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "raw file exceeds size limit"}
		}
		return n, ClassifyRemoteFetchError(copyErr, s.RepoSlug(), cred, c.clock())
	}

	if expectExactSize && n != f.Size {
		return n, &Error{Kind: KindUpstream, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "raw file size does not match git tree"}
	}
	if blobHash != nil {
		gotSHA := hex.EncodeToString(blobHash.Sum(nil))
		if !strings.EqualFold(gotSHA, f.BlobSHA) {
			return n, &Error{Kind: KindUpstream, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "raw file blob SHA mismatch"}
		}
	}
	return n, nil
}

// FetchFolder resolves s, lists and validates its git tree, and materializes
// its files into dst (*os.Root). Small, non-truncated folders (within
// RawMaxFiles and RawMaxBytes) are fetched per-file via raw.githubusercontent.com
// with bounded parallelism and git blob SHA verification; larger or truncated
// folders stream a single-pass tarball by commit SHA.
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

	return c.fetchFolderMaterialize(ctx, resolvedSrc, resolved.CommitSHA, &tree, cred, dst, l)
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
	return c.fetchFolderMaterialize(ctx, s, commitSHA, tree, cred, dst, l)
}

func (c *Client) fetchFolderMaterialize(ctx context.Context, s Source, commitSHA string, tree *Tree, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	if c.shouldUseRaw(tree, l) {
		return c.fetchFolderRaw(ctx, s, commitSHA, tree, cred, dst, l)
	}
	return c.fetchFolderTarball(ctx, s, commitSHA, tree, cred, dst, l)
}

func (c *Client) shouldUseRaw(tree *Tree, l Limits) bool {
	if tree == nil || tree.Truncated {
		return false
	}
	maxFiles := l.RawMaxFiles
	if maxFiles == 0 {
		maxFiles = c.rawMaxFiles
	}
	if maxFiles == 0 {
		maxFiles = DefaultRawMaxFiles
	}
	if maxFiles < 0 {
		return false
	}
	maxBytes := l.RawMaxBytes
	if maxBytes == 0 {
		maxBytes = c.rawMaxBytes
	}
	if maxBytes == 0 {
		maxBytes = DefaultRawMaxBytes
	}
	if maxBytes < 0 {
		return false
	}
	if len(tree.Files) > maxFiles {
		return false
	}
	var total int64
	for _, f := range tree.Files {
		if f.Size < 0 {
			return false
		}
		total += f.Size
		if total > maxBytes {
			return false
		}
	}
	return true
}

func (c *Client) fetchFolderRaw(ctx context.Context, s Source, commitSHA string, tree *Tree, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	if err := ctx.Err(); err != nil {
		return FolderResult{}, &Error{Kind: KindTimeout, Repo: s.RepoSlug(), CredKind: cred.Kind, err: err}
	}

	// Validate every tree entry, skip .git segments (matching decodeTree and
	// extractTarGz for caller-supplied Trees passed to FetchFolderResolved),
	// and pre-create parent directories before launching parallel workers so
	// directory creation never races.
	var plannedTotal int64
	createdDirs := make(map[string]struct{})
	files := make([]File, 0, len(tree.Files))
	for _, f := range tree.Files {
		if !validRelativePath(f.Path) {
			return FolderResult{}, &Error{Kind: KindUnsupportedContent, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "unsafe file path in git tree"}
		}
		if hasDotGitSegment(f.Path) {
			continue
		}
		if len(f.Path) > l.MaxNameBytes {
			return FolderResult{}, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "file path exceeds maximum name length"}
		}
		if f.Size < 0 || (l.MaxFileBytes > 0 && f.Size > l.MaxFileBytes) {
			return FolderResult{}, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "file exceeds maximum file size"}
		}
		plannedTotal += f.Size
		if l.MaxTotal > 0 && plannedTotal > l.MaxTotal {
			return FolderResult{}, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "directory exceeds maximum total size"}
		}
		files = append(files, f)
		if l.MaxFiles > 0 && len(files) > l.MaxFiles {
			return FolderResult{}, &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "directory exceeds maximum file count"}
		}
		if dir := path.Dir(f.Path); dir != "." && dir != "" {
			if _, ok := createdDirs[dir]; !ok {
				d, err := rootopen.MkdirAll(dst, dir, 0o755)
				if err != nil {
					return FolderResult{}, err
				}
				_ = d.Close()
				createdDirs[dir] = struct{}{}
			}
		}
	}

	if len(files) == 0 {
		return FolderResult{
			CommitSHA: commitSHA,
			TreeSHA:   tree.TreeSHA,
			Strategy:  StrategyRaw,
		}, nil
	}

	concurrency := c.rawConcurrency
	if concurrency <= 0 {
		concurrency = DefaultRawConcurrency
	}
	if concurrency > len(files) {
		concurrency = len(files)
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan File, len(files))
	for _, f := range files {
		jobs <- f
	}
	close(jobs)

	var (
		mu           sync.Mutex
		firstErr     error
		completed    int
		totalWritten int64
	)
	recordErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for f := range jobs {
				if err := fetchCtx.Err(); err != nil {
					recordErr(&Error{Kind: KindTimeout, Repo: s.RepoSlug(), CredKind: cred.Kind, err: err})
					return
				}
				mode := normalizeArchiveMode(int64(f.Mode))
				out, err := rootopen.OpenFileForWrite(dst, f.Path, uint32(mode), false)
				if err != nil {
					recordErr(fmt.Errorf("create raw file %q: %w", f.Path, err))
					return
				}
				// Apply the normalized mode independent of process umask.
				if err := out.Chmod(mode); err != nil { //winsec:not-protection normalize fetched file mode to 0644/0755
					_ = out.Close()
					_ = dst.Remove(f.Path)
					recordErr(err)
					return
				}

				fileCtx := fetchCtx
				var fileCancel context.CancelFunc
				if l.RequestTimeout > 0 {
					fileCtx, fileCancel = context.WithTimeout(fetchCtx, l.RequestTimeout)
				}
				n, fetchErr := c.fetchFile(fileCtx, s, commitSHA, f, cred, out, l.MaxFileBytes)
				closeErr := out.Close()
				if fileCancel != nil {
					fileCancel()
				}
				if fetchErr != nil {
					_ = dst.Remove(f.Path)
					recordErr(fetchErr)
					return
				}
				if closeErr != nil {
					_ = dst.Remove(f.Path)
					recordErr(closeErr)
					return
				}

				// Defence in depth: plannedTotal was already validated against
				// MaxTotal and each file is capped at f.Size, so this guards
				// against any future caller or File with Size==0 and BlobSHA=="".
				mu.Lock()
				totalWritten += n
				over := l.MaxTotal > 0 && totalWritten > l.MaxTotal
				if !over {
					completed++
				}
				mu.Unlock()
				if over {
					recordErr(&Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "fetched files exceed MaxTotal"})
					return
				}
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return FolderResult{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return FolderResult{}, &Error{Kind: KindTimeout, Repo: s.RepoSlug(), CredKind: cred.Kind, err: err}
	}
	if completed != len(files) {
		return FolderResult{}, &Error{Kind: KindTimeout, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "incomplete raw folder fetch"}
	}

	return FolderResult{
		CommitSHA: commitSHA,
		TreeSHA:   tree.TreeSHA,
		Files:     completed,
		Bytes:     totalWritten,
		Strategy:  StrategyRaw,
	}, nil
}

func (c *Client) fetchFolderTarball(ctx context.Context, s Source, commitSHA string, tree *Tree, cred Credential, dst *os.Root, l Limits) (FolderResult, error) {
	if l.MaxDownload <= 0 || l.MaxUnpacked <= 0 {
		return FolderResult{}, &Error{
			Kind:     KindTooLarge,
			Repo:     s.RepoSlug(),
			CredKind: cred.Kind,
			Hint:     "tarball fetch requires positive MaxDownload and MaxUnpacked limits",
		}
	}

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

	body := &streamCapReader{
		r:           resp.Body,
		remaining:   l.MaxDownload,
		tooLargeErr: &Error{Kind: KindTooLarge, Repo: s.RepoSlug(), CredKind: cred.Kind, Hint: "archive download exceeds MaxDownload"},
	}
	files, written, skipped, err := extractTarGz(body, s.RepoSlug(), s.Path, commitSHA, tree, dst, l)
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
		Strategy:  StrategyTarball,
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
