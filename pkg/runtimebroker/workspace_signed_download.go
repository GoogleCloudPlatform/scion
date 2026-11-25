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

package runtimebroker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// The signed-URL workspace download (WorkspaceDownloadModeSignedURLs).
//
// The broker pages through the Hub's broker-self workspace download
// endpoint, which lists the agent's uploaded files with the Hub's own
// storage credential and answers each page with one GET-only signed URL per
// file. The broker then GETs each URL straight from the object store. It
// needs no storage credential and never lists the bucket.
//
// A signed URL is a bearer capability. Nothing here logs, stores or returns
// one: every failure is mapped to a signedDownloadError, which carries only
// the page and file index, an HTTP status and a fixed class. Errors from
// net/http (a *url.Error quotes the full URL, query included) and response
// bodies never reach error text.

// Signed download error classes are defined in signed_transport.go.

// signedDownloadError is the only error the signed-URL workspace download
// returns. It holds no URL, path, bucket or response text.
type signedDownloadError struct {
	// Stage is "page" for a page request to the Hub, "file" for an object
	// GET or write.
	Stage string
	// Page is the 1-based page number.
	Page int
	// File is the 0-based index of the file in its page, -1 for a page
	// request.
	File int
	// Status is the HTTP status, 0 when there was none.
	Status int
	// Class is one of the signedClass* constants, "http_<status>" for an
	// object GET status or "endpoint_<status>" for a Hub status.
	Class string
}

func (e *signedDownloadError) Error() string {
	if e.Stage == "page" {
		return fmt.Sprintf("signed workspace download: page %d request: %s", e.Page, e.Class)
	}
	return fmt.Sprintf("signed workspace download: page %d file %d: %s", e.Page, e.File, e.Class)
}

// signedDownloadSettings are the signed-URL download's object client, page
// source and timing. A zero value field takes its default.
type signedDownloadSettings struct {
	// client GETs the signed URLs. It must stay a plain client: an
	// instrumented transport (otelhttp or httptrace) would record the full
	// URL, signature included, as a span attribute.
	client *http.Client
	// fetchPage replaces the Hub page request (tests).
	fetchPage func(ctx context.Context, req CreateAgentRequest, cursor string) (*api.WorkspaceDownloadResponse, error)
	// sleep waits between retries; it returns ctx.Err() when ctx ends first.
	sleep func(ctx context.Context, d time.Duration) error
	// now is the clock for page expiry.
	now func() time.Time
	// pageBackoff are the waits before each page request retry.
	pageBackoff []time.Duration
	// fileBackoff are the waits before each object GET retry.
	fileBackoff []time.Duration
	// concurrency is the number of concurrent object GETs.
	concurrency int
	// stallTimeout ends an object GET whose body delivers no bytes for this
	// long (class timeout).
	stallTimeout time.Duration
}

const (
	// signedDownloadConcurrency is the default number of concurrent GETs.
	signedDownloadConcurrency = 4
)

// resolved returns the settings with defaults filled in.
func (st signedDownloadSettings) resolved() signedDownloadSettings {
	if st.client == nil {
		st.client = defaultSignedDownloadClient
	}
	if st.sleep == nil {
		st.sleep = sleepCtx
	}
	if st.now == nil {
		st.now = time.Now
	}
	if st.pageBackoff == nil {
		st.pageBackoff = defaultSignedPageBackoff
	}
	if st.fileBackoff == nil {
		st.fileBackoff = defaultSignedFileBackoff
	}
	if st.concurrency <= 0 {
		st.concurrency = signedDownloadConcurrency
	}
	if st.stallTimeout <= 0 {
		st.stallTimeout = signedDownloadStallTimeout
	}
	return st
}

// signedDownloadResult is what a completed signed download wrote.
type signedDownloadResult struct {
	// written are the relative paths written, in page order.
	written []string
	// bytes is the total size of the files written.
	bytes int64
}

// signedWorkspaceDownload downloads the agent's uploaded workspace files
// into the staging directory dir through the Hub's signed URLs and returns
// the relative paths it wrote and their total size. It never reads the
// bucket with this broker's credentials.
func (s *Server) signedWorkspaceDownload(ctx context.Context, req CreateAgentRequest, dir string) (signedDownloadResult, error) {
	st := s.signedDownload.resolved()
	if st.fetchPage == nil {
		st.fetchPage = s.fetchSignedWorkspacePage
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return signedDownloadResult{}, &signedDownloadError{Stage: "file", Page: 0, File: -1, Class: signedClassWrite}
	}
	defer func() { _ = root.Close() }()
	d := &signedDownloader{st: st, req: req, root: root}
	if err := d.run(ctx); err != nil {
		return signedDownloadResult{}, err
	}
	return signedDownloadResult{written: d.written, bytes: d.bytes}, nil
}

