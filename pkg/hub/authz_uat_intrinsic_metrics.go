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
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Use counter for the intrinsic set of token-recorded hops
// (uatIntrinsicPermissionIDs, authz_effect_ceiling.go). A use is a decision
// that allowed a permission only because the intrinsic rule admitted it at a
// hop recorded from a user access token. It is normal behaviour, as for a
// session-created agent, so it is counted but never logged at Warn. The
// count is the evidence for any later decision to narrow the set.

// MetricUATIntrinsic is the intrinsic-rule use counter.
const MetricUATIntrinsic = "scion.hub.authz.uat_intrinsic"

// Path attributes of scion.hub.authz.uat_intrinsic.
const (
	// uatIntrinsicPathOwnerSecret: an owner's progeny-shared user secret at
	// create, start, resume, restart or as_needed (S3c, startSecretResolveOpts).
	uatIntrinsicPathOwnerSecret = "owner_secret"
	// uatIntrinsicPathLaunchDelivery: a launch delivery admission, once per
	// launch credential and deliver permission (hubDeliveryAdmission).
	uatIntrinsicPathLaunchDelivery = "launch_delivery"
	// uatIntrinsicPathStep10: the user secret parent ceiling inside Decide
	// (checkUserSecretParentCeiling), the runtime read's step 10.
	uatIntrinsicPathStep10 = "step10"
)

// uatIntrinsicMetrics receives intrinsic-rule uses.
type uatIntrinsicMetrics interface {
	RecordUATIntrinsic(ctx context.Context, permission, path string)
}

// OTelUATIntrinsicMetrics counts intrinsic-rule uses as
// scion.hub.authz.uat_intrinsic (attributes permission, path).
type OTelUATIntrinsicMetrics struct {
	total metric.Int64Counter
}

// NewOTelUATIntrinsicMetrics creates the counter.
func NewOTelUATIntrinsicMetrics(mp metric.MeterProvider) (*OTelUATIntrinsicMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel uat intrinsic metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter(MetricUATIntrinsic,
		metric.WithUnit("{use}"),
		metric.WithDescription("Decisions that allowed an agent-only permission only through the intrinsic set "+
			"of a delegation hop recorded from a user access token, by permission and path."))
	if err != nil {
		return nil, fmt.Errorf("creating authz.uat_intrinsic counter: %w", err)
	}
	return &OTelUATIntrinsicMetrics{total: c}, nil
}

// RecordUATIntrinsic implements uatIntrinsicMetrics.
func (m *OTelUATIntrinsicMetrics) RecordUATIntrinsic(ctx context.Context, permission, path string) {
	m.total.Add(ctx, 1, metric.WithAttributes(
		attribute.String("permission", permission),
		attribute.String("path", path),
	))
}

// SetUATIntrinsicMetrics installs the intrinsic-rule use counter.
func (s *Server) SetUATIntrinsicMetrics(m uatIntrinsicMetrics) {
	if s == nil || s.authzService == nil {
		return
	}
	s.authzService.uatIntrinsicMetrics = m
}

// UATIntrinsicUses returns the number of intrinsic-rule uses since the
// process started.
func (a *AuthzService) UATIntrinsicUses() int64 {
	if a == nil {
		return 0
	}
	return a.uatIntrinsicUses.Load()
}

// recordUATIntrinsic counts one intrinsic-rule use of permission on path.
func (a *AuthzService) recordUATIntrinsic(ctx context.Context, permission, path string) {
	if a == nil {
		return
	}
	a.uatIntrinsicUses.Add(1)
	if m := a.uatIntrinsicMetrics; m != nil {
		m.RecordUATIntrinsic(ctx, permission, path)
	}
}
