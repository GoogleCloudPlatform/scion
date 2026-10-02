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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/stretchr/testify/require"
)

// fakeGCSJSONServer is a minimal subset of the GCS JSON API: an object
// metadata GET and a media (alt=media) GET, with ifGenerationMatch
// precondition support. It records every request's Authorization header and
// query string so the test can assert on them: this test runs the
// production source against an httptest.Server that speaks a minimal GCS
// JSON API subset, asserting the Authorization: Bearer <minted> header,
// generation pinning, 404/403 mapping and ReadCompressed.
type fakeGCSJSONServer struct {
	mu       sync.Mutex
	requests []*http.Request

	name            string
	size            int64
	generation      int64
	contentType     string
	contentEncoding string
	body            []byte

	// denyMetadata/denyMedia, when non-zero, make the corresponding call
	// respond with that HTTP status instead of succeeding.
	denyMetadataStatus int
	denyMediaStatus    int

	// metadataBlock/mediaBlock, when non-nil, make the corresponding call
	// wait to respond until the channel is closed, simulating a hung
	// upstream GCS call.
	metadataBlock chan struct{}
	mediaBlock    chan struct{}
}

func (f *fakeGCSJSONServer) lastAuthHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	return f.requests[len(f.requests)-1].Header.Get("Authorization")
}

// ServeHTTP routes two request shapes, matching the real storage client's
// own split: Attrs always uses the JSON metadata API
// (.../b/{bucket}/o/{object}, a single percent-encoded object segment), and
// NewReader — since production code never sets storage.WithJSONReads — uses
// the plain "XML" media GET (/{bucket}/{object}, literal path segments, no
// query string), authenticated by the same underlying http.Client either
// way. The precondition on the media path arrives as the
// x-goog-if-generation-match header, not a query parameter.
func (f *fakeGCSJSONServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.mu.Unlock()

	writeGoogleAPIError := func(status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"denied"}}`, status)
	}

	if idx := strings.Index(r.URL.Path, "/b/"); idx != -1 {
		rest := r.URL.Path[idx+len("/b/"):]
		if parts := strings.SplitN(rest, "/o/", 2); len(parts) == 2 {
			// Metadata GET (alt=json).
			if f.metadataBlock != nil {
				<-f.metadataBlock
			}
			if f.denyMetadataStatus != 0 {
				writeGoogleAPIError(f.denyMetadataStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"name":%q,"size":"%d","generation":"%d","contentType":%q,"contentEncoding":%q}`,
				f.name, f.size, f.generation, f.contentType, f.contentEncoding)
			return
		}
	}

	// Media GET (XML-style): /{bucket}/{object}.
	if f.mediaBlock != nil {
		<-f.mediaBlock
	}
	if f.denyMediaStatus != 0 {
		writeGoogleAPIError(f.denyMediaStatus)
		return
	}
	if ig := r.Header.Get("x-goog-if-generation-match"); ig != "" {
		want, _ := strconv.ParseInt(ig, 10, 64)
		if want != f.generation {
			writeGoogleAPIError(http.StatusPreconditionFailed)
			return
		}
	}
	// A real Content-Encoding header is what makes ReadCompressed(true) mean
	// anything to Go's http.Transport: without it, there is nothing to
	// transparently decompress either way, and the flag makes no observable
	// difference (see TestGCSLink_RealClient_ReadCompressedServesStoredBytes).
	if f.contentEncoding != "" {
		w.Header().Set("Content-Encoding", f.contentEncoding)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.body)
}

// newRealGCSClientSource builds a gcsClientSource backed by a real
// *storage.Client pointed at ts, authenticated with a static bearer token
// (no IAM impersonation — the seam under test here is gcsClientSource's use
// of the storage API, not token minting, which gcs_link_test.go's fakes
// already cover). No storage.WithJSONReads(): production code doesn't set
// it either, and reads go through ReadCompressed(true), which is
// incompatible with the JSON-reads code path in this client version (it
// sets Accept-Encoding, which the generated JSON transport rejects
// outright) — the default "XML" media path both matches production and has
// no such restriction.
func newRealGCSClientSource(t *testing.T, ts *httptest.Server, token string) *gcsClientSource {
	t.Helper()
	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(ts.URL),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return &gcsClientSource{client: client}
}

func TestGCSLink_RealClient_AttrsAndOpen(t *testing.T) {
	const wantToken = "Bearer test-minted-access-token"
	fake := &fakeGCSJSONServer{
		name: "o.txt", size: 5, generation: 9, contentType: "text/html", body: []byte("hello"),
	}
	ts := httptest.NewServer(fake)
	defer ts.Close()

	source := newRealGCSClientSource(t, ts, "test-minted-access-token")

	attrs, err := source.Attrs(context.Background(), "bkt", "o.txt")
	require.NoError(t, err)
	require.Equal(t, int64(5), attrs.Size)
	require.Equal(t, int64(9), attrs.Generation)
	require.Equal(t, wantToken, fake.lastAuthHeader())

	reader, err := source.Open(context.Background(), "bkt", "o.txt", attrs.Generation)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "hello", string(body))
	require.Equal(t, wantToken, fake.lastAuthHeader())
}

