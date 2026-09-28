/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// stampIdentityLabels adds the canonical scion_project_id, scion_agent_id
// and scion_agent_slug point labels to every metric point — Sum, Gauge,
// Histogram, ExponentialHistogram and Summary alike (F5: "every exported
// metric point" per AC-1.1b) — from the receiver's authoritative resource
// identity only (design §3.4). Unlike the GCP path (gcpIdentityMetrics),
// this does not enforce the Cloud Monitoring allowlist: generic OTLP still
// forwards every other attribute unchanged. It runs on both exporters so any
// backend, not only Cloud Monitoring, gets point-level identity (today
// generic OTLP kept identity on the resource only).
//
// This is pure stamping (F3): a producer-supplied reserved identity key is
// rejected at admission (metricStreams.add, rejectReservedIdentityPointLabel)
// before a point ever reaches this function, so there is nothing left to
// reject here, and nothing here can fail a whole batch that admission has
// already accepted point-by-point.
//
// The ExponentialHistogram and Summary branches are unreachable through the
// real pipeline today (round-2 review Nit-2): Pipeline.handleMetrics calls
// metricKind on every metric in a request, for both exporters, before any
// admission or export step runs, and metricKind only recognizes Sum, Gauge
// and Histogram — a request containing either kind is rejected outright
// (InvalidArgument) long before ExportProtoMetrics or this function sees it.
// They stay, stamping correctly rather than silently skipping, in case that
// restriction is ever lifted for the generic-forwarding path specifically;
// TestGenericOTLPIdentityStampingCoversEveryPointKind exercises this
// function directly (bypassing metricKind) to pin that they do.
func stampIdentityLabels(input []*metricpb.ResourceMetrics) ([]*metricpb.ResourceMetrics, error) {
	output := make([]*metricpb.ResourceMetrics, 0, len(input))
	for _, source := range input {
		if source == nil {
			// Also unreachable through the real pipeline: metricStreams.add
			// rejects a nil ResourceMetrics at admission (both exporters),
			// so nothing nil ever reaches a stream, let alone a snapshot
			// batch passed to ExportProtoMetrics. Kept as a defensive,
			// explicitly non-retryable guard against a direct or future
			// caller, rather than a panic.
			return nil, status.Error(codes.InvalidArgument, "nil resource metrics")
		}
		rm := proto.Clone(source).(*metricpb.ResourceMetrics)
		attrs := rm.GetResource().GetAttributes()
		agentID := metricAttrString(attrs, "scion.agent.id")
		projectID := metricAttrString(attrs, "scion.project.id")
		agentSlug := metricAttrString(attrs, "scion.agent.slug")
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.GetMetrics() {
				// A nil point cannot reach appendCanonicalIdentity here
				// (Gemini finding on upstream PR 2051, identity_stamp.go:73):
				// verified two layers deep, not merely inferred.
				//   1. Through the real pipeline, admission itself rejects a
				//      nil point before it is ever stored, and metricStreams
				//      only ever repopulates DataPoints from a proto.Clone of
				//      an already-admitted, non-nil point (metric_streams.go,
				//      e.g. entry.metric.GetSum().DataPoints =
				//      []*metricpb.NumberDataPoint{copyPoint}).
				//   2. Even for a direct or future caller that hand-builds a
				//      ResourceMetrics with an explicit nil entry in a
				//      DataPoints slice: `rm := proto.Clone(source)` above
				//      runs first and, verified with a standalone
				//      proto.Clone repro, replaces a nil element of a
				//      repeated message field with a fresh non-nil
				//      zero-value message rather than preserving the nil.
				//      So `point` is never nil by the time this loop runs,
				//      regardless of what the caller passed in.
				// See TestGenericOTLPIdentityStampingToleratesNilDataPointEntries,
				// which pins point 2 and would fail if a future refactor
				// stopped cloning before this loop.
				for _, point := range metric.GetSum().GetDataPoints() {
					appendCanonicalIdentity(&point.Attributes, agentID, projectID, agentSlug)
				}
				for _, point := range metric.GetGauge().GetDataPoints() {
					appendCanonicalIdentity(&point.Attributes, agentID, projectID, agentSlug)
				}
				for _, point := range metric.GetHistogram().GetDataPoints() {
					appendCanonicalIdentity(&point.Attributes, agentID, projectID, agentSlug)
				}
				for _, point := range metric.GetExponentialHistogram().GetDataPoints() {
					appendCanonicalIdentity(&point.Attributes, agentID, projectID, agentSlug)
				}
				for _, point := range metric.GetSummary().GetDataPoints() {
					appendCanonicalIdentity(&point.Attributes, agentID, projectID, agentSlug)
				}
			}
		}
		output = append(output, rm)
	}
	return output, nil
}
