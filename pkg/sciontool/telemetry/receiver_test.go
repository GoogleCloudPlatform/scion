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

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func TestReceiverHTTPExports(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		request      proto.Message
		response     func() proto.Message
		processError string
		configure    func(*Receiver, *bool, error)
		serve        func(*Receiver, http.ResponseWriter, *http.Request)
	}{
		{
			name:         "traces",
			path:         "/v1/traces",
			request:      &coltracepb.ExportTraceServiceRequest{},
			response:     func() proto.Message { return &coltracepb.ExportTraceServiceResponse{} },
			processError: "Failed to process spans\n",
			configure: func(r *Receiver, called *bool, result error) {
				r.handler = func(context.Context, []*tracepb.ResourceSpans) error {
					*called = true
					return result
				}
			},
			serve: (*Receiver).handleHTTPTraces,
		},
		{
			name:         "metrics",
			path:         "/v1/metrics",
			request:      &colmetricpb.ExportMetricsServiceRequest{},
			response:     func() proto.Message { return &colmetricpb.ExportMetricsServiceResponse{} },
			processError: "Failed to process metrics\n",
			configure: func(r *Receiver, called *bool, result error) {
				r.metricHandler = func(context.Context, []*metricpb.ResourceMetrics) error {
					*called = true
					return result
				}
			},
			serve: (*Receiver).handleHTTPMetrics,
		},
		{
			name:         "logs",
			path:         "/v1/logs",
			request:      &collogspb.ExportLogsServiceRequest{},
			response:     func() proto.Message { return &collogspb.ExportLogsServiceResponse{} },
			processError: "Failed to process logs\n",
			configure: func(r *Receiver, called *bool, result error) {
				r.logHandler = func(context.Context, []*logspb.ResourceLogs) error {
					*called = true
					return result
				}
			},
			serve: (*Receiver).handleHTTPLogs,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := proto.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}

			t.Run("success", func(t *testing.T) {
				called := false
				receiver := &Receiver{}
				test.configure(receiver, &called, nil)
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)))

				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
				}
				if !called {
					t.Fatal("signal handler was not called")
				}
				if got := response.Header().Get("Content-Type"); got != "application/x-protobuf" {
					t.Fatalf("Content-Type = %q", got)
				}
				if err := proto.Unmarshal(response.Body.Bytes(), test.response()); err != nil {
					t.Fatalf("invalid response protobuf: %v", err)
				}
			})

			t.Run("method", func(t *testing.T) {
				called := false
				receiver := &Receiver{}
				test.configure(receiver, &called, nil)
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodGet, test.path, nil))

				if response.Code != http.StatusMethodNotAllowed || response.Body.String() != "Method not allowed\n" {
					t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
				}
				if called {
					t.Fatal("signal handler called for rejected method")
				}
			})

			t.Run("body read", func(t *testing.T) {
				called := false
				receiver := &Receiver{}
				test.configure(receiver, &called, nil)
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodPost, test.path, failingReader{}))

				if response.Code != http.StatusBadRequest || response.Body.String() != "Failed to read body\n" {
					t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
				}
				if called {
					t.Fatal("signal handler called after body read failure")
				}
			})

			t.Run("malformed protobuf", func(t *testing.T) {
				called := false
				receiver := &Receiver{}
				test.configure(receiver, &called, nil)
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader([]byte{0x80})))

				if response.Code != http.StatusBadRequest || response.Body.String() != "Failed to parse OTLP request\n" {
					t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
				}
				if called {
					t.Fatal("signal handler called for malformed protobuf")
				}
			})

			t.Run("handler error", func(t *testing.T) {
				called := false
				receiver := &Receiver{}
				test.configure(receiver, &called, errors.New("process failed"))
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)))

				if response.Code != http.StatusInternalServerError || response.Body.String() != test.processError {
					t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
				}
				if !called {
					t.Fatal("signal handler was not called")
				}
			})

			t.Run("nil handler", func(t *testing.T) {
				receiver := &Receiver{}
				response := httptest.NewRecorder()
				test.serve(receiver, response, httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)))

				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
				}
			})
		})
	}
}
