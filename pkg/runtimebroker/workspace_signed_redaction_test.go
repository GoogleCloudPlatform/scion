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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// urlLeaks returns the signed-URL material found in text: an X-Goog- query
// parameter, or any issued URL.
func urlLeaks(text string, issued []string) []string {
	var found []string
	if strings.Contains(text, "X-Goog-") {
		found = append(found, "X-Goog-")
	}
	for _, u := range issued {
		if u != "" && strings.Contains(text, u) {
			found = append(found, u)
		}
	}
	return found
}

// spanText collects every span's status description, attributes and events
// as text.
func recordedSpansText(rec *tracetest.SpanRecorder) string {
	var b strings.Builder
	for _, s := range rec.Ended() {
		fmt.Fprintf(&b, "%s|%s|", s.Name(), s.Status().Description)
		for _, a := range s.Attributes() {
			fmt.Fprintf(&b, "%s=%s;", a.Key, a.Value.String())
		}
		for _, e := range s.Events() {
			fmt.Fprintf(&b, "event %s:", e.Name)
			for _, a := range e.Attributes {
				fmt.Fprintf(&b, "%s=%s;", a.Key, a.Value.String())
			}
		}
	}
	return b.String()
}

// filesText concatenates every regular file under dir (the launch journal
// and any other broker state written there).
func filesText(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil {
			b.Write(data)
		}
		return nil
	})
	return b.String()
}

// rawUnmappedFailure performs the scenario's failing step without the
// downloader's mapping and returns the raw text a naive implementation would
// have logged: the page request's error, else the object GET's *url.Error,
// its body read error, or its error response body.
func rawUnmappedFailure(t *testing.T, srv *Server, fetch func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error)) string {
	t.Helper()
	resp, err := fetch(context.Background(), CreateAgentRequest{}, "")
	if err != nil {
		return err.Error()
	}
	if len(resp.Files) == 0 {
		t.Fatal("the page lists no file")
	}
	client := srv.signedDownload.client
	if client == nil {
		client = newSignedDownloadClient()
	}
	r, err := client.Get(resp.Files[0].URL)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = r.Body.Close() }()
	body, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		return rerr.Error()
	}
	if r.StatusCode != http.StatusOK {
		return string(body)
	}
	return ""
}

// leakScenario injects one failure into the signed download. setup returns
// the page source and the URLs it issues.
type leakScenario struct {
	name  string
	setup func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string)
	// rawNoURL marks the one scenario whose raw failure carries no URL by
	// net/http's design: a body read error after the response headers is
	// io.ErrUnexpectedEOF itself, not a *url.Error. It still exercises the
	// class mapping (reset); the leak path for a reset is covered by "reset
	// before response".
	rawNoURL bool
}

// signedPage returns a page source with one file at base+"/o/a" carrying a
// V4-shaped query, and records the URLs it issues.
func signedPage(base string) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
	issued := &[]string{}
	return func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error) {
		u := fmt.Sprintf("%s/o/a?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=hub%%40example&X-Goog-Signature=sig%d", base, len(*issued))
		*issued = append(*issued, u)
		return &api.WorkspaceDownloadResponse{Files: []api.WorkspaceDownloadFile{{Path: "a", URL: u, Size: 100}}, Expires: time.Now().Add(time.Hour)}, nil
	}, issued
}

