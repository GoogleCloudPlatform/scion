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

// Package hubtracing creates the OpenTelemetry TracerProvider used by
// hub and broker HTTP servers. It exports directly to GCP Cloud Trace
// via Application Default Credentials.
package hubtracing

import (
	"context"
	"fmt"
	"os"

	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Option configures the TracerProvider.
type Option func(*options)

type options struct {
	hubID       string
	hubName     string
	serviceName string
}

// WithHubID sets the scion.hub.id resource attribute.
func WithHubID(id string) Option {
	return func(o *options) { o.hubID = id }
}

// WithHubName sets the scion.hub.name resource attribute.
func WithHubName(name string) Option {
	return func(o *options) { o.hubName = name }
}

// WithServiceName overrides the default service name ("scion-server").
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// NewTracerProvider creates an OTel SDK TracerProvider that exports to GCP
// Cloud Trace. It uses Application Default Credentials (workload identity on
// Cloud Run, attached SA on GCE).
//
// The provider is registered as the global tracer provider and a W3C
// TraceContext propagator is installed so that incoming traceparent headers
// are recognised as parent spans.
//
// If gcpProjectID is empty, an error is returned.
func NewTracerProvider(ctx context.Context, gcpProjectID string, opts ...Option) (*trace.TracerProvider, error) {
	if gcpProjectID == "" {
		return nil, fmt.Errorf("GCP project ID is required for hub tracing export")
	}

	o := &options{serviceName: "scion-server"}
	for _, fn := range opts {
		fn(o)
	}

	exporter, err := texporter.New(texporter.WithProjectID(gcpProjectID))
	if err != nil {
		return nil, fmt.Errorf("creating GCP trace exporter: %w", err)
	}

	resAttrs := []attribute.KeyValue{
		semconv.ServiceName(o.serviceName),
	}
	if o.hubID != "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.id", o.hubID))
	}
	if envHubID := os.Getenv("SCION_HUB_ID"); envHubID != "" && o.hubID == "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.id", envHubID))
	}
	if o.hubName != "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.name", o.hubName))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTel resource: %w", err)
	}

	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(res),
	)

	// Register as the global tracer provider.
	otel.SetTracerProvider(tp)

	// Install W3C TraceContext propagator so incoming traceparent headers
	// are extracted as parent span context.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp, nil
}