// TestGCSLink_RealSource_PerRequestTokenNoCrossSAReuse drives the real
// production factory (gcsObjectSourceFor, not a hand-built client) against a
// fake GCS JSON server, proving two back-to-back requests for different SAs
// each carry exactly the token minted for that request's own SA on their
// GCS calls — never the other SA's — and that each request mints its own
// token once, with no cross-request reuse (there is no per-SA cache, so
// there is nothing to reuse from). It also pins the production mint scope:
// gcsObjectSourceFor itself is checked here, not just the test fake that
// backs the handler-level tests.
func TestGCSLink_RealSource_PerRequestTokenNoCrossSAReuse(t *testing.T) {
	wantScopes := []string{"https://www.googleapis.com/auth/devstorage.read_only"}

	fake := &fakeGCSJSONServer{name: "o.txt", size: 5, generation: 9, body: []byte("hello")}
	ts := httptest.NewServer(fake)
	defer ts.Close()

	gen := &fakeGCSTokenGenerator{}
	srv := &Server{
		gcpTokenGenerator:       gen,
		gcsLinkBaseTransport:    newGCSLinkBaseTransport(),
		gcsLinkEndpointOverride: ts.URL,
	}

	sourceA, releaseA, err := srv.gcsObjectSourceFor(context.Background(), "sa-a@test.iam.gserviceaccount.com")
	require.NoError(t, err)
	_, err = sourceA.Attrs(context.Background(), "bkt", "o.txt")
	require.NoError(t, err)
	require.Equal(t, "Bearer fake-token-for-sa-a@test.iam.gserviceaccount.com", fake.lastAuthHeader())
	releaseA()

	callA, ok := gen.lastCall()
	require.True(t, ok, "expected a mint call for sa-a")
	require.Equal(t, wantScopes, callA.scopes, "the production mint must request exactly the read-only scope, never a wider one")

	sourceB, releaseB, err := srv.gcsObjectSourceFor(context.Background(), "sa-b@test.iam.gserviceaccount.com")
	require.NoError(t, err)
	_, err = sourceB.Attrs(context.Background(), "bkt", "o.txt")
	require.NoError(t, err)
	require.Equal(t, "Bearer fake-token-for-sa-b@test.iam.gserviceaccount.com", fake.lastAuthHeader(),
		"sa-b's request must carry its own token, not sa-a's")
	releaseB()

	callB, ok := gen.lastCall()
	require.True(t, ok, "expected a mint call for sa-b")
	require.Equal(t, wantScopes, callB.scopes, "the production mint must request exactly the read-only scope, never a wider one")

	require.Equal(t, 2, gen.mintCount(), "one mint per request: nothing is cached or reused across requests")
}

// TestGCSLink_RealSource_NoADCDetection lives in
// gcs_link_realclient_unix_test.go, under a unix build tag: it uses
// syscall.Mkfifo, which has no Windows equivalent.

func TestGCSLink_RealClient_GenerationMismatchIsPreconditionFailed(t *testing.T) {
	fake := &fakeGCSJSONServer{name: "o.txt", size: 5, generation: 9, body: []byte("hello")}
	ts := httptest.NewServer(fake)
	defer ts.Close()

	source := newRealGCSClientSource(t, ts, "tok")

	// Pin to a generation the fake server does not currently have.
	_, err := source.Open(context.Background(), "bkt", "o.txt", 1)
	require.Error(t, err)
	require.Equal(t, GCSLinkReasonUpstreamError, classifyGCSError(err))
}

func TestGCSLink_RealClient_404And403Mapping(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		fake := &fakeGCSJSONServer{denyMetadataStatus: http.StatusNotFound}
		ts := httptest.NewServer(fake)
		defer ts.Close()
		source := newRealGCSClientSource(t, ts, "tok")

		_, err := source.Attrs(context.Background(), "bkt", "missing.txt")
		require.Error(t, err)
		require.Equal(t, GCSLinkReasonGCSNotFound, classifyGCSError(err))
	})

	t.Run("403", func(t *testing.T) {
		fake := &fakeGCSJSONServer{denyMetadataStatus: http.StatusForbidden}
		ts := httptest.NewServer(fake)
		defer ts.Close()
		source := newRealGCSClientSource(t, ts, "tok")

		_, err := source.Attrs(context.Background(), "bkt", "denied.txt")
		require.Error(t, err)
		require.Equal(t, GCSLinkReasonGCSDenied, classifyGCSError(err))
	})
}

