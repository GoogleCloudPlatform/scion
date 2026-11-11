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

// Package httpcompress provides an HTTP middleware that gzips JSON API
// responses while leaving streaming, upgraded and non-JSON responses
// untouched.
//
// Streaming is preserved by three independent layers:
//
//  1. Request-time bypass: upgrade requests, event-stream requests, paths
//     selected by Options.BypassPath, HEAD requests and clients that do not
//     accept gzip get the original ResponseWriter; no wrapper is installed.
//  2. Response-time decision, made once at the first WriteHeader, Write or
//     Flush: only 200/201/203 responses whose media type is
//     application/json or application/problem+json, that carry no
//     Content-Encoding and whose Content-Length is unset or at least
//     MinSize are compressed. Everything else is passed through unchanged.
//  3. Flush semantics: Flush in compressing mode flushes the gzip stream and
//     then the underlying writer, so flushed JSON reaches the client promptly.
package httpcompress

import (
	"net/http"
	"strconv"
	"strings"
)

// DefaultMinSize is the smallest JSON body, in bytes, that is compressed.
const DefaultMinSize = 1024

// Options configures Wrap.
type Options struct {
	// BypassPath reports request paths that are never wrapped.
	BypassPath func(path string) bool
	// MinSize is the smallest body that is compressed. Zero or a negative
	// value means DefaultMinSize.
	MinSize int
}

// Wrap returns a handler that gzips eligible responses.
func Wrap(next http.Handler, opts Options) http.Handler {
	minSize := opts.MinSize
	if minSize <= 0 {
		minSize = DefaultMinSize
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bypassRequest(r, opts.BypassPath) {
			next.ServeHTTP(w, r)
			return
		}
		cw := newCompressWriter(w, minSize)
		completed := false
		defer func() {
			if !completed {
				// next panicked: drop any held body and return the gzip
				// writer to the pool, then let the panic continue.
				cw.abort()
			}
		}()
		next.ServeHTTP(cw, r)
		completed = true
		cw.finish()
	})
}

// bypassRequest reports whether the request must reach next with the
// original ResponseWriter.
func bypassRequest(r *http.Request, bypassPath func(string) bool) bool {
	if r.Method == http.MethodHead {
		return true
	}
	if r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade") {
		return true
	}
	for _, v := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(v), "text/event-stream") {
			return true
		}
	}
	if bypassPath != nil && bypassPath(r.URL.Path) {
		return true
	}
	return !acceptsGzip(r.Header)
}

// headerHasToken reports whether any comma-separated element of the named
// header equals token, case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// acceptsGzip reports whether Accept-Encoding lists gzip with a non-zero
// quality value. A wildcard is not treated as gzip.
func acceptsGzip(h http.Header) bool {
	for _, v := range h.Values("Accept-Encoding") {
		for _, part := range strings.Split(v, ",") {
			coding, params, _ := strings.Cut(part, ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			if qualityIsZero(params) {
				return false
			}
			return true
		}
	}
	return false
}

// qualityIsZero reports whether the parameter list carries q=0 (any of
// "0", "0.", "0.0", "0.00", "0.000"). An unparsable q is treated as zero.
func qualityIsZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return true
		}
		return q <= 0
	}
	return false
}
