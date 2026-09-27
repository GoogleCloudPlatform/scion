/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"fmt"

	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
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
func stampIdentityLabels(input []*metricpb.ResourceMetrics) ([]*metricpb.ResourceMetrics, error) {
	output := make([]*metricpb.ResourceMetrics, 0, len(input))
	for _, source := range input {
		if source == nil {
			return nil, fmt.Errorf("nil resource metrics")
		}
		rm := proto.Clone(source).(*metricpb.ResourceMetrics)
		attrs := rm.GetResource().GetAttributes()
		agentID := metricAttrString(attrs, "scion.agent.id")
		projectID := metricAttrString(attrs, "scion.project.id")
		agentSlug := metricAttrString(attrs, "scion.agent.slug")
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.GetMetrics() {
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
