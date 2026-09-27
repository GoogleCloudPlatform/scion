/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"fmt"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// stampIdentityLabels adds the canonical scion_project_id, scion_agent_id
// and scion_agent_slug point labels to every metric point, from the
// receiver's authoritative resource identity only (design §3.4). Unlike the
// GCP path (gcpIdentityMetrics), this does not enforce the Cloud Monitoring
// allowlist: generic OTLP still forwards every other attribute unchanged. It
// runs on both exporters so any backend, not only Cloud Monitoring, gets
// point-level identity (today generic OTLP kept identity on the resource
// only).
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
					if err := stampGenericPoint(&point.Attributes, agentID, projectID, agentSlug); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetGauge().GetDataPoints() {
					if err := stampGenericPoint(&point.Attributes, agentID, projectID, agentSlug); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetHistogram().GetDataPoints() {
					if err := stampGenericPoint(&point.Attributes, agentID, projectID, agentSlug); err != nil {
						return nil, err
					}
				}
			}
		}
		output = append(output, rm)
	}
	return output, nil
}

// stampGenericPoint rejects a producer-supplied canonical identity label
// (design AC-1.1b: "producer-supplied values for these keys are rejected"),
// then appends the labels from authoritative identity.
func stampGenericPoint(attrs *[]*commonpb.KeyValue, agentID, projectID, agentSlug string) error {
	for _, kv := range *attrs {
		if kv == nil {
			continue
		}
		for _, reserved := range identityLabelKeys {
			if kv.Key == reserved {
				return fmt.Errorf("reserved canonical identity metric label")
			}
		}
	}
	appendCanonicalIdentity(attrs, agentID, projectID, agentSlug)
	return nil
}