func leakScenarios() []leakScenario {
	return []leakScenario{
		{name: "closed listener", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := l.Addr().String()
			_ = l.Close()
			return signedPage("http://" + addr)
		}},
		{name: "mid-body reset", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("partial"))
				if hj, ok := w.(http.Hijacker); ok {
					if c, _, err := hj.Hijack(); err == nil {
						if tc, ok := c.(*net.TCPConn); ok {
							_ = tc.SetLinger(0)
						}
						_ = c.Close()
					}
				}
			}))
			t.Cleanup(obj.Close)
			return signedPage(obj.URL)
		}, rawNoURL: true},
		{name: "reset before response", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hj, ok := w.(http.Hijacker); ok {
					if c, _, err := hj.Hijack(); err == nil {
						if tc, ok := c.(*net.TCPConn); ok {
							_ = tc.SetLinger(0)
						}
						_ = c.Close()
					}
				}
			}))
			t.Cleanup(obj.Close)
			return signedPage(obj.URL)
		}},
		{name: "context timeout", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}))
			t.Cleanup(obj.Close)
			srv.signedDownload.client = &http.Client{Timeout: 100 * time.Millisecond}
			return signedPage(obj.URL)
		}},
		{name: "object store 403", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "<Error>SignatureDoesNotMatch "+r.URL.String()+"</Error>", http.StatusForbidden)
			}))
			t.Cleanup(obj.Close)
			return signedPage(obj.URL)
		}},
		{name: "hub endpoint 5xx", setup: func(t *testing.T, srv *Server) (func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error), *[]string) {
			u := "https://storage.googleapis.com/b/o?X-Goog-Signature=from-endpoint-body"
			issued := &[]string{u}
			return func(context.Context, CreateAgentRequest, string) (*api.WorkspaceDownloadResponse, error) {
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "upstream answered " + u}
			}, issued
		}},
	}
}

// TestSignedDownload_NoURLLeaks injects each transport failure into a full
// create, synchronous and async, and asserts that no X-Goog- parameter and
// no issued URL reaches the broker log, the response, the span status, any
// span attribute or event, the async failure message, the launch journal or
// the returned error. The detector is checked on planted values first.
func TestSignedDownload_NoURLLeaks(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	origTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(origTP) })

	// Detector control: a planted log line, span status and error are found.
	planted := "X-Goog-Signature=planted"
	if len(urlLeaks("log line "+planted, nil)) == 0 || len(urlLeaks(fmt.Errorf("err: %s", planted).Error(), nil)) == 0 {
		t.Fatal("detector control: planted values must be found")
	}
	// Span controls, one surface per span so each is proven on its own: a
	// planted status, attribute value and event attribute value are each
	// found, and a clean span is not.
	for _, pc := range []struct {
		name  string
		plant func(s trace.Span)
		want  bool
	}{
		{"status only detected", func(s trace.Span) { s.SetStatus(codes.Error, "status "+planted) }, true},
		{"attribute only detected", func(s trace.Span) { s.SetAttributes(attribute.String("planted.url", "https://h/o?"+planted)) }, true},
		{"event attribute only detected", func(s trace.Span) {
			s.AddEvent("planted", trace.WithAttributes(attribute.String("planted.url", "https://h/o?"+planted)))
		}, true},
		{"clean span not detected", func(s trace.Span) {
			s.SetStatus(codes.Error, "status fixed")
			s.SetAttributes(attribute.String("k", "v"))
			s.AddEvent("e", trace.WithAttributes(attribute.String("k", "v")))
		}, false},
	} {
		t.Run("detector control/"+pc.name, func(t *testing.T) {
			rec := tracetest.NewSpanRecorder()
			ptp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			_, ps := ptp.Tracer("t").Start(context.Background(), "planted")
			pc.plant(ps)
			ps.End()
			got := len(urlLeaks(recordedSpansText(rec), nil)) > 0
			if got != pc.want {
				t.Fatalf("found %v, want %v (text %q)", got, pc.want, recordedSpansText(rec))
			}
			t.Logf("detected=%v", got)
		})
	}
	if t.Failed() {
		t.FailNow()
	}

	for _, sc := range leakScenarios() {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/async=%v", sc.name, async), func(t *testing.T) {
				srv, rtb := newAsyncTestServer(t, newAsyncManager())
				srv.config.WorktreeBase = t.TempDir()
				srv.signedDownload.sleep = noSleep
				fetch, issued := sc.setup(t, srv)
				srv.signedDownload.fetchPage = fetch
				forbidGCSDownload(t, srv)
				logs := captureLifecycleJSONLog(srv)
				rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
					return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
				}
				name := "agent-leak-" + strings.ReplaceAll(sc.name, " ", "-")
				w := postCreate(t, srv, signedCreate(name, api.WorkspaceDownloadModeSignedURLs, async))
				surfaces := map[string]string{"response": w.Body.String()}
				if async {
					if w.Code != http.StatusCreated {
						t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
					}
					surfaces["async failure message"] = waitFailedReport(t, rtb).Message
				} else if w.Code < 400 {
					t.Fatalf("status = %d, want a failure", w.Code)
				}
				if len(*issued) == 0 {
					t.Fatal("the scenario must issue at least one URL")
				}
				surfaces["broker log"] = logs.String()
				surfaces["spans"] = recordedSpansText(recorder)
				surfaces["broker state files"] = filesText(t, os.Getenv("HOME"))
				if !strings.Contains(logs.String(), "signed workspace download") && !strings.Contains(logs.String(), "signed workspace bootstrap") {
					t.Fatalf("the failure cause must still be logged (URL-free): %s", logs.String())
				}
				for where, text := range surfaces {
					if leaks := urlLeaks(text, *issued); len(leaks) > 0 {
						t.Errorf("%s leaks %v:\n%s", where, leaks, text)
					}
				}
			})
		}
		t.Run(sc.name+"/returned error", func(t *testing.T) {
			srv, _ := newAsyncTestServer(t, newAsyncManager())
			srv.config.WorktreeBase = t.TempDir()
			srv.signedDownload.sleep = noSleep
			fetch, issued := sc.setup(t, srv)
			srv.signedDownload.fetchPage = fetch
			// Positive control per scenario: the raw failure, before the
			// mapping, carries the URL, so this scenario exercises a real
			// leak path and the mapping is what keeps it out.
			raw := rawUnmappedFailure(t, srv, fetch)
			if raw == "" {
				t.Fatal("the scenario must fail")
			}
			if sc.rawNoURL {
				if len(urlLeaks(raw, *issued)) != 0 {
					t.Fatalf("expected a URL-free raw body read error, got %q", raw)
				}
			} else if len(urlLeaks(raw, *issued)) == 0 {
				t.Fatalf("the raw failure must carry the URL for this scenario to prove anything: %q", raw)
			}
			req := CreateAgentRequest{ID: "agent-ret", Name: "agent-ret", WorkspaceStoragePath: "p", WorkspaceDownloadMode: api.WorkspaceDownloadModeSignedURLs}
			_, attempt, msg, err := srv.downloadWorkspaceFromGCS(context.Background(), req, api.StartOptions{})
			if err == nil {
				t.Fatal("want a failure")
			}
			for where, text := range map[string]string{"error": err.Error(), "attempt": attempt, "client text": msg} {
				if leaks := urlLeaks(text, *issued); len(leaks) > 0 {
					t.Errorf("%s leaks %v: %s", where, leaks, text)
				}
			}
		})
	}
}

