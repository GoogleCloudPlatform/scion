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
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// serveTunneledRequest runs one request tunnelled from the Hub through the
// broker's HTTP handlers and hands the response envelope to deliver. The control
// channel and the conduit session both call it, so a tunnelled request
// reaches the same handler tree whichever connection carried it.
//
// ctx is the request's own context (cancelled when the Hub cancels the
// request). spanName names the dispatch span. connectionName, if set, is
// passed to the handlers in X-Scion-Hub-Connection so they can route to the
// correct hydrator. A handler panic (e.g. httptest.NewRequest on a
// malformed URL) is recovered and answered with 400 instead of crashing
// the broker process. deliver runs inside the dispatch span; its error is
// logged and recorded on the span.
func serveTunneledRequest(ctx context.Context, handlers http.Handler, connectionName, spanName string, log *slog.Logger, req wsprotocol.RequestEnvelope, deliver func(*wsprotocol.ResponseEnvelope) error) {
	// Extract trace context from request envelope headers for cross-component propagation.
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(req.Headers))
	ctx, span := tracer.Start(ctx, spanName)
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.request.method", req.Method),
		attribute.String("scion.request.path", req.Path),
	)

	defer func() {
		if r := recover(); r != nil {
			span.SetStatus(codes.Error, fmt.Sprintf("panic: %v", r))
			log.Error("Panic in tunnelled request handler", "panic", r, "span", spanName, "method", req.Method, "path", req.Path)
			resp := wsprotocol.NewResponseEnvelope(req.RequestID, http.StatusBadRequest, nil, []byte(fmt.Sprintf(`{"error":"request caused panic: %v"}`, r)))
			if writeErr := deliver(resp); writeErr != nil {
				log.Error("Failed to send panic error response", "error", writeErr)
			}
		}
	}()

	// Build HTTP request
	path := req.Path
	if req.Query != "" {
		path = path + "?" + req.Query
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}

	httpReq := httptest.NewRequest(req.Method, path, body)
	httpReq = httpReq.WithContext(ctx)
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	// Inject connection name header so the server can route to the correct hydrator
	if connectionName != "" {
		httpReq.Header.Set("X-Scion-Hub-Connection", connectionName)
	}

	// Execute through existing handlers
	w := httptest.NewRecorder()
	handlers.ServeHTTP(w, httpReq)

	// Build response envelope
	result := w.Result()
	respBody, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()

	headers := make(map[string]string)
	for key := range result.Header {
		headers[key] = result.Header.Get(key)
	}

	resp := wsprotocol.NewResponseEnvelope(req.RequestID, result.StatusCode, headers, respBody)

	if err := deliver(resp); err != nil {
		span.SetStatus(codes.Error, "failed to send response: "+err.Error())
		log.Error("Failed to send response", "error", err, "requestID", req.RequestID)
	}
}
