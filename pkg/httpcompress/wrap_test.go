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
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// jsonBody returns a JSON document of at least n bytes.
func jsonBody(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"agent-` + strconv.Itoa(i) + `","phase":"running"}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// newServer serves h through Wrap with the default options plus bypass.
func newServer(t *testing.T, h http.Handler, bypass func(string) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Wrap(h, Options{BypassPath: bypass, MinSize: DefaultMinSize}))
	t.Cleanup(srv.Close)
	return srv
}

// rawClient does not add or strip Accept-Encoding itself.
func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}
}

func doReq(t *testing.T, method, url, acceptEncoding string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func gunzip(t *testing.T, r io.Reader) []byte {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return b
}

func jsonHandler(body []byte, setLength bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if setLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		_, _ = w.Write(body)
	}
}

func assertIdentity(t *testing.T, resp *http.Response, want []byte) {
	t.Helper()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q, want none", ce)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func assertGzip(t *testing.T, resp *http.Response, want []byte) {
	t.Helper()
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if !headerHasToken(resp.Header, "Vary", "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", resp.Header.Values("Vary"))
	}
	if got := gunzip(t, resp.Body); !bytes.Equal(got, want) {
		t.Fatalf("decoded body differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestWrap_JSONCompressed(t *testing.T) {
	body := jsonBody(4096)
	for _, setLength := range []bool{false, true} {
		t.Run("content-length="+strconv.FormatBool(setLength), func(t *testing.T) {
			srv := newServer(t, jsonHandler(body, setLength), nil)
			resp := doReq(t, http.MethodGet, srv.URL+"/api/v1/agents", "gzip, deflate, br", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			// net/http may compute a length for the compressed body, but
			// the handler's identity length must never be sent.
			if got := resp.Header.Get("Content-Length"); got == strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length = %q, the identity length", got)
			}
			assertGzip(t, resp, body)
		})
	}
}

func TestWrap_ProblemJSONCompressed(t *testing.T) {
	body := jsonBody(2048)
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}), nil)
	resp := doReq(t, http.MethodPost, srv.URL+"/x", "gzip", strings.NewReader("{}"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertGzip(t, resp, body)
}

func TestWrap_SmallContentLengthNotCompressed(t *testing.T) {
	body := jsonBody(DefaultMinSize - 200)
	srv := newServer(t, jsonHandler(body, true), nil)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
	if resp.Header.Get("Vary") != "" {
		t.Fatalf("Vary = %q, want none", resp.Header.Get("Vary"))
	}
	assertIdentity(t, resp, body)
}

func TestWrap_SmallJSONUncompressed(t *testing.T) {
	body := []byte(`{"version":"1.0.0","authProviders":["google"],"experiments":{},"devAuth":false,"maintenance":false,"brand":"scion","features":["chat","terminals"],"x":"` + strings.Repeat("a", 10) + `"}`)
	if len(body) >= DefaultMinSize {
		t.Fatalf("fixture too large: %d", len(body))
	}
	srv := newServer(t, jsonHandler(body, false), nil)
	resp := doReq(t, http.MethodGet, srv.URL+"/api/v1/settings/public", "gzip", nil)
	if resp.Header.Get("Vary") != "" {
		t.Fatalf("Vary = %q, want none", resp.Header.Get("Vary"))
	}
	assertIdentity(t, resp, body)
}

func TestWrap_JSONAtThresholdCompressed(t *testing.T) {
	body := bytes.Repeat([]byte("a"), DefaultMinSize)
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Several small writes cross the threshold together.
		for i := 0; i < len(body); i += 100 {
			end := min(i+100, len(body))
			_, _ = w.Write(body[i:end])
		}
	}), nil)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	assertGzip(t, resp, body)
}

func TestWrap_EventStreamPassthroughUnbuffered(t *testing.T) {
	// The first case omits Accept: text/event-stream so the request-time
	// bypass does not apply and the response-time rule alone must pass the
	// stream through; the second exercises the request-time bypass.
	for _, accept := range []string{"", "text/event-stream"} {
		t.Run("accept="+accept, func(t *testing.T) {
			release := make(chan struct{})
			handlerDone := make(chan struct{})
			srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				_, _ = io.WriteString(w, "data: hello\n\n")
				w.(http.Flusher).Flush()
				<-release
			}), nil)
			defer close(release)

			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/agents/a1/messages/stream", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			if accept != "" {
				req.Header.Set("Accept", accept)
			}
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Fatalf("Content-Encoding = %q, want none", ce)
			}
			if v := resp.Header.Get("Vary"); v != "" {
				t.Fatalf("Vary = %q, want none", v)
			}
			lineCh := make(chan string, 1)
			go func() {
				line, _ := bufio.NewReader(resp.Body).ReadString('\n')
				lineCh <- line
			}()
			select {
			case line := <-lineCh:
				if line != "data: hello\n" {
					t.Fatalf("first line = %q", line)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("event not received before the handler was released")
			}
			select {
			case <-handlerDone:
				t.Fatal("handler finished before the event was read")
			default:
			}
		})
	}
}