// TestSignedBootstrap_NoCloudCredentials: with no Google credentials and an
// unreachable metadata server, a hub-managed project's workspace is
// bootstrapped through the Hub's signed URLs, for a synchronous and an async
// create; the bucket download is never used.
func TestSignedBootstrap_NoCloudCredentials(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
			t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
			srv, rtb := newAsyncTestServer(t, newAsyncManager())
			srv.signedDownload.sleep = noSleep
			forbidGCSDownload(t, srv)
			h := newFakeSignedHub(t, map[string]string{"README.md": "hello", "src/app.go": "package app"})
			wireSignedHub(rtb, h)
			rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			}
			name := fmt.Sprintf("agent-nocreds-%v", async)
			body := signedCreate(name, api.WorkspaceDownloadModeSignedURLs, async)
			body["projectSlug"] = "notes"
			w := postCreate(t, srv, body)
			if w.Code >= 300 {
				t.Fatalf("create = %d: %s", w.Code, w.Body.String())
			}
			globalDir, err := config.GetGlobalDir()
			if err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(globalDir, "projects", "notes")
			if !waitUntil(t, 5*time.Second, func() bool {
				b, err := os.ReadFile(filepath.Join(ws, "src", "app.go"))
				return err == nil && string(b) == "package app"
			}) {
				t.Fatalf("workspace = %v", readTree(t, ws))
			}
			if _, err := os.Stat(filepath.Join(ws, ".scion")); err != nil {
				t.Fatalf("the project identity entry must be kept: %v", err)
			}
			if h.pageCalls == 0 {
				t.Fatal("the Hub's workspace download must have been used")
			}
		})
	}
}