// msgSignedBootstrapDone is the message of the one Info line a completed
// signed-URL workspace bootstrap logs.
const msgSignedBootstrapDone = "Workspace bootstrapped through signed URLs"

// logSignedBootstrapDone logs a completed signed-URL workspace bootstrap at
// Info: the agent, the number of files and bytes written, and the time since
// start, which is taken under the workspace lock, so it covers staging,
// download and commit but not the lock wait. It never names a URL or a path.
func (s *Server) logSignedBootstrapDone(req CreateAgentRequest, res signedDownloadResult, start time.Time) {
	s.agentLifecycleLog.Info(msgSignedBootstrapDone,
		"agent_id", req.ID, "name", req.Name, "project_id", req.ProjectID,
		"files", len(res.written), "bytes", res.bytes, "elapsed", time.Since(start).String())
}

// fetchSignedWorkspacePage requests one page from the Hub connection the
// create arrived on, with this broker's HMAC client.
func (s *Server) fetchSignedWorkspacePage(ctx context.Context, req CreateAgentRequest, cursor string) (*api.WorkspaceDownloadResponse, error) {
	s.hubMu.RLock()
	conn := s.hubConnections[req.workspaceHubConn]
	s.hubMu.RUnlock()
	if req.workspaceHubConn == "" || conn == nil || conn.HubClient == nil {
		return nil, errNoSignedDownloadHub
	}
	return conn.HubClient.RuntimeBrokers().WorkspaceDownload(ctx, conn.BrokerID, req.ID, &api.WorkspaceDownloadRequest{Cursor: cursor})
}

// errNoSignedDownloadHub: the create's Hub connection is not known or has
// no client, so there is no Hub to ask for URLs. The download fails closed.
var errNoSignedDownloadHub = errors.New("no hub connection for the signed workspace download")

// signedDownloader runs one signed-URL download into root.
type signedDownloader struct {
	st   signedDownloadSettings
	req  CreateAgentRequest
	root *os.Root
	// written are the relative paths written, in page order.
	written []string
	// bytes is the total listed size of written; a file is only completed
	// when its length matches its listed size.
	bytes int64
}

func (d *signedDownloader) run(ctx context.Context) error {
	cursor := ""
	seen := map[string]bool{}
	for page := 1; ; page++ {
		resp, err := d.fetchPage(ctx, page, cursor)
		if err != nil {
			return err
		}
		if err := d.downloadPage(ctx, page, cursor, resp, seen); err != nil {
			return err
		}
		if resp.NextCursor == "" {
			return nil
		}
		if resp.NextCursor <= cursor {
			// The cursor must move forward; a Hub that repeats or rewinds it
			// would loop.
			return &signedDownloadError{Stage: "page", Page: page, File: -1, Class: signedClassListingChanged}
		}
		cursor = resp.NextCursor
	}
}

// fetchPage requests a page, retrying a transport error or a 5xx after each
// pageBackoff wait, then failing closed. Any other Hub answer fails at once.
func (d *signedDownloader) fetchPage(ctx context.Context, page int, cursor string) (*api.WorkspaceDownloadResponse, error) {
	for attempt := 0; ; attempt++ {
		resp, err := d.st.fetchPage(ctx, d.req, cursor)
		if err == nil {
			if resp == nil {
				return nil, &signedDownloadError{Stage: "page", Page: page, File: -1, Class: signedClassDecode}
			}
			return resp, nil
		}
		mapped, retryable := mapPageError(page, err)
		if !retryable || ctx.Err() != nil || attempt >= len(d.st.pageBackoff) {
			if ctx.Err() != nil {
				return nil, &signedDownloadError{Stage: "page", Page: page, File: -1, Class: contextClass(ctx.Err())}
			}
			return nil, mapped
		}
		if serr := d.st.sleep(ctx, d.st.pageBackoff[attempt]); serr != nil {
			return nil, &signedDownloadError{Stage: "page", Page: page, File: -1, Class: contextClass(serr)}
		}
	}
}

