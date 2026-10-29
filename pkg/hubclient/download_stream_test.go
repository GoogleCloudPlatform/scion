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

package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

type downloadStreamBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *downloadStreamBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

type downloadStreamTransport struct {
	base     http.RoundTripper
	body     *downloadStreamBody
	requests int
}

func (rt *downloadStreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requests++
	resp, err := rt.base.RoundTrip(req)
	if err == nil {
		rt.body = &downloadStreamBody{ReadCloser: resp.Body}
		resp.Body = rt.body
	}
	return resp, err
}

func TestDownloadStream_ResponseContract(t *testing.T) {
	const capability = "capability-fixture"
	for _, endpoint := range []string{"artifact", "delivery"} {
		t.Run(endpoint, func(t *testing.T) {
			for _, status := range []int{200, 201, 202, 204, 205, 206, 207, 299, 300, 301, 304, 308, 403, 404, 429, 503} {
				for _, empty := range []bool{false, true} {
					if empty && status != http.StatusOK {
						continue
					}
					name := strconv.Itoa(status)
					if empty {
						name += " empty file"
					}
					t.Run(name, func(t *testing.T) {
						wantPath := "/api/v1/artifacts/asset/versions/1/files/a.txt"
						if endpoint == "delivery" {
							wantPath = attachmentDeliveriesPath + capability
						}
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodGet || r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer credential-fixture" {
								t.Error("unexpected download request")
							}
							w.Header().Set("X-Request-ID", "trace-fixture")
							w.Header().Set("Retry-After", "7")
							w.WriteHeader(status)
							if empty || status == http.StatusNoContent || status == http.StatusNotModified {
								return
							}
							if status >= 400 {
								_, _ = io.WriteString(w, `{"error":{"code":"refused","message":"refused"}}`)
							} else {
								_, _ = io.WriteString(w, "file bytes")
							}
						}))
						defer srv.Close()
						base := http.DefaultTransport.(*http.Transport).Clone()
						defer base.CloseIdleConnections()
						rt := &downloadStreamTransport{base: base}
						c, err := New(srv.URL, WithBearerToken("credential-fixture"), WithRetry(3, 0), WithHTTPClient(&http.Client{Transport: rt}))
						if err != nil {
							t.Fatal(err)
						}
						var body io.ReadCloser
						if endpoint == "artifact" {
							body, err = c.Artifacts().OpenFile(context.Background(), "asset", 1, "a.txt")
						} else {
							body, err = c.(AttachmentDeliveryFetcher).FetchAttachmentDelivery(context.Background(), wantPath)
						}
						if body != nil {
							defer func() { _ = body.Close() }()
						}
						if rt.requests != 1 || rt.body == nil {
							t.Fatalf("requests = %d, response body = %v; want one request with a body", rt.requests, rt.body)
						}
						if status == http.StatusOK {
							if err != nil || body == nil || rt.body.closed.Load() {
								t.Fatalf("download = %v, %v; want an open stream", body, err)
							}
							data, readErr := io.ReadAll(body)
							want := "file bytes"
							if empty {
								want = ""
							}
							if readErr != nil || string(data) != want {
								t.Fatalf("read = %q, %v; want %q", data, readErr, want)
							}
							return
						}
						if body != nil || err == nil || !rt.body.closed.Load() {
							t.Fatalf("download = %v, %v; closed = %v; want nil, error and a closed response", body, err, rt.body.closed.Load())
						}
						if endpoint == "delivery" && (strings.Contains(err.Error(), capability) || strings.Contains(err.Error(), srv.URL)) {
							t.Fatalf("delivery error exposes capability or URL: %v", err)
						}
						if status < 300 && !strings.Contains(err.Error(), strconv.Itoa(status)) {
							t.Errorf("error %q does not identify status %d", err, status)
						}
						if endpoint == "delivery" && status == http.StatusNotFound {
							if !errors.Is(err, errDeliveryRefused) {
								t.Errorf("error = %v; want delivery refusal", err)
							}
						} else if status >= 400 {
							var apiErr *apiclient.APIError
							if !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.Code != "refused" || apiErr.RequestID != "trace-fixture" || apiErr.RetryAfterSeconds != 7 {
								t.Errorf("error = %#v; want original API error metadata", err)
							}
						}
					})
				}
			}
		})
	}
}
