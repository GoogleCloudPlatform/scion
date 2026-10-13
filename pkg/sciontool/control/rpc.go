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

package control

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// RPCHandler serves conduit RpcRequests with h. Only canonical paths
// under Prefix reach h (see allowedPath): any other path is answered 404
// without calling it. Of the request headers only Content-Type is passed
// on. A panic in h is answered 500.
func RPCHandler(h http.Handler) core.RPCHandler {
	return core.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return serveRPC(ctx, h, req)
	})
}

func serveRPC(ctx context.Context, h http.Handler, req *conduitv1.RpcRequest) (resp *conduitv1.RpcResponse) {
	p := req.GetPath()
	if !allowedPath(p) {
		// The same answer the handler gives an unknown path.
		w := &responseWriter{header: http.Header{}}
		http.Error(w, "404 page not found", http.StatusNotFound)
		return w.response()
	}
	method := req.GetMethod()
	if method == "" {
		method = http.MethodGet
	}
	hr, err := http.NewRequestWithContext(ctx, method, "/", bytes.NewReader(req.GetBody()))
	if err != nil { // an invalid method token
		return statusResponse(http.StatusBadRequest)
	}
	hr.URL = &url.URL{Path: p, RawQuery: req.GetQuery()}
	hr.RequestURI = hr.URL.RequestURI()
	hr.ContentLength = int64(len(req.GetBody()))
	for k, v := range req.GetHeaders() {
		if http.CanonicalHeaderKey(k) == "Content-Type" {
			hr.Header.Set("Content-Type", v)
		}
	}

	w := &responseWriter{header: http.Header{}}
	defer func() {
		if r := recover(); r != nil {
			log.Error("Control: handler panic serving %s %s: %v", method, p, r)
			resp = statusResponse(http.StatusInternalServerError)
		}
	}()
	h.ServeHTTP(w, hr)
	return w.response()
}

// allowedPath is the conduit allow-list: a canonical path under Prefix.
// Percent-escapes and backslashes are refused outright, since no control
// route needs one, so an encoded slash or dot segment cannot reach the
// handler. The path must also already be clean, which refuses dot
// segments, repeated slashes and a trailing slash.
func allowedPath(p string) bool {
	return strings.HasPrefix(p, Prefix) &&
		!strings.ContainsAny(p, "%\\") &&
		path.Clean(p) == p
}

func statusResponse(status int) *conduitv1.RpcResponse {
	return &conduitv1.RpcResponse{Status: int32(status)}
}

// responseWriter buffers a handler's response for an RpcResponse. The
// body is capped just above core.MaxRPCBody: a larger response is
// answered 413 by the session anyway.
type responseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if room := core.MaxRPCBody + 1 - w.body.Len(); room < len(b) {
		if room > 0 {
			w.body.Write(b[:room])
		}
		return len(b), nil
	}
	return w.body.Write(b)
}

func (w *responseWriter) response() *conduitv1.RpcResponse {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	resp := &conduitv1.RpcResponse{Status: int32(status)}
	if len(w.header) > 0 {
		resp.Headers = make(map[string]string, len(w.header))
		for k, v := range w.header {
			if len(v) > 0 {
				resp.Headers[k] = strings.Join(v, ", ")
			}
		}
	}
	if w.body.Len() > 0 {
		resp.Body = w.body.Bytes()
	}
	return resp
}
