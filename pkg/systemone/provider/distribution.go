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

package provider

import (
	"fmt"
	"math"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
)

const (
	// SumTolerance is the allowed deviation from 1.0 when validating closed
	// probability distributions (provider-design §6.3, §7.4).
	SumTolerance = metrics.SumTolerance
	// SumToleranceFloatSlack is numerical slack (1e-12) for IEEE-754 binary64
	// representation at the existing SumTolerance (1e-3) boundary, widening the
	// accepted band by a negligible 1e-12 (mathematically non-zero, far below
	// reported probability precision) without widening the 1 ± 1e-3 contract or
	// accommodating provider rounding.
	SumToleranceFloatSlack = metrics.SumToleranceFloatSlack
	// TieTolerance is the top-two probability margin at or below which a binary
	// or closed-choice answer is classified as Uncertain("tie")
	// (provider-design §4.3, §6.3, §7.4).
	TieTolerance = 1e-9
)

// ValidateDistribution verifies that dist contains exact keys matching
// declaredIDs, every value is finite and in [0, 1], and the sum is within
// 1 ± SumTolerance (with 1e-12 binary64 boundary slack). It never repairs or
// renormalizes dist; any violation returns a *ProviderError with Reason
// FailMalformedResponse (provider-design §6.3).
func ValidateDistribution(dist map[string]float64, declaredIDs []string) error {
	if len(declaredIDs) < 2 {
		return NewProviderError(FailMalformedResponse, false, 0, 0, "declared option set has fewer than 2 options")
	}
	if len(dist) != len(declaredIDs) {
		return NewProviderError(
			FailMalformedResponse,
			false,
			0,
			0,
			fmt.Sprintf("distribution key count %d does not match declared count %d", len(dist), len(declaredIDs)),
		)
	}

	probs := make([]float64, 0, len(declaredIDs))
	seen := make(map[string]struct{}, len(declaredIDs))
	for _, id := range declaredIDs {
		if _, dup := seen[id]; dup {
			return NewProviderError(FailMalformedResponse, false, 0, 0, "duplicate declared option id")
		}
		seen[id] = struct{}{}
		p, ok := dist[id]
		if !ok {
			return NewProviderError(FailMalformedResponse, false, 0, 0, fmt.Sprintf("missing probability for declared option %q", id))
		}
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return NewProviderError(FailMalformedResponse, false, 0, 0, fmt.Sprintf("non-finite or out-of-range probability for option %q", id))
		}
		probs = append(probs, p)
	}

	sort.Float64s(probs)
	var sum float64
	for _, p := range probs {
		sum += p
	}
	// Compare against SumTolerance + 1e-12 for IEEE-754 binary64 representation
	// slack at the existing 1e-3 boundary (negligible 1e-12 widening; not a
	// wider contract or provider-rounding accommodation).
	if math.Abs(sum-1.0) > SumTolerance+SumToleranceFloatSlack {
		return NewProviderError(FailMalformedResponse, false, 0, 0, "distribution probabilities do not sum to 1 ± 1e-3")
	}
	return nil
}

// IsTie reports whether dist is tied at the top: its two largest probabilities
// differ by at most TieTolerance (1e-9), or dist has fewer than two finite
// options (provider-design §4.3, §6.3, §7.4). Binary and closed-choice answers
// share this single predicate across adapter decoding and runtime classification
// (for binary {"true": p, "false": 1 - p}, this is |2p - 1| <= 1e-9).
func IsTie(dist map[string]float64) bool {
	_, tied := ArgmaxChoice(dist)
	return tied
}

// ArgmaxChoice returns the unique highest-probability option key in dist, or
// ("", true) when dist is tied at the top within TieTolerance (1e-9).
// Ties are resolved deterministically regardless of Go map iteration order.
func ArgmaxChoice(dist map[string]float64) (choice string, tied bool) {
	if len(dist) < 2 {
		return "", true
	}
	var (
		bestKey   string
		bestProb  float64
		hasBest   bool
		second    float64
		hasSecond bool
	)
	for k, p := range dist {
		if math.IsNaN(p) || math.IsInf(p, 0) {
			return "", true
		}
		if !hasBest || p > bestProb || (p == bestProb && k < bestKey) {
			if hasBest {
				second = bestProb
				hasSecond = true
			}
			bestKey = k
			bestProb = p
			hasBest = true
			continue
		}
		if !hasSecond || p > second {
			second = p
			hasSecond = true
		}
	}
	if !hasSecond || bestProb-second <= TieTolerance {
		return "", true
	}
	return bestKey, false
}

// BinaryDistribution validates pTrue in [0, 1] and builds the canonical
// {"true": pTrue, "false": 1 - pTrue} distribution (provider-design §4.3, §6.3, §7.4).
// When IsTie(dist) holds (|2*pTrue - 1| <= TieTolerance), val is nil and tied is
// true; otherwise val points to (pTrue > 0.5).
func BinaryDistribution(pTrue float64) (dist map[string]float64, val *bool, tied bool, err error) {
	if math.IsNaN(pTrue) || math.IsInf(pTrue, 0) || pTrue < 0 || pTrue > 1 {
		return nil, nil, false, NewProviderError(FailMalformedResponse, false, 0, 0, "binary probability must be finite and in [0, 1]")
	}
	dist = map[string]float64{
		BinaryTrueKey:  pTrue,
		BinaryFalseKey: 1.0 - pTrue,
	}
	if IsTie(dist) {
		return dist, nil, true, nil
	}
	b := pTrue > 0.5
	return dist, &b, false, nil
}