func TestWrap_UpgradeNotWrapped(t *testing.T) {
	type result struct {
		wrapped bool
		err     error
	}
	results := make(chan result, 1)
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, wrapped := w.(*compressWriter)
		conn, rw, err := http.NewResponseController(w).Hijack()
		results <- result{wrapped: wrapped, err: err}
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
	}), nil)

	for _, hdrs := range []string{
		"Upgrade: websocket\r\nConnection: Upgrade\r\n",
		"Connection: keep-alive, Upgrade\r\n",
	} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "GET /api/v1/agents/a1/pty HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n"+hdrs+"\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		status, err := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		res := <-results
		if res.wrapped {
			t.Fatalf("handler got the compress wrapper for an upgrade request (%q)", hdrs)
		}
		if res.err != nil {
			t.Fatalf("Hijack: %v", res.err)
		}
		if !strings.HasPrefix(status, "HTTP/1.1 101") {
			t.Fatalf("status line = %q", status)
		}
	}
}

func TestWrap_BypassPath(t *testing.T) {
	body := jsonBody(4096)
	var sawWrapper bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawWrapper = w.(*compressWriter)
		jsonHandler(body, false)(w, r)
	})
	srv := newServer(t, h, func(p string) bool { return strings.HasPrefix(p, "/api/v1/auth/") })

	resp := doReq(t, http.MethodGet, srv.URL+"/api/v1/auth/me", "gzip", nil)
	if sawWrapper {
		t.Fatal("bypassed path got the compress wrapper")
	}
	assertIdentity(t, resp, body)

	resp = doReq(t, http.MethodGet, srv.URL+"/api/v1/agents", "gzip", nil)
	if !sawWrapper {
		t.Fatal("non-bypassed path did not get the compress wrapper")
	}
	assertGzip(t, resp, body)
}

func TestWrap_AlreadyEncodedPassthrough(t *testing.T) {
	var pre bytes.Buffer
	zw := gzip.NewWriter(&pre)
	_, _ = zw.Write(jsonBody(4096))
	_ = zw.Close()
	encoded := pre.Bytes()
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(encoded)
	}), nil)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", resp.Header.Get("Content-Encoding"))
	}
	if v := resp.Header.Get("Vary"); v != "" {
		t.Fatalf("Vary = %q, want none (not added by the wrapper)", v)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, encoded) {
		t.Fatal("pre-encoded body was modified (double compression?)")
	}
}

func TestWrap_NonOKStatusPassthrough(t *testing.T) {
	large := jsonBody(4096)
	cases := []struct {
		code int
		body []byte
	}{
		{http.StatusNoContent, nil},
		{http.StatusNotModified, nil},
		{http.StatusBadRequest, large},
		{http.StatusUnauthorized, large},
		{http.StatusNotFound, large},
		{http.StatusConflict, large},
		{http.StatusInternalServerError, large},
		{http.StatusAccepted, large},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				if tc.body != nil {
					_, _ = w.Write(tc.body)
				}
			}), nil)
			resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
			if resp.StatusCode != tc.code {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.code)
			}
			if v := resp.Header.Get("Vary"); v != "" {
				t.Fatalf("Vary = %q, want none", v)
			}
			want := tc.body
			if want == nil {
				want = []byte{}
			}
			assertIdentity(t, resp, want)
		})
	}
}

func TestWrap_NonJSONPassthrough(t *testing.T) {
	body := bytes.Repeat([]byte("<p>hello</p>"), 500)
	for _, ct := range []string{"text/html; charset=utf-8", "application/octet-stream", "application/jsonx", "text/plain", ""} {
		t.Run(ct, func(t *testing.T) {
			srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				_, _ = w.Write(body)
			}), nil)
			resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
			assertIdentity(t, resp, body)
		})
	}
}

func TestWrap_HEAD(t *testing.T) {
	body := jsonBody(4096)
	var sawWrapper bool
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawWrapper = w.(*compressWriter)
		jsonHandler(body, true)(w, r)
	}), nil)
	resp := doReq(t, http.MethodHead, srv.URL+"/x", "gzip", nil)
	if sawWrapper {
		t.Fatal("HEAD request got the compress wrapper")
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q, want none", ce)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
}