// mapPageError maps a page request error to a signedDownloadError and
// reports whether it may be retried (a transport error or a 5xx).
func mapPageError(page int, err error) (*signedDownloadError, bool) {
	out := &signedDownloadError{Stage: "page", Page: page, File: -1}
	var apiErr *apiclient.APIError
	switch {
	case errors.Is(err, errNoSignedDownloadHub):
		out.Class = signedClassNoHub
		return out, false
	case errors.As(err, &apiErr):
		out.Status = apiErr.StatusCode
		out.Class = "endpoint_" + strconv.Itoa(apiErr.StatusCode)
		return out, apiErr.StatusCode >= 500
	}
	out.Class = transportClass(err)
	if out.Class == signedClassDecode {
		return out, false
	}
	return out, out.Class != signedClassCanceled
}

// signedFileState is one file's outcome in a download round.
type signedFileState int

const (
	signedFileDone signedFileState = iota
	signedFileRefresh
	signedFileFailed
)

// downloadPage downloads every file of one page, refreshing the page when
// its URLs expire or are refused. A refreshed page must list exactly the
// same files with the same sizes; files already written are not fetched
// again. At most signedDownloadMaxIdleRefreshes consecutive refreshes may
// pass with no file completed since the previous refresh.
func (d *signedDownloader) downloadPage(ctx context.Context, page int, cursor string, resp *api.WorkspaceDownloadResponse, seen map[string]bool) error {
	listing := make(map[string]int64, len(resp.Files))
	for i, f := range resp.Files {
		if transfer.ExcludeWorkspaceObject(f.Path) != transfer.WorkspaceObjectKept || f.Size < 0 || f.URL == "" || seen[f.Path] {
			return &signedDownloadError{Stage: "file", Page: page, File: i, Class: signedClassInvalidEntry}
		}
		if _, dup := listing[f.Path]; dup {
			return &signedDownloadError{Stage: "file", Page: page, File: i, Class: signedClassInvalidEntry}
		}
		listing[f.Path] = f.Size
	}
	for _, f := range resp.Files {
		seen[f.Path] = true
		d.written = append(d.written, f.Path)
		d.bytes += f.Size
	}

	done := make(map[string]bool, len(resp.Files))
	idleRefreshes := 0
	completedSinceRefresh := 0
	deadline := pageDeadline(resp, d.st.now())
	for {
		needRefresh := deadline.Sub(d.st.now()) < signedDownloadRefreshMargin
		if !needRefresh {
			completed, refresh, err := d.round(ctx, page, resp, done)
			if err != nil {
				return err
			}
			completedSinceRefresh += completed
			if !refresh {
				return nil
			}
		}
		if completedSinceRefresh == 0 {
			idleRefreshes++
		} else {
			idleRefreshes = 0
		}
		if idleRefreshes > signedDownloadMaxIdleRefreshes {
			return &signedDownloadError{Stage: "page", Page: page, File: -1, Class: signedClassExpired}
		}
		completedSinceRefresh = 0
		fresh, err := d.fetchPage(ctx, page, cursor)
		if err != nil {
			return err
		}
		if !sameListing(listing, fresh) {
			return &signedDownloadError{Stage: "page", Page: page, File: -1, Class: signedClassListingChanged}
		}
		resp = fresh
		deadline = pageDeadline(resp, d.st.now())
	}
}

// pageDeadline is when a page's URLs expire, on this broker's clock: the
// page's relative lifetime measured from its receipt, so clock skew between
// the Hub and the broker does not shorten it. A page without a relative
// lifetime (an older Hub) falls back to its absolute expiry.
func pageDeadline(resp *api.WorkspaceDownloadResponse, received time.Time) time.Time {
	if resp.ExpiresInSeconds > 0 {
		return received.Add(time.Duration(resp.ExpiresInSeconds) * time.Second)
	}
	return resp.Expires
}

// sameListing reports whether a refreshed page lists exactly the paths and
// sizes of the first fetch of its cursor.
func sameListing(listing map[string]int64, fresh *api.WorkspaceDownloadResponse) bool {
	if len(fresh.Files) != len(listing) {
		return false
	}
	for _, f := range fresh.Files {
		size, ok := listing[f.Path]
		if !ok || size != f.Size || f.URL == "" {
			return false
		}
	}
	return true
}