func TestGCSLink_RealClient_ReadCompressedServesStoredBytes(t *testing.T) {
	// A gzip-encoded object's stored bytes (the compressed form) must come
	// back unchanged: ReadCompressed(true) disables Go's http.Transport's
	// own transparent gzip decompression. The fake server actually
	// gzip-compresses the plaintext and sets a real Content-Encoding: gzip
	// header on the media response — without that, there is nothing for
	// Go's transport to transparently decompress either way, and mutating
	// ReadCompressed(true) to false would not be caught by this test.
	plaintext := []byte("hello, this is the object's real decompressed content")
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(plaintext)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	compressedBytes := compressed.Bytes()

	fake := &fakeGCSJSONServer{
		name: "o.gz", size: int64(len(compressedBytes)), generation: 1,
		contentEncoding: "gzip", body: compressedBytes,
	}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	source := newRealGCSClientSource(t, ts, "tok")

	attrs, err := source.Attrs(context.Background(), "bkt", "o.gz")
	require.NoError(t, err)
	require.Equal(t, "gzip", attrs.ContentEncoding)

	reader, err := source.Open(context.Background(), "bkt", "o.gz", attrs.Generation)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, compressedBytes, body, "ReadCompressed(true) must serve the stored (compressed) bytes unchanged, never transparently decompressed")
	require.NotEqual(t, plaintext, body, "sanity: the compressed and plaintext forms must actually differ, or this test cannot distinguish them")
}

// TestGCSLink_RealClient_AttrsRespectsContextDeadline proves the real
// storage client's Attrs call is bounded by the ctx the caller passes, not
// by anything open-ended: the fake server never responds to the metadata
// request on its own, so the call can only return via ctx expiring.
func TestGCSLink_RealClient_AttrsRespectsContextDeadline(t *testing.T) {
	before := goroutineBaseline()
	fake := &fakeGCSJSONServer{metadataBlock: make(chan struct{})}
	ts := httptest.NewServer(fake)

	source := newRealGCSClientSource(t, ts, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := source.Attrs(ctx, "bkt", "o.txt")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, 2*time.Second, "Attrs must be bounded by ctx, not by the upstream call")

	close(fake.metadataBlock) // unblocks the server handler so its goroutine can exit before the leak check
	// The client's transport keeps an idle connection's read goroutine alive
	// until the client is closed, and ts's own accept-loop goroutine lives
	// until ts.Close() — both closed here explicitly, not deferred, because
	// a deferred Close only runs after the test function returns, which is
	// too late for the check below.
	_ = source.client.Close()
	ts.Close()
	requireNoGoroutineLeak(t, before)
}

// TestGCSLink_RealClient_ReadRespectsContextDeadline proves the reader
// returned by Open is also bounded by the same ctx for the actual data
// transfer, not just for the initial request: the metadata call succeeds
// normally, but the fake server never sends the media response body, so a
// Read on the returned reader can only return via ctx expiring.
func TestGCSLink_RealClient_ReadRespectsContextDeadline(t *testing.T) {
	before := goroutineBaseline()
	fake := &fakeGCSJSONServer{
		name: "o.txt", size: 5, generation: 9,
		mediaBlock: make(chan struct{}),
	}
	ts := httptest.NewServer(fake)

	source := newRealGCSClientSource(t, ts, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	attrs, err := source.Attrs(ctx, "bkt", "o.txt")
	require.NoError(t, err, "the metadata call itself is not blocked")

	start := time.Now()
	_, err = source.Open(ctx, "bkt", "o.txt", attrs.Generation)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, 2*time.Second, "Open/the reader must be bounded by ctx, not by the upstream call")

	close(fake.mediaBlock) // unblocks the server handler so its goroutine can exit before the leak check
	// See the matching comment in AttrsRespectsContextDeadline: closing the
	// client and ts here, rather than deferring them, matters for the check
	// below.
	_ = source.client.Close()
	ts.Close()
	requireNoGoroutineLeak(t, before)
}

// TestGCSLink_RealClient_ReadMidStreamRespectsContextDeadline covers the
// case TestGCSLink_RealClient_ReadRespectsContextDeadline does not: the
// server sends headers and part of the body immediately (so Open/NewReader
// itself succeeds), then hangs without finishing — proving a Read call
// already in progress is also bounded by ctx, not just the initial
// connection.
func TestGCSLink_RealClient_ReadMidStreamRespectsContextDeadline(t *testing.T) {
	before := goroutineBaseline()
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ab"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))

	source := newRealGCSClientSource(t, ts, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	reader, err := source.Open(ctx, "bkt", "o.txt", 9)
	require.NoError(t, err, "headers and the first bytes arrive immediately; only the rest of the body hangs")

	start := time.Now()
	_, err = io.ReadAll(reader)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, 2*time.Second, "a Read already in progress must be bounded by ctx too, not just the initial connection")

	close(block) // unblocks the server handler so its goroutine can exit before the leak check
	// See the matching comment in AttrsRespectsContextDeadline: closing the
	// reader, client and ts here, rather than deferring them, matters for
	// the check below.
	_ = reader.Close()
	_ = source.client.Close()
	ts.Close()
	requireNoGoroutineLeak(t, before)
}
