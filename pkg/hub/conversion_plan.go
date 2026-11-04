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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A legacy Runtime Broker reports its publishable conversion plan
// (api.ConversionPlanPresentation) on every heartbeat; the Hub stores it
// read-only on the broker row and serves it from
// GET /api/v1/runtime-brokers/{id}/conversion-plan. ApplyConversionPlan is
// the one writer: handleBrokerHeartbeat calls it only when the request is
// authenticated as the broker itself.

// Reasons ApplyConversionPlan does not store a snapshot. The stored value
// is kept in every case.
const (
	conversionPlanIgnoredOmitted       = "omitted"
	conversionPlanIgnoredExperimentOff = "experiment_off"
	conversionPlanIgnoredSchemaVersion = "unknown_schema_version"
	conversionPlanIgnoredFlatRow       = "flat_row"
	conversionPlanDroppedOverCap       = "over_cap"
	conversionPlanDroppedUndecodable   = "undecodable"
)

// ConversionPlanResult is what ApplyConversionPlan did: the store outcome,
// or the reason the snapshot was not stored.
type ConversionPlanResult struct {
	Outcome store.PresentationOutcome
	Ignored string
}

// ConversionPlanDrops is the number of snapshots dropped for being over a
// cap or not decoding since the Hub started.
func (s *Server) ConversionPlanDrops() int64 { return s.conversionPlanDrops.Load() }

// conversionPlanDroppedMetric is the exported count of dropped snapshots.
const conversionPlanDroppedMetric = "scion.hub.broker.conversion_plan_dropped"

// RegisterConversionPlanMetrics registers
// scion.hub.broker.conversion_plan_dropped, the cumulative count of
// conversion plan snapshots the Hub dropped for being over a cap or not
// decoding (the stored plan is kept). It counts only; no snapshot content is
// recorded.
func (s *Server) RegisterConversionPlanMetrics(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("conversion plan metrics: nil MeterProvider")
	}
	_, err := mp.Meter(instrumentationScope).Int64ObservableCounter(conversionPlanDroppedMetric,
		metric.WithUnit("{snapshot}"),
		metric.WithDescription("Runtime Broker conversion plan snapshots dropped by the Hub for being over a size or count cap or not decoding; the stored plan is kept"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(s.conversionPlanDrops.Load())
			return nil
		}))
	return err
}

// ApplyConversionPlan stores the conversion plan raw (the heartbeat
// field's bytes as received) reported for broker brokerID. It keeps the
// stored value when raw is absent, when the flat Runtime Brokers
// experiment is off, when the schema version is not one this Hub knows,
// and when the row is flat. A snapshot over any cap
// (api.CheckConversionPlanCaps, the byte cap checked before any decode) or
// that does not decode is dropped and counted; the log line names the
// broker and a fixed reason only, never content. The caller has checked
// that the request is authenticated as the broker itself.
func (s *Server) ApplyConversionPlan(ctx context.Context, brokerID string, raw json.RawMessage, now time.Time) (ConversionPlanResult, error) {
	// An ignored plan is logged at Debug: a legacy Runtime Broker sends its
	// plan on every heartbeat whatever the Hub's experiment state, so an
	// experiment-off Hub (the default) or an unknown schema version would
	// otherwise log a line per broker per heartbeat. A dropped plan (over a
	// cap, or not decodable) is a Warn and is counted.
	ignore := func(reason string) (ConversionPlanResult, error) {
		if reason != conversionPlanIgnoredOmitted {
			s.agentLifecycleLog.Debug("conversion plan not stored", "broker_id", brokerID, "reason", reason)
		}
		return ConversionPlanResult{Ignored: reason}, nil
	}
	drop := func(reason string) (ConversionPlanResult, error) {
		s.conversionPlanDrops.Add(1)
		s.agentLifecycleLog.Warn("conversion plan dropped", "broker_id", brokerID, "reason", reason)
		return ConversionPlanResult{Ignored: reason}, nil
	}
	if len(raw) == 0 || string(raw) == "null" {
		return ignore(conversionPlanIgnoredOmitted)
	}
	if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
		return ignore(conversionPlanIgnoredExperimentOff)
	}
	if err := api.CheckConversionPlanCaps(raw); err != nil {
		if errors.Is(err, api.ErrConversionPlanOverCap) {
			return drop(conversionPlanDroppedOverCap)
		}
		return drop(conversionPlanDroppedUndecodable)
	}
	var p api.ConversionPlanPresentation
	if err := json.Unmarshal(raw, &p); err != nil {
		return drop(conversionPlanDroppedUndecodable)
	}
	if p.SchemaVersion != api.ConversionPlanPresentationSchemaVersion {
		return ignore(conversionPlanIgnoredSchemaVersion)
	}
	stored := conversionPlanForStore(&p)
	hash, err := conversionPlanHash(stored)
	if err != nil {
		return ConversionPlanResult{}, err
	}
	outcome, err := s.store.SetRuntimeBrokerConversionPlan(ctx, brokerID, stored, hash, now)
	switch {
	case errors.Is(err, store.ErrRuntimeBrokerNotLegacy):
		return ignore(conversionPlanIgnoredFlatRow)
	case err != nil:
		return ConversionPlanResult{}, err
	}
	return ConversionPlanResult{Outcome: outcome}, nil
}

// conversionPlanForStore normalises p for storage: nil lists and maps
// become empty, and each instance's image registry gets the broker
// record's display redaction (presentationForStore). p is not modified.
func conversionPlanForStore(p *api.ConversionPlanPresentation) *api.ConversionPlanPresentation {
	out := *p
	out.Instances = make([]api.RuntimeTargetPresentation, 0, len(p.Instances))
	for i := range p.Instances {
		out.Instances = append(out.Instances, *presentationForStore(&p.Instances[i]))
	}
	if out.ProfileMap == nil {
		out.ProfileMap = map[string]string{}
	}
	out.Findings = make([]api.ConversionPlanFinding, 0, len(p.Findings))
	for _, f := range p.Findings {
		if f.Profiles == nil {
			f.Profiles = []string{}
		}
		out.Findings = append(out.Findings, f)
	}
	return &out
}

// conversionPlanHash is the hex SHA-256 of the canonical JSON of p, the
// plan as stored. ComputedAt is part of it, so a recompute on the Runtime
// Broker is stored even when nothing else changed.
func conversionPlanHash(p *api.ConversionPlanPresentation) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
