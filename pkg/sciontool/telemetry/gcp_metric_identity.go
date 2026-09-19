package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Monitoring only maps point labels and selected service resource attributes.
// These fixed labels retain permitted post-policy stream identity. Fields not
// explicitly allowed here must never enter a Cloud label or identity digest.
const (
	gcpResourceIDLabel = "scion_metric_resource_id"
	gcpScopeIDLabel    = "scion_metric_scope_id"
	gcpPointIDLabel    = "scion_metric_point_id"
	gcpAgentLabel      = "scion_agent_id"
	gcpProjectLabel    = "scion_project_id"
)

var cloudResourceFields = map[string]bool{
	"service.name": true, "service.namespace": true, "service.instance.id": true,
	"scion.agent.id": true, "scion.project.id": true, "scion.agent.slug": true,
	"scion.harness": true, "scion.model": true, "scion.broker.name": true,
}
var cloudScopeFields = map[string]bool{"component": true, "scope.kind": true, "scope.variant": true}
var cloudPointFields = map[string]bool{
	"agent_id": true, "project_id": true, "harness": true, "model": true,
	"tool_name": true, "status": true, "operation": true, "sensor": true,
	"phase": true, "run": true,
}

func forbiddenCloudMetricField(key string) bool {
	key = strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToLower(key))
	for _, forbidden := range []string{"prompt", "conversation", "session_id", "tool_input", "tool_output", "payload", "message", "content"} {
		if strings.Contains(key, forbidden) {
			return true
		}
	}
	return false
}

func checkCloudMetricFields(attrs []*commonpb.KeyValue) error {
	for _, kv := range attrs {
		if forbiddenCloudMetricField(kv.Key) {
			return fmt.Errorf("forbidden Cloud Monitoring metric dimension")
		}
	}
	return nil
}

func allowedMetricAttrs(attrs []*commonpb.KeyValue, allowed map[string]bool) []*commonpb.KeyValue {
	var result []*commonpb.KeyValue
	for _, kv := range attrs {
		if allowed[kv.Key] {
			result = append(result, kv)
		}
	}
	return result
}

func cloudResourceKey(rm *metricpb.ResourceMetrics) (string, error) {
	copyRM := proto.Clone(rm).(*metricpb.ResourceMetrics)
	if copyRM.Resource != nil {
		copyRM.Resource.Attributes = allowedMetricAttrs(copyRM.Resource.Attributes, cloudResourceFields)
	}
	return resourceKey(copyRM)
}

func cloudScopeKey(sm *metricpb.ScopeMetrics) (string, error) {
	copySM := proto.Clone(sm).(*metricpb.ScopeMetrics)
	if copySM.Scope != nil {
		copySM.Scope.Attributes = allowedMetricAttrs(copySM.Scope.Attributes, cloudScopeFields)
	}
	return scopeKey(copySM)
}

func metricAttrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func gcpIdentityMetrics(input []*metricpb.ResourceMetrics) ([]*metricpb.ResourceMetrics, error) {
	output := make([]*metricpb.ResourceMetrics, 0, len(input))
	for _, source := range input {
		if source == nil {
			return nil, fmt.Errorf("nil Cloud Monitoring resource")
		}
		if err := checkCloudMetricFields(source.GetResource().GetAttributes()); err != nil {
			return nil, err
		}
		rm := proto.Clone(source).(*metricpb.ResourceMetrics)
		rkey, err := cloudResourceKey(source)
		if err != nil {
			return nil, err
		}
		resourceID := identityDigest(rkey)
		agentID := metricAttrString(source.GetResource().GetAttributes(), "scion.agent.id")
		projectID := metricAttrString(source.GetResource().GetAttributes(), "scion.project.id")
		if rm.Resource != nil {
			rm.Resource.Attributes = allowedMetricAttrs(rm.Resource.Attributes, cloudResourceFields)
		}
		for i, sm := range rm.ScopeMetrics {
			if sm == nil {
				return nil, fmt.Errorf("nil Cloud Monitoring scope")
			}
			if err := checkCloudMetricFields(source.ScopeMetrics[i].GetScope().GetAttributes()); err != nil {
				return nil, err
			}
			skey, err := cloudScopeKey(source.ScopeMetrics[i])
			if err != nil {
				return nil, err
			}
			scopeID := identityDigest(skey)
			if sm.Scope != nil {
				sm.Scope.Attributes = allowedMetricAttrs(sm.Scope.Attributes, cloudScopeFields)
			}
			for _, metric := range sm.Metrics {
				if metric == nil {
					return nil, fmt.Errorf("nil Cloud Monitoring metric")
				}
				if _, _, _, err := metricKind(metric); err != nil {
					return nil, err
				}
				for _, point := range metric.GetSum().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetGauge().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetHistogram().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
			}
		}
		output = append(output, rm)
	}
	return output, nil
}

func addGCPIdentityLabels(attrs *[]*commonpb.KeyValue, resourceID, scopeID, agentID, projectID string) error {
	if err := checkCloudMetricFields(*attrs); err != nil {
		return err
	}
	for _, kv := range *attrs {
		for _, reserved := range []string{gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel, "service_name", "service_namespace", "service_instance_id"} {
			if cloudLabelKey(kv.Key) == reserved {
				return fmt.Errorf("reserved Cloud Monitoring metric label")
			}
		}
	}
	*attrs = allowedMetricAttrs(*attrs, cloudPointFields)
	key, err := canonicalAttrs(*attrs)
	if err != nil {
		return err
	}
	*attrs = append(*attrs,
		metricStringLabel(gcpResourceIDLabel, resourceID),
		metricStringLabel(gcpScopeIDLabel, scopeID),
		metricStringLabel(gcpPointIDLabel, identityDigest(key)),
	)
	if agentID != "" {
		*attrs = append(*attrs, metricStringLabel(gcpAgentLabel, agentID))
	}
	if projectID != "" {
		*attrs = append(*attrs, metricStringLabel(gcpProjectLabel, projectID))
	}
	return nil
}

func metricStringLabel(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func identityDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
