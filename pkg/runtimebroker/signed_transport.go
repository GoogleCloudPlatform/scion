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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Shared signed-URL object-store transport helpers reused by both
// workspace_signed_download.go and move_bundle.go.
//
// Signed URLs are bearer capabilities. Nothing in this file logs, stores or
// returns a URL: every transport or HTTP error is reduced to a fixed class
// string and HTTP status code, never a *url.Error or response body.

// Signed transfer error classes.
const (
	signedClassTimeout        = "timeout"
	signedClassDial           = "dial"
	signedClassTLS            = "tls"
	signedClassReset          = "reset"
	signedClassCanceled       = "canceled"
	signedClassTransport      = "transport"
	signedClassExpired        = "expired"
	signedClassSizeMismatch   = "size_mismatch"
	signedClassListingChanged = "listing_changed"
	signedClassInvalidEntry   = "invalid_entry"
	signedClassWrite          = "write"
	signedClassNoHub          = "no_hub_connection"
	signedClassDecode         = "decode"
)

const (
	// signedDownloadRefreshMargin: a URL set expiring sooner than this is
	// refreshed before starting the next part/file transfer.
	signedDownloadRefreshMargin = 60 * time.Second
	// signedDownloadMaxIdleRefreshes bounds consecutive refreshes without
	// completing any part/file.
	signedDownloadMaxIdleRefreshes = 2
	// signedDownloadStallTimeout ends an object transfer whose body makes no
	// progress for this duration.
	signedDownloadStallTimeout = 2 * time.Minute
	// signedDownloadHeaderTimeout bounds the wait for response headers.
	signedDownloadHeaderTimeout = time.Minute
)

var (
	// defaultSignedPageBackoff: hub signed-URL requests are retried up to 3
	// times on a transport error or 5xx.
	defaultSignedPageBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	// defaultSignedFileBackoff: object PUT/GET calls are tried up to 3 times
	// (initial attempt + 2 retries) on a transport error or 5xx.
	defaultSignedFileBackoff = []time.Duration{time.Second, 2 * time.Second}
	// defaultSignedDownloadClient is a plain client with no instrumentation
	// (otelhttp/httptrace would record the signed URL) and a bounded header
	// timeout.
	defaultSignedDownloadClient = newSignedDownloadClient()
)

// newSignedDownloadClient returns the plain object client: the default
// transport with a response header timeout, and no instrumentation.
func newSignedDownloadClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = signedDownloadHeaderTimeout
	return &http.Client{Transport: tr}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stallReader cancels a transfer whose body delivers no bytes for the stall
// timeout. Every read that returns bytes restarts the timer.
type stallReader struct {
	r       io.Reader
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
}

func newStallReader(r io.Reader, timeout time.Duration, cancel context.CancelFunc) *stallReader {
	s := &stallReader{r: r, timeout: timeout}
	s.timer = time.AfterFunc(timeout, func() {
		s.fired.Store(true)
		cancel()
	})
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.timeout)
	}
	return n, err
}

func (s *stallReader) stop()         { s.timer.Stop() }
func (s *stallReader) stalled() bool { return s.fired.Load() }

// contextClass is the class of a context error.
func contextClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return signedClassTimeout
	}
	return signedClassCanceled
}

// transportClass classifies a transport or body read error without keeping
// any of its text: a *url.Error quotes the full request URL.
func transportClass(err error) string {
	var (
		netErr     net.Error
		opErr      *net.OpError
		recErr     tls.RecordHeaderError
		certErr    *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, hubclient.ErrWorkspaceDownloadDecode),
		errors.Is(err, hubclient.ErrMoveBundleURLDecode):
		return signedClassDecode
	case errors.Is(err, context.Canceled):
		return signedClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return signedClassTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		return signedClassTimeout
	case errors.As(err, &recErr), errors.As(err, &certErr), errors.As(err, &unknownCA),
		errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return signedClassTLS
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return signedClassReset
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return signedClassDial
	case errors.Is(err, syscall.ECONNREFUSED):
		return signedClassDial
	}
	return signedClassTransport
}

// signedBundleTransferError is the redacted error type for move-bundle
// signed-URL operations (hub URL requests, part PUTs, part GETs). It never
// contains a URL, query parameter, bucket name, or response body text.
type signedBundleTransferError struct {
	Stage     string // "upload_url" | "download_urls" | "put_part" | "get_part"
	Root      string
	PartIndex int
	Status    int
	Class     string
}

func (e *signedBundleTransferError) Error() string {
	if e.PartIndex >= 0 && e.Root != "" {
		return fmt.Sprintf("move bundle %s (%s part %d): %s", e.Stage, e.Root, e.PartIndex, e.Class)
	}
	if e.Root != "" {
		return fmt.Sprintf("move bundle %s (%s): %s", e.Stage, e.Root, e.Class)
	}
	return fmt.Sprintf("move bundle %s: %s", e.Stage, e.Class)
}

