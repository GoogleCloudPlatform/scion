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
	// TieTolerance is the top-two probability margin at or below which a closed
	// choice answer is classified as Uncertain("tie") (provider-design §4.3, §7.4).
	TieTolerance = 1e-9
)

// ValidateDistribution verifies that dist contains exact keys matching
// declaredIDs, every value is finite and in [0, 1], and the sum is within
// 1 ± SumTolerance. It never repairs or renormalizes dist; any violation returns
// a *ProviderError with Reason FailMalformedResponse (provider-design §6.3).
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
	if math.Abs(sum-1.0) > SumTolerance {
		return NewProviderError(FailMalformedResponse, false, 0, 0, "distribution probabilities do not sum to 1 ± 1e-3")
	}
	return nil
}

// ArgmaxChoice returns the unique highest-probability option key in dist, or
// ("", true) when the top two options are tied within TieTolerance (1e-9).
// Ties are resolved deterministically regardless of Go map iteration order.
func ArgmaxChoice(dist map[string]float64) (choice string, tied bool) {
	if len(dist) < 2 {
		return "", true
	}
	type kv struct {
		key  string
		prob float64
	}
	items := make([]kv, 0, len(dist))
	for k, p := range dist {
		items = append(items, kv{key: k, prob: p})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].prob != items[j].prob {
			return items[i].prob > items[j].prob
		}
		return items[i].key < items[j].key
	})
	if items[0].prob-items[1].prob <= TieTolerance {
		return "", true
	}
	return items[0].key, false
}

// BinaryDistribution validates pTrue in [0, 1] and builds the canonical
// {"true": pTrue, "false": 1 - pTrue} distribution (provider-design §4.3, §7.4).
// When pTrue == 0.5 exactly, val is nil and tied is true.
func BinaryDistribution(pTrue float64) (dist map[string]float64, val *bool, tied bool, err error) {
	if math.IsNaN(pTrue) || math.IsInf(pTrue, 0) || pTrue < 0 || pTrue > 1 {
		return nil, nil, false, NewProviderError(FailMalformedResponse, false, 0, 0, "binary probability must be finite and in [0, 1]")
	}
	dist = map[string]float64{
		BinaryTrueKey:  pTrue,
		BinaryFalseKey: 1.0 - pTrue,
	}
	if pTrue == 0.5 {
		return dist, nil, true, nil
	}
	b := pTrue > 0.5
	return dist, &b, false, nil
}