// round GETs every file of resp not yet done, with st.concurrency workers.
// It returns how many files it completed and whether some need a page
// refresh. The first fatal error cancels the round and is returned.
func (d *signedDownloader) round(ctx context.Context, page int, resp *api.WorkspaceDownloadResponse, done map[string]bool) (int, bool, error) {
	roundCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pending := make([]int, 0, len(resp.Files))
	for i, f := range resp.Files {
		if !done[f.Path] {
			pending = append(pending, i)
		}
	}

	type result struct {
		path  string
		state signedFileState
		err   error
	}
	work := make(chan int)
	results := make(chan result)
	var wg sync.WaitGroup
	for w := 0; w < d.st.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				f := resp.Files[i]
				state, err := d.getFile(roundCtx, page, i, f)
				results <- result{path: f.Path, state: state, err: err}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, i := range pending {
			select {
			case work <- i:
			case <-roundCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	completed := 0
	refresh := false
	var firstErr error
	for r := range results {
		switch r.state {
		case signedFileDone:
			done[r.path] = true
			completed++
		case signedFileRefresh:
			refresh = true
		case signedFileFailed:
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
		}
	}
	if firstErr != nil {
		return completed, false, firstErr
	}
	if err := ctx.Err(); err != nil {
		return completed, false, &signedDownloadError{Stage: "page", Page: page, File: -1, Class: contextClass(err)}
	}
	return completed, refresh, nil
}

// getFile downloads one file, retrying a transport error or a 5xx after
// each fileBackoff wait. A 400, 401 or 403 asks for a page refresh (the URL
// expired or was refused). A 404 fails closed: the listing is stale.
func (d *signedDownloader) getFile(ctx context.Context, page, index int, f api.WorkspaceDownloadFile) (signedFileState, error) {
	fail := func(status int, class string) (signedFileState, error) {
		return signedFileFailed, &signedDownloadError{Stage: "file", Page: page, File: index, Status: status, Class: class}
	}
	for attempt := 0; ; attempt++ {
		status, class, retryable := d.getOnce(ctx, f)
		switch {
		case class == "":
			return signedFileDone, nil
		case status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden:
			return signedFileRefresh, nil
		case !retryable || ctx.Err() != nil || attempt >= len(d.st.fileBackoff):
			if err := ctx.Err(); err != nil {
				return fail(0, contextClass(err))
			}
			return fail(status, class)
		}
		if err := d.st.sleep(ctx, d.st.fileBackoff[attempt]); err != nil {
			return fail(0, contextClass(err))
		}
	}
}

// getOnce makes one GET of f and writes it. It returns the HTTP status (0
// without a response), an error class ("" on success) and whether the
// failure may be retried.
func (d *signedDownloader) getOnce(ctx context.Context, f api.WorkspaceDownloadFile) (int, string, bool) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, f.URL, nil)
	if err != nil {
		return 0, signedClassInvalidEntry, false
	}
	resp, err := d.st.client.Do(httpReq)
	if err != nil {
		class := transportClass(err)
		return 0, class, class != signedClassCanceled
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is drained a little, for connection reuse, and never read
		// into any text.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, "http_" + strconv.Itoa(resp.StatusCode), resp.StatusCode >= 500
	}
	body := newStallReader(resp.Body, d.st.stallTimeout, cancel)
	defer body.stop()
	class := d.writeFile(f, body)
	if class != "" && body.stalled() {
		class = signedClassTimeout
	}
	if class != "" {
		return resp.StatusCode, class, class == signedClassReset || class == signedClassTimeout || class == signedClassTransport
	}
	return resp.StatusCode, "", false
}

// writeFile streams body into a temporary file next to f.Path inside the
// root, checks its length against f.Size, and renames it into place. It
// returns an error class, "" on success.
func (d *signedDownloader) writeFile(f api.WorkspaceDownloadFile, body io.Reader) string {
	// The write side checks the name again, so no path can reach the root
	// identity entry in any letter case, whatever the page validation did.
	if transfer.ExcludeWorkspaceObject(f.Path) != transfer.WorkspaceObjectKept {
		return signedClassInvalidEntry
	}
	name := filepath.FromSlash(f.Path)
	if dir := filepath.Dir(name); dir != "." {
		// The modes match the rclone download's (0777 directories, 0666
		// files): the umask or the directory's default ACL then applies
		// exactly as it does there.
		if err := d.root.MkdirAll(dir, 0o777); err != nil {
			return signedClassWrite
		}
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return signedClassWrite
	}
	tmp := name + ".scion-dl-" + hex.EncodeToString(suffix[:])
	out, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return signedClassWrite
	}
	n, copyErr := io.Copy(out, io.LimitReader(body, f.Size+1))
	closeErr := out.Close()
	class := ""
	switch {
	case copyErr != nil:
		class = transportClass(copyErr)
	case closeErr != nil:
		class = signedClassWrite
	case n != f.Size:
		class = signedClassSizeMismatch
	}
	if class == "" {
		if err := d.root.Rename(tmp, name); err != nil {
			class = signedClassWrite
		}
	}
	if class != "" {
		_ = d.root.Remove(tmp)
	}
	return class
}