// signedPutPart uploads a seekable part to a signed PUT URL with retries,
// stall timeout, and strict URL redaction.
func signedPutPart(
	ctx context.Context,
	client *http.Client,
	targetURL string,
	headers map[string]string,
	part io.ReadSeeker,
	size int64,
	rootName string,
	partIndex int,
	stallTimeout time.Duration,
	backoff []time.Duration,
	sleep func(context.Context, time.Duration) error,
) error {
	if targetURL == "" {
		return &signedBundleTransferError{Stage: "put_part", Root: rootName, PartIndex: partIndex, Class: signedClassInvalidEntry}
	}
	for attempt := 0; ; attempt++ {
		if _, err := part.Seek(0, io.SeekStart); err != nil {
			return &signedBundleTransferError{Stage: "put_part", Root: rootName, PartIndex: partIndex, Class: signedClassWrite}
		}
		status, class, retryable := signedPutOnce(ctx, client, targetURL, headers, part, size, stallTimeout)
		if class == "" {
			return nil
		}
		if !retryable || ctx.Err() != nil || attempt >= len(backoff) {
			if err := ctx.Err(); err != nil {
				return &signedBundleTransferError{Stage: "put_part", Root: rootName, PartIndex: partIndex, Class: contextClass(err)}
			}
			return &signedBundleTransferError{Stage: "put_part", Root: rootName, PartIndex: partIndex, Status: status, Class: class}
		}
		if err := sleep(ctx, backoff[attempt]); err != nil {
			return &signedBundleTransferError{Stage: "put_part", Root: rootName, PartIndex: partIndex, Class: contextClass(err)}
		}
	}
}

func signedPutOnce(
	ctx context.Context,
	client *http.Client,
	targetURL string,
	headers map[string]string,
	part io.Reader,
	size int64,
	stallTimeout time.Duration,
) (int, string, bool) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	body := newStallReader(io.LimitReader(part, size), stallTimeout, cancel)
	defer body.stop()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, body)
	if err != nil {
		return 0, signedClassInvalidEntry, false
	}
	httpReq.ContentLength = size
	hasContentType := false
	for k, v := range headers {
		if http.CanonicalHeaderKey(k) == "Content-Type" {
			hasContentType = true
		}
		httpReq.Header.Set(k, v)
	}
	if !hasContentType {
		httpReq.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		class := transportClass(err)
		if body.stalled() {
			class = signedClassTimeout
		}
		return 0, class, class != signedClassCanceled
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, "http_" + strconv.Itoa(resp.StatusCode), resp.StatusCode >= 500
	}
	return resp.StatusCode, "", false
}

// tempFileReadCloser wraps an *os.File and removes it on Close.
type tempFileReadCloser struct {
	f    *os.File
	path string
}

func (t *tempFileReadCloser) Read(p []byte) (int, error) {
	return t.f.Read(p)
}

func (t *tempFileReadCloser) Close() error {
	closeErr := t.f.Close()
	removeErr := os.Remove(t.path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

// signedGetOnce downloads a signed GET URL into a fresh temp file under
// scratchDir with a stall timeout. It returns (file, status, class, retryable).
// On a non-empty class, file is nil and any temp file has been removed.
func signedGetOnce(
	ctx context.Context,
	client *http.Client,
	targetURL string,
	scratchDir string,
	maxBytes int64,
	stallTimeout time.Duration,
) (io.ReadCloser, int, string, bool) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, 0, signedClassInvalidEntry, false
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		class := transportClass(err)
		return nil, 0, class, class != signedClassCanceled
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, resp.StatusCode, "http_" + strconv.Itoa(resp.StatusCode), resp.StatusCode >= 500
	}

	tmp, err := os.CreateTemp(scratchDir, "mb-part-get-*")
	if err != nil {
		return nil, resp.StatusCode, signedClassWrite, false
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	body := newStallReader(resp.Body, stallTimeout, cancel)
	defer body.stop()

	var reader io.Reader = body
	if maxBytes > 0 {
		reader = io.LimitReader(body, maxBytes+1)
	}
	_, copyErr := io.Copy(tmp, reader)
	if copyErr != nil {
		cleanup()
		class := transportClass(copyErr)
		if body.stalled() {
			class = signedClassTimeout
		}
		retryable := class == signedClassReset || class == signedClassTimeout || class == signedClassTransport
		return nil, resp.StatusCode, class, retryable
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, resp.StatusCode, signedClassWrite, false
	}
	return &tempFileReadCloser{f: tmp, path: tmpPath}, resp.StatusCode, "", false
}
