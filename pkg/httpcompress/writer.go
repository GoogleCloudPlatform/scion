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

package httpcompress

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipLevel is the compression level used for every response.
const gzipLevel = 5

var gzipPool = sync.Pool{
	New: func() any {
		gz, err := gzip.NewWriterLevel(io.Discard, gzipLevel)
		if err != nil {
			panic(err) // gzipLevel is a valid constant level
		}
		return gz
	},
}

// errHijackCompressing is returned by Hijack once the response has started
// as (or is held as) a compressed body.
var errHijackCompressing = errors.New("httpcompress: cannot hijack a compressed response")

type writerState int

const (
	// stateUndecided: nothing written yet; the decision is still open.
	stateUndecided writerState = iota
	// stateHolding: the response qualified as JSON without a
	// Content-Length; up to minSize bytes are held to decide whether it is
	// worth compressing. Headers are not yet sent.
	stateHolding
	// stateCompressing: headers sent with Content-Encoding: gzip.
	stateCompressing
	// statePassthrough: every call goes straight to the underlying writer.
	statePassthrough
)

// compressWriter is the ResponseWriter handed to the wrapped handler.
type compressWriter struct {
	w       http.ResponseWriter
	minSize int
	state   writerState
	status  int
	held    []byte
	gz      *gzip.Writer
}

func newCompressWriter(w http.ResponseWriter, minSize int) *compressWriter {
	return &compressWriter{w: w, minSize: minSize}
}

// Header returns the underlying header map; handlers set headers as usual.
func (cw *compressWriter) Header() http.Header { return cw.w.Header() }

// Unwrap returns the underlying ResponseWriter, for http.ResponseController.
func (cw *compressWriter) Unwrap() http.ResponseWriter { return cw.w }

// WriteHeader records the status and makes the compression decision.
func (cw *compressWriter) WriteHeader(code int) {
	if cw.state != stateUndecided {
		if cw.state == statePassthrough {
			cw.w.WriteHeader(code)
		}
		// Superfluous WriteHeader on a held or compressing response: ignore,
		// as net/http does.
		return
	}
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		// Informational responses do not end the header phase.
		cw.w.WriteHeader(code)
		return
	}
	cw.decide(code)
}

// Write writes body bytes, deciding at the first call if needed.
func (cw *compressWriter) Write(p []byte) (int, error) {
	if cw.state == stateUndecided {
		cw.decide(http.StatusOK)
	}
	switch cw.state {
	case stateHolding:
		cw.held = append(cw.held, p...)
		if len(cw.held) >= cw.minSize {
			if err := cw.startCompressing(); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	case stateCompressing:
		return cw.gz.Write(p)
	default:
		return cw.w.Write(p)
	}
}

// Flush implements http.Flusher.
func (cw *compressWriter) Flush() { _ = cw.FlushError() }

// FlushError flushes buffered data to the client. A held JSON body starts
// compressing; a compressing body flushes the gzip stream first.
func (cw *compressWriter) FlushError() error {
	if cw.state == stateUndecided {
		cw.decide(http.StatusOK)
	}
	switch cw.state {
	case stateHolding:
		if err := cw.startCompressing(); err != nil {
			return err
		}
		if err := cw.gz.Flush(); err != nil {
			return err
		}
	case stateCompressing:
		if err := cw.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(cw.w).Flush()
}

// Hijack is delegated in passthrough (or still undecided) mode. It fails
// once the response is held or compressing.
func (cw *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	switch cw.state {
	case stateUndecided, statePassthrough:
		cw.state = statePassthrough
		return http.NewResponseController(cw.w).Hijack()
	default:
		return nil, nil, errHijackCompressing
	}
}

// decide makes the one-time compression decision for status code.
func (cw *compressWriter) decide(code int) {
	cw.status = code
	if !cw.eligible(code) {
		cw.state = statePassthrough
		cw.w.WriteHeader(code)
		return
	}
	if cw.w.Header().Get("Content-Length") != "" {
		// eligible already checked Content-Length >= minSize, so there is
		// nothing to hold back.
		_ = cw.startCompressing()
		return
	}
	cw.state = stateHolding
}

// eligible applies the response-time rules.
func (cw *compressWriter) eligible(code int) bool {
	switch code {
	case http.StatusOK, http.StatusCreated, http.StatusNonAuthoritativeInfo:
	default:
		return false
	}
	h := cw.w.Header()
	if _, set := h["Content-Encoding"]; set {
		return false
	}
	if !isJSONMediaType(h.Get("Content-Type")) {
		return false
	}
	if cl := h.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < int64(cw.minSize) {
			return false
		}
	}
	return true
}

func isJSONMediaType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || mt == "application/problem+json"
}

// startCompressing sends the compressed-response headers and the held bytes.
func (cw *compressWriter) startCompressing() error {
	h := cw.w.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	if !headerHasToken(h, "Vary", "Accept-Encoding") {
		h.Add("Vary", "Accept-Encoding")
	}
	if etag := h.Get("ETag"); etag != "" {
		h.Set("ETag", weakenETag(etag))
	}
	cw.w.WriteHeader(cw.status)
	cw.gz = gzipPool.Get().(*gzip.Writer)
	cw.gz.Reset(cw.w)
	cw.state = stateCompressing
	held := cw.held
	cw.held = nil
	if len(held) > 0 {
		if _, err := cw.gz.Write(held); err != nil {
			return err
		}
	}
	return nil
}

// weakenETag turns `"x"` or `W/"x"` into `W/"x-gz"`.
func weakenETag(etag string) string {
	tag := strings.TrimPrefix(etag, "W/")
	if len(tag) >= 2 && strings.HasPrefix(tag, `"`) && strings.HasSuffix(tag, `"`) {
		return `W/"` + tag[1:len(tag)-1] + `-gz"`
	}
	return `W/"` + strings.Trim(tag, `"`) + `-gz"`
}

// finish completes the response after the handler has returned.
func (cw *compressWriter) finish() {
	switch cw.state {
	case stateHolding:
		// Small JSON: send it as identity.
		cw.state = statePassthrough
		cw.w.WriteHeader(cw.status)
		if len(cw.held) > 0 {
			_, _ = cw.w.Write(cw.held)
		}
		cw.held = nil
	case stateCompressing:
		_ = cw.gz.Close()
		cw.release()
		cw.state = statePassthrough
	}
}

// abort releases resources without writing anything more.
func (cw *compressWriter) abort() {
	cw.held = nil
	if cw.gz != nil {
		cw.release()
	}
}

func (cw *compressWriter) release() {
	cw.gz.Reset(io.Discard)
	gzipPool.Put(cw.gz)
	cw.gz = nil
}