func TestWrap_QZeroGzip(t *testing.T) {
	body := jsonBody(4096)
	srv := newServer(t, jsonHandler(body, false), nil)
	for _, ae := range []string{"gzip;q=0", "br, gzip; q=0.0", "GZIP;Q=0.000", "identity", "deflate, br", "*"} {
		t.Run(ae, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
			req.Header.Set("Accept-Encoding", ae)
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			assertIdentity(t, resp, body)
		})
	}
	t.Run("no-header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		assertIdentity(t, resp, body)
	})
	t.Run("gzip;q=0.5", func(t *testing.T) {
		resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip;q=0.5, br;q=1", nil)
		assertGzip(t, resp, body)
	})
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                   false,
		"gzip":               true,
		"GZip":               true,
		"deflate, gzip":      true,
		"gzip;q=1.0":         true,
		"gzip;q=0.001":       true,
		"gzip;q=0":           false,
		"gzip; q=0.":         false,
		"gzip;q=bogus":       false,
		"x-gzip":             false,
		"*":                  false,
		"br;q=0, gzip":       true,
		"identity;q=0, gzip": true,
	}
	for in, want := range cases {
		h := http.Header{}
		if in != "" {
			h.Set("Accept-Encoding", in)
		}
		if got := acceptsGzip(h); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWrap_EventStreamAcceptBypass(t *testing.T) {
	var sawWrapper bool
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawWrapper = w.(*compressWriter)
		jsonHandler(jsonBody(4096), false)(w, r)
	}), nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept", "application/json, Text/Event-Stream")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if sawWrapper {
		t.Fatal("event-stream request got the compress wrapper")
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q, want none", ce)
	}
}

// readGzipPrefix decodes want from a still-open gzip response body.
func readGzipPrefix(t *testing.T, body io.Reader, want []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		zr, err := gzip.NewReader(body)
		if err != nil {
			done <- err
			return
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(zr, got); err != nil {
			done <- err
			return
		}
		if !bytes.Equal(got, want) {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode flushed prefix: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flushed bytes did not reach the client before the handler returned")
	}
}

func TestWrap_FlushWhileCompressing(t *testing.T) {
	first := jsonBody(4096)
	release := make(chan struct{})
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(first)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
		<-release
		_, _ = w.Write([]byte("\n"))
	}), nil)
	defer close(release)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	readGzipPrefix(t, resp.Body, first)
}

func TestWrap_FlushForcesDecision(t *testing.T) {
	first := []byte(`{"seq":12}`)
	if len(first) != 10 {
		t.Fatalf("fixture is %d bytes, want 10", len(first))
	}
	release := make(chan struct{})
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(first)
		w.(http.Flusher).Flush()
		<-release
	}), nil)
	defer close(release)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	readGzipPrefix(t, resp.Body, first)
}

func TestWrap_ETagWeakened(t *testing.T) {
	body := jsonBody(4096)
	cases := map[string]string{
		`"abc123"`:   `W/"abc123-gz"`,
		`W/"abc123"`: `W/"abc123-gz"`,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", in)
				jsonHandler(body, false)(w, r)
			}), nil)
			resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
			if got := resp.Header.Get("ETag"); got != want {
				t.Fatalf("ETag = %q, want %q", got, want)
			}
			assertGzip(t, resp, body)

			// The identity variant keeps the handler's validator.
			resp = doReq(t, http.MethodGet, srv.URL+"/x", "identity", nil)
			if got := resp.Header.Get("ETag"); got != in {
				t.Fatalf("identity ETag = %q, want %q", got, in)
			}
		})
	}
}

func TestWrap_VaryAppended(t *testing.T) {
	body := jsonBody(4096)
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin")
		jsonHandler(body, false)(w, r)
	}), nil)
	resp := doReq(t, http.MethodGet, srv.URL+"/x", "gzip", nil)
	vary := resp.Header.Values("Vary")
	if !headerHasToken(resp.Header, "Vary", "Origin") || !headerHasToken(resp.Header, "Vary", "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Origin and Accept-Encoding", vary)
	}
	assertGzip(t, resp, body)
}

func TestWrap_PatchJSONControlNotBypassed(t *testing.T) {
	large := jsonBody(2048)
	small := []byte(`{"connection_id":"c1","subscriptions":["agent.a1"]}`)
	for _, tc := range []struct {
		name string
		body []byte
		gzip bool
	}{
		{"large", large, true},
		{"small", small, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawWrapper bool
			srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, sawWrapper = w.(*compressWriter)
				if r.Method != http.MethodPatch {
					t.Errorf("method = %s", r.Method)
				}
				jsonHandler(tc.body, false)(w, r)
			}), func(p string) bool { return strings.HasSuffix(p, "/stream") })
			resp := doReq(t, http.MethodPatch, srv.URL+"/events/c1", "gzip", strings.NewReader(`{"add":["x"]}`))
			if !sawWrapper {
				t.Fatal("PATCH control call was bypassed")
			}
			if tc.gzip {
				assertGzip(t, resp, tc.body)
			} else {
				assertIdentity(t, resp, tc.body)
			}
		})
	}
}

func TestWrap_HijackWhileCompressingFails(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCompressWriter(rec, DefaultMinSize)
	cw.Header().Set("Content-Type", "application/json")
	_, _ = cw.Write(jsonBody(4096))
	if _, _, err := cw.Hijack(); err == nil {
		t.Fatal("Hijack succeeded on a compressing response")
	}
	cw.finish()
}

func TestWrap_EmptyJSONResponse(t *testing.T) {
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}), nil)
	resp := doReq(t, http.MethodPost, srv.URL+"/x", "gzip", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertIdentity(t, resp, []byte{})
}

func TestWrap_PanicReleases(t *testing.T) {
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBody(4096))
		panic(http.ErrAbortHandler)
	}), Options{})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req)
}
