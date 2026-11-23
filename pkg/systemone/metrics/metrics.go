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

// Package metrics computes Scion's portable concentration metrics over closed
// probability distributions returned by system-one adapters.
//
// Adapters never compute these metrics and never map provider-declared
// confidence fields onto them; Scion computes p_top, margin, and norm_entropy
// identically across providers (provider-design §6.2).
package metrics

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	// NamePTop is max_i p_i.
	NamePTop = "p_top"
	// NameMargin is p_(1) - p_(2), the gap between the top two probabilities.
	NameMargin = "margin"
	// NameNormEntropy is -sum_i(p_i * ln(p_i)) / ln(K), normalized Shannon entropy in [0, 1].
	NameNormEntropy = "norm_entropy"

	// UnitProbability is the unit for probability and probability-gap metrics in [0, 1].
	UnitProbability = "probability"
	// UnitFraction is the unit for dimensionless ratios in [0, 1] such as normalized entropy.
	UnitFraction = "fraction"
	// UnitNats is the unit for raw natural-log entropy metrics.
	UnitNats = "nats"

	// SourceScion marks a metric computed by Scion from the returned distribution.
	SourceScion = "scion"
	// SourceProviderDoc marks a native metric whose formula is documented by the provider.
	SourceProviderDoc = "provider-doc"
	// SourceProviderUndocumented marks a native metric whose formula is not documented by the provider.
	SourceProviderUndocumented = "provider-undocumented"

	// SumTolerance is the maximum allowed deviation of sum(p_i) from 1.0 (provider-design §6.3).
	SumTolerance = 1e-3
	// SumToleranceFloatSlack is numerical slack (1e-12) for IEEE-754 binary64
	// representation at the existing SumTolerance (1e-3) boundary (for example
	// {0.99, 0.004, 0.003, 0.002}, whose exact decimal sum is 0.999 where
	// |sum - 1| exceeds float64(1e-3) by ~8.9e-19). It widens the accepted band
	// by a negligible 1e-12—mathematically non-zero, far below reported
	// probability precision—and is not a wider contract or provider-rounding
	// accommodation.
	SumToleranceFloatSlack = 1e-12
)

var (
	// ErrTooFewOptions indicates a distribution has fewer than 2 options.
	ErrTooFewOptions = errors.New("metrics: distribution must contain at least 2 options")
	// ErrInvalidProbability indicates an option probability is non-finite or outside [0, 1].
	ErrInvalidProbability = errors.New("metrics: probability must be finite and in [0, 1]")
	// ErrUnnormalizedDistribution indicates the probabilities do not sum to 1 ± 1e-3.
	ErrUnnormalizedDistribution = errors.New("metrics: distribution sum outside 1 ± 1e-3")
)

// Metric is a typed uncertainty or concentration measurement attached to an
// Answer, carrying explicit unit, range, and provenance source (provider-design §6.1).
type Metric struct {
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Source string  `json:"source"`
}

// IsPortableMetric reports whether name is one of the three Scion-computed
// portable metrics (p_top, margin, norm_entropy).
func IsPortableMetric(name string) bool {
	switch name {
	case NamePTop, NameMargin, NameNormEntropy:
		return true
	default:
		return false
	}
}

// Compute validates dist (without renormalizing) and returns the three
// Scion-computed portable metrics: p_top, margin, and norm_entropy.
func Compute(dist map[string]float64) (map[string]Metric, error) {
	sortedDesc, err := sortedValidatedProbs(dist)
	if err != nil {
		return nil, err
	}
	pTop := sortedDesc[0]
	margin := sortedDesc[0] - sortedDesc[1]
	normEnt := normEntropyFromSorted(sortedDesc)

	return map[string]Metric{
		NamePTop: {
			Value:  pTop,
			Unit:   UnitProbability,
			Min:    0,
			Max:    1,
			Source: SourceScion,
		},
		NameMargin: {
			Value:  margin,
			Unit:   UnitProbability,
			Min:    0,
			Max:    1,
			Source: SourceScion,
		},
		NameNormEntropy: {
			Value:  normEnt,
			Unit:   UnitFraction,
			Min:    0,
			Max:    1,
			Source: SourceScion,
		},
	}, nil
}

// PTop returns max_i p_i over a validated distribution.
func PTop(dist map[string]float64) (float64, error) {
	sortedDesc, err := sortedValidatedProbs(dist)
	if err != nil {
		return 0, err
	}
	return sortedDesc[0], nil
}

// Margin returns p_(1) - p_(2) over a validated distribution.
func Margin(dist map[string]float64) (float64, error) {
	sortedDesc, err := sortedValidatedProbs(dist)
	if err != nil {
		return 0, err
	}
	return sortedDesc[0] - sortedDesc[1], nil
}

// NormEntropy returns -sum_i(p_i * ln(p_i)) / ln(K) in [0, 1] over a validated
// distribution, using 0 * ln(0) = 0.
func NormEntropy(dist map[string]float64) (float64, error) {
	sortedDesc, err := sortedValidatedProbs(dist)
	if err != nil {
		return 0, err
	}
	return normEntropyFromSorted(sortedDesc), nil
}

// sortedValidatedProbs validates every probability in dist without repairing or
// renormalizing it, and returns the values sorted descending for deterministic
// floating-point arithmetic regardless of Go map iteration order.
func sortedValidatedProbs(dist map[string]float64) ([]float64, error) {
	if len(dist) < 2 {
		return nil, ErrTooFewOptions
	}
	probs := make([]float64, 0, len(dist))
	for k, p := range dist {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return nil, fmt.Errorf("%w: key %q = %v", ErrInvalidProbability, k, p)
		}
		probs = append(probs, p)
	}
	sort.Slice(probs, func(i, j int) bool {
		return probs[i] > probs[j]
	})

	// Sum ascending (smallest magnitudes first) for deterministic floating-point precision.
	var sum float64
	for i := len(probs) - 1; i >= 0; i-- {
		sum += probs[i]
	}
	// Compare against SumTolerance + 1e-12 for binary64 representation slack at
	// the 1e-3 boundary (negligible 1e-12 widening, not a wider contract or
	// provider-rounding accommodation).
	if math.Abs(sum-1.0) > SumTolerance+SumToleranceFloatSlack {
		return nil, fmt.Errorf("%w: sum = %v", ErrUnnormalizedDistribution, sum)
	}
	return probs, nil
}

func normEntropyFromSorted(sortedDesc []float64) float64 {
	var negSum float64
	for i := len(sortedDesc) - 1; i >= 0; i-- {
		p := sortedDesc[i]
		if p > 0 {
			negSum -= p * math.Log(p)
		}
	}
	h := negSum / math.Log(float64(len(sortedDesc)))
	if h <= 0 {
		return 0
	}
	if h > 1 {
		return 1
	}
	return h
}
