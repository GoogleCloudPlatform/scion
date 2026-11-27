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

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type transportMetaKey struct{}

// transportMeta carries per-request client metadata from the front door to
// the outbound Hub RoundTripper without forwarding raw inbound headers.
type transportMeta struct {
	RemoteAddr string
	ClientName string
}

func withTransportMeta(ctx context.Context, meta transportMeta) context.Context {
	return context.WithValue(ctx, transportMetaKey{}, meta)
}

func transportMetaFromContext(ctx context.Context) transportMeta {
	if m, ok := ctx.Value(transportMetaKey{}).(transportMeta); ok {
		return m
	}
	return transportMeta{}
}

// InProcessTransport is an http.RoundTripper that dispatches requests directly
// to an in-process http.Handler (such as hubSrv.Handler()) using a streaming
// io.Pipe-backed ResponseWriter. No network socket or loopback port is used.
type InProcessTransport struct {
	Handler http.Handler
}

// RoundTrip executes req against t.Handler in a goroutine and returns the
// streaming *http.Response as soon as response headers are written.
func (t *InProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.Handler == nil {
		return nil, errors.New("mcpserver: in-process transport has nil handler")
	}

	serverReq := req.Clone(req.Context())
	if serverReq.Body == nil {
		serverReq.Body = http.NoBody
	}
	if serverReq.Host == "" && serverReq.URL != nil {
		serverReq.Host = serverReq.URL.Host
	}
	if meta := transportMetaFromContext(req.Context()); meta.RemoteAddr != "" {
		serverReq.RemoteAddr = meta.RemoteAddr
	} else if serverReq.RemoteAddr == "" {
		serverReq.RemoteAddr = "127.0.0.1:0"
	}
	if serverReq.RequestURI == "" && serverReq.URL != nil {
		serverReq.RequestURI = serverReq.URL.RequestURI()
	}

	pr, pw := io.Pipe()
	rw := newPipeResponseWriter(pw)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				rw.abort(fmt.Errorf("mcpserver: in-process handler panic: %v", r))
				return
			}
			rw.finish()
		}()
		t.Handler.ServeHTTP(rw, serverReq)
	}()

	select {
	case <-rw.headerReady:
		if rw.err != nil {
			_ = pr.Close()
			return nil, rw.err
		}
		return &http.Response{
			StatusCode:    rw.statusCode,
			Status:        fmt.Sprintf("%d %s", rw.statusCode, http.StatusText(rw.statusCode)),
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        rw.committedHeaders,
			Body:          pr,
			ContentLength: -1,
			Request:       req,
		}, nil
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
}

// pipeResponseWriter is a streaming http.ResponseWriter backed by an io.PipeWriter.
// It intentionally does not implement SetWriteDeadline; callers like
// handleConvWatch ignore ErrNotSupported from ResponseController.
type pipeResponseWriter struct {
	mu               sync.Mutex
	header           http.Header
	committedHeaders http.Header
	statusCode       int
	wroteHeader      bool
	headerReady      chan struct{}
	headerOnce       sync.Once
	pw               *io.PipeWriter
	err              error
}

func newPipeResponseWriter(pw *io.PipeWriter) *pipeResponseWriter {
	return &pipeResponseWriter{
		header:      make(http.Header),
		headerReady: make(chan struct{}),
		pw:          pw,
	}
}

func (w *pipeResponseWriter) Header() http.Header {
	return w.header
}

func (w *pipeResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.wroteHeader {
		w.mu.Unlock()
		return
	}
	w.statusCode = code
	w.wroteHeader = true
	w.committedHeaders = w.header.Clone()
	w.mu.Unlock()
	w.headerOnce.Do(func() { close(w.headerReady) })
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

// Flush satisfies http.Flusher. Because io.Pipe is unbuffered, writes are
// already delivered immediately to the reader once headers are committed.
func (w *pipeResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

// Ensure pipeResponseWriter implements http.Flusher.
var _ http.Flusher = (*pipeResponseWriter)(nil)

func (w *pipeResponseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.pw.Close()
}

func (w *pipeResponseWriter) abort(err error) {
	w.mu.Lock()
	if !w.wroteHeader {
		w.err = err
		w.wroteHeader = true
		w.mu.Unlock()
		w.headerOnce.Do(func() { close(w.headerReady) })
		_ = w.pw.CloseWithError(err)
		return
	}
	w.mu.Unlock()
	_ = w.pw.CloseWithError(err)
}

// AllowlistTransport wraps a base http.RoundTripper and enforces the MCP->Hub
// header allowlist (§4.1, §10 AC4): only Authorization or
// X-Scion-Federation-Token, User-Agent, X-Scion-Client: mcp, and standard
// JSON content negotiation headers (Content-Type, Accept) may leave on a Hub
// request. Cookies, IAP assertion headers, and all other headers are dropped.
type AllowlistTransport struct {
	Base    http.RoundTripper
	Version string
}

// RoundTrip rebuilds the outbound header map from the strict allowlist and
// stamps User-Agent and X-Scion-Client: mcp before delegating to Base.
func (t *AllowlistTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	out := req.Clone(req.Context())
	h := make(http.Header)

	if authz := req.Header.Get("Authorization"); authz != "" {
		h.Set("Authorization", authz)
	}
	if fed := req.Header.Get(HeaderFederationToken); fed != "" {
		h.Set(HeaderFederationToken, fed)
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	}
	if acc := req.Header.Get("Accept"); acc != "" {
		h.Set("Accept", acc)
	}

	ver := t.Version
	if ver == "" {
		ver = "dev"
	}
	clientName := sanitizeHeaderValue(transportMetaFromContext(req.Context()).ClientName)
	if clientName == "" {
		clientName = "unknown"
	}
	h.Set("User-Agent", fmt.Sprintf("scion-mcp/%s (%s)", ver, clientName))
	h.Set(HeaderScionClient, ClientHeaderValue)

	out.Header = h
	return base.RoundTrip(out)
}

func sanitizeHeaderValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}
