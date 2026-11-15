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

package jevwire

import (
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	// AdapterNameTypeSafe is the registered adapter name for TypeSafe Jev.
	AdapterNameTypeSafe = "typesafe"

	// AdapterVersionTypeSafe is the version string recorded in provenance for
	// the TypeSafe jevwire profile.
	AdapterVersionTypeSafe = "0.1.0"

	// DefaultTypeSafeEndpoint is the default HTTPS base URL when InstanceView.Endpoint is unset.
	DefaultTypeSafeEndpoint = "https://api.typesafe.ai"

	// DecisionsPath is the Jev-family decision evaluation HTTP path.
	DecisionsPath = "/v1/systemone"

	// ModelsPath is the TypeSafe model catalog HTTP path.
	ModelsPath = "/v1/models"

	// HeaderTypeSafeRequestID is the content-free response header carrying TypeSafe's request ID.
	HeaderTypeSafeRequestID = "x-typesafe-request-id"

	// Danger-zone opt-in headers documented by TypeSafe. The jevwire TypeSafe
	// profile never sends either header so image inputs and lower-accuracy 262k
	// long-context mode remain disabled (provider-design §7.4).
	HeaderDangerZoneImages      = "TypeSafe-Danger-Zone-Images"
	HeaderDangerZoneLongContext = "TypeSafe-Danger-Zone-Long-Context"

	// NativeMetricTypeSafeConfidence is the namespaced native metric key for
	// TypeSafe's ChoiceAnswer confidence field.
	NativeMetricTypeSafeConfidence = "typesafe.confidence"

	// NoteConfidenceFormulaDeviation is a content-free Answer observation note
	// recorded when a ChoiceAnswer's confidence deviates from the documented
	// (n*p_max-1)/(n-1) formula by more than 1e-3 without failing validation (U4).
	NoteConfidenceFormulaDeviation = "confidence_formula_deviation"

	// TypeSafeProviderRetention documents the unverified retention status (U6).
	TypeSafeProviderRetention = "UNKNOWN — not stated in fetched docs"
)

// Profile configures a specific Jev-wire implementation profile (such as
// "typesafe" in PV1, and later profiles in PV2).
type Profile struct {
	Name                    string
	Version                 string
	DefaultEndpoint         string
	DecisionsPath           string
	ModelsPath              string
	RequestIDHeader         string
	KnownAliases            []string
	ConfidenceMetric        *provider.MetricDecl
	RequireChoiceConfidence bool
	CheckConfidenceFormula  bool
	ChoiceNullOnTie         bool
	AllowReportedTruncation bool
	Capabilities            provider.Capabilities
}

// TypeSafeProfile returns the authoritative PV1 capability and wire profile for
// TypeSafe Jev (provider-design §5.2, §7.4).
func TypeSafeProfile() Profile {
	confDecl := provider.MetricDecl{
		Name:        NativeMetricTypeSafeConfidence,
		Unit:        metrics.UnitFraction,
		Min:         0,
		Max:         1,
		HigherMeans: "more_concentrated",
		Source:      metrics.SourceProviderDoc,
	}
	return Profile{
		Name:                    AdapterNameTypeSafe,
		Version:                 AdapterVersionTypeSafe,
		DefaultEndpoint:         DefaultTypeSafeEndpoint,
		DecisionsPath:           DecisionsPath,
		ModelsPath:              ModelsPath,
		RequestIDHeader:         HeaderTypeSafeRequestID,
		KnownAliases:            []string{"jev-latest", "jev-preview"},
		ConfidenceMetric:        &confDecl,
		RequireChoiceConfidence: true,
		CheckConfidenceFormula:  true,
		ChoiceNullOnTie:         false,
		AllowReportedTruncation: false,
		Capabilities: provider.Capabilities{
			Primitives: map[string]provider.PrimitiveLimits{
				spec.PrimitiveBinary: {
					Supported:                true,
					DistributionCompleteness: provider.CompletenessComplete,
				},
				spec.PrimitiveChoice: {
					Supported:                true,
					MinOptions:               spec.MinChoiceOptions,
					MaxOptions:               spec.MaxChoiceOptions,
					DistributionCompleteness: provider.CompletenessComplete,
				},
			},
			MaxQuestionsPerRequest: 64,
			MaxInputTokens:         32768,
			Truncation:             provider.TruncationNever,
			ReturnsDistribution:    true,
			NativeMetrics:          []provider.MetricDecl{confDecl},
			Abstains:               false,
			SideEffectFree:         true,
			AuthScheme:             provider.AuthBearer,
			ProviderRetention:      TypeSafeProviderRetention,
		},
	}
}

func init() {
	provider.MustRegister(NewTypeSafe())
}

// Register registers the TypeSafe profile into r.
func Register(r *provider.Registry) error {
	return r.Register(NewTypeSafe())
}
