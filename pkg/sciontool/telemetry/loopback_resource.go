/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/sdk/resource"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
)

// loopbackResourceDialOption pins the resource that sciontool's own loopback
// providers put on the wire to the attributes of res (see buildResource).
//
// The OTel SDK's WithResource options (trace, metric and log) always merge
// resource.Environment() underneath the supplied resource, so
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME in sciontool's environment
// would otherwise add attributes outside the GCP resource allowlist and the
// loopback data would fail Cloud admission (ptone/scion#2249). The SDK has no
// way to opt out of that merge, and log records have no resource setter, so
// the attributes are filtered on the outgoing OTLP request instead. Keys that
// res defines already win the SDK merge, so dropping every other key leaves
// exactly the authoritative resource.
func loopbackResourceDialOption(res *resource.Resource) grpc.DialOption {
	allowed := make(map[string]bool, res.Len())
	for _, kv := range res.Attributes() {
		allowed[string(kv.Key)] = true
	}
	return grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		pinLoopbackRequestResource(req, allowed)
		return invoker(ctx, method, req, reply, cc, opts...)
	})
}

func pinLoopbackRequestResource(req any, allowed map[string]bool) {
	switch r := req.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		for _, rs := range r.GetResourceSpans() {
			pinResourceAttributes(rs.GetResource(), allowed)
		}
	case *colmetricpb.ExportMetricsServiceRequest:
		for _, rm := range r.GetResourceMetrics() {
			pinResourceAttributes(rm.GetResource(), allowed)
		}
	case *collogspb.ExportLogsServiceRequest:
		for _, rl := range r.GetResourceLogs() {
			pinResourceAttributes(rl.GetResource(), allowed)
		}
	}
}

func pinResourceAttributes(res *resourcepb.Resource, allowed map[string]bool) {
	if res == nil {
		return
	}
	kept := res.Attributes[:0]
	for _, kv := range res.Attributes {
		if kv != nil && allowed[kv.Key] {
			kept = append(kept, kv)
		}
	}
	clear(res.Attributes[len(kept):])
	res.Attributes = kept
}
