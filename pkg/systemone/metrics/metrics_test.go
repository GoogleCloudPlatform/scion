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

package metrics_test

import (
	"math"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompute_GoldenDistributions(t *testing.T) {
	const eps = 1e-12

	tests := []struct {
		name        string
		dist        map[string]float64
		wantPTop    float64
		wantMargin  float64
		wantEntropy float64
	}{
		{
			name:        "uniform_binary",
			dist:        map[string]float64{"true": 0.5, "false": 0.5},
			wantPTop:    0.5,
			wantMargin:  0.0,
			wantEntropy: 1.0,
		},
		{
			name:        "deterministic_binary",
			dist:        map[string]float64{"true": 1.0, "false": 0.0},
			wantPTop:    1.0,
			wantMargin:  1.0,
			wantEntropy: 0.0,
		},
		{
			name:        "skewed_binary_80_20",
			dist:        map[string]float64{"true": 0.8, "false": 0.2},
			wantPTop:    0.8,
			wantMargin:  0.6,
			wantEntropy: 0.7219280948873623, // -(0.8*ln(0.8) + 0.2*ln(0.2)) / ln(2)
		},
		{
			name:        "typesafe_openapi_choice_f2",
			dist:        map[string]float64{"billing": 0.15, "engineering": 0.80, "cannot_tell": 0.05},
			wantPTop:    0.80,
			wantMargin:  0.65,
			wantEntropy: 0.5578578164321781, // -(0.80*ln(0.80) + 0.15*ln(0.15) + 0.05*ln(0.05)) / ln(3)
		},
		{
			name:        "dyadic_four_choice_exact_7_8ths",
			dist:        map[string]float64{"a": 0.5, "b": 0.25, "c": 0.125, "d": 0.125},
			wantPTop:    0.5,
			wantMargin:  0.25,
			wantEntropy: 0.875, // (0.5*1 + 0.25*2 + 2*0.125*3) / log2(4) = 1.75 / 2 = 0.875
		},
		{
			name:        "uniform_four_choice",
			dist:        map[string]float64{"a": 0.25, "b": 0.25, "c": 0.25, "d": 0.25},
			wantPTop:    0.25,
			wantMargin:  0.0,
			wantEntropy: 1.0,
		},
		{
			name:        "tied_top_two_of_three",
			dist:        map[string]float64{"a": 0.45, "b": 0.45, "c": 0.10},
			wantPTop:    0.45,
			wantMargin:  0.0,
			wantEntropy: 0.8637400525036962, // -(2*0.45*ln(0.45) + 0.10*ln(0.10)) / ln(3)
		},
		{
			name:        "within_sum_tolerance_0999",
			dist:        map[string]float64{"a": 0.333, "b": 0.333, "c": 0.333},
			wantPTop:    0.333,
			wantMargin:  0.0,
			wantEntropy: -(3 * 0.333 * math.Log(0.333)) / math.Log(3),
		},
		{
			name:        "gemini_doc_choice_example_float_boundary_0999",
			dist:        map[string]float64{"support": 0.99, "billing": 0.004, "sales": 0.003, "other": 0.002},
			wantPTop:    0.99,
			wantMargin:  0.99 - 0.004,
			wantEntropy: -(0.002*math.Log(0.002) + 0.003*math.Log(0.003) + 0.004*math.Log(0.004) + 0.99*math.Log(0.99)) / math.Log(4),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := metrics.Compute(tc.dist)
			require.NoError(t, err)
			require.Len(t, got, 3)

			pTop := got[metrics.NamePTop]
			assert.InDelta(t, tc.wantPTop, pTop.Value, eps)
			assert.Equal(t, metrics.UnitProbability, pTop.Unit)
			assert.Equal(t, 0.0, pTop.Min)
			assert.Equal(t, 1.0, pTop.Max)
			assert.Equal(t, metrics.SourceScion, pTop.Source)

			margin := got[metrics.NameMargin]
			assert.InDelta(t, tc.wantMargin, margin.Value, eps)
			assert.Equal(t, metrics.UnitProbability, margin.Unit)
			assert.Equal(t, 0.0, margin.Min)
			assert.Equal(t, 1.0, margin.Max)
			assert.Equal(t, metrics.SourceScion, margin.Source)

			ent := got[metrics.NameNormEntropy]
			assert.InDelta(t, tc.wantEntropy, ent.Value, eps)
			assert.False(t, math.Signbit(ent.Value), "norm_entropy must not be -0.0")
			assert.GreaterOrEqual(t, ent.Value, 0.0)
			assert.LessOrEqual(t, ent.Value, 1.0)
			assert.Equal(t, metrics.UnitFraction, ent.Unit)
			assert.Equal(t, 0.0, ent.Min)
			assert.Equal(t, 1.0, ent.Max)
			assert.Equal(t, metrics.SourceScion, ent.Source)

			// Individual metric helpers match Compute exactly.
			vTop, err := metrics.PTop(tc.dist)
			require.NoError(t, err)
			assert.Equal(t, pTop.Value, vTop)

			vMargin, err := metrics.Margin(tc.dist)
			require.NoError(t, err)
			assert.Equal(t, margin.Value, vMargin)

			vEnt, err := metrics.NormEntropy(tc.dist)
			require.NoError(t, err)
			assert.Equal(t, ent.Value, vEnt)
		})
	}
}

func TestCompute_UpperToleranceClampsOvershootToOne(t *testing.T) {
	// Sum = 1.0008 (within 1 ± 1e-3); raw -3*0.3336*ln(0.3336)/ln(3) slightly exceeds 1.0.
	dist := map[string]float64{"a": 0.3336, "b": 0.3336, "c": 0.3336}
	got, err := metrics.Compute(dist)
	require.NoError(t, err)
	assert.LessOrEqual(t, got[metrics.NameNormEntropy].Value, 1.0)
}

func TestCompute_BitExactDeterminismAcrossMapOrder(t *testing.T) {
	dist := map[string]float64{
		"opt_a": 0.31,
		"opt_b": 0.27,
		"opt_c": 0.19,
		"opt_d": 0.13,
		"opt_e": 0.07,
		"opt_f": 0.03,
	}
	first, err := metrics.Compute(dist)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		next, err := metrics.Compute(dist)
		require.NoError(t, err)
		assert.Equal(t, first, next)
	}
}

func TestCompute_RejectsInvalidDistributions(t *testing.T) {
	tests := []struct {
		name string
		dist map[string]float64
	}{
		{name: "nil", dist: nil},
		{name: "empty", dist: map[string]float64{}},
		{name: "single_option", dist: map[string]float64{"only": 1.0}},
		{name: "nan", dist: map[string]float64{"a": math.NaN(), "b": 0.5}},
		{name: "pos_inf", dist: map[string]float64{"a": math.Inf(1), "b": 0.0}},
		{name: "neg_inf", dist: map[string]float64{"a": math.Inf(-1), "b": 1.0}},
		{name: "negative_prob", dist: map[string]float64{"a": -0.01, "b": 1.01}},
		{name: "prob_above_one", dist: map[string]float64{"a": 1.0005, "b": 0.0}},
		{name: "sum_too_low", dist: map[string]float64{"a": 0.45, "b": 0.45}},
		{name: "sum_too_high", dist: map[string]float64{"a": 0.501, "b": 0.501}},
		{name: "sum_0_98", dist: map[string]float64{"a": 0.90, "b": 0.08}},
		{name: "sum_just_below_slack_band", dist: map[string]float64{"a": 0.5, "b": 0.5 - (1e-3 + 1e-9)}},
		{name: "sum_just_above_slack_band", dist: map[string]float64{"a": 0.5, "b": 0.5 + (1e-3 + 1e-9)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := metrics.Compute(tc.dist)
			assert.Error(t, err)
			_, err = metrics.PTop(tc.dist)
			assert.Error(t, err)
			_, err = metrics.Margin(tc.dist)
			assert.Error(t, err)
			_, err = metrics.NormEntropy(tc.dist)
			assert.Error(t, err)
		})
	}
}

func TestIsPortableMetric(t *testing.T) {
	assert.True(t, metrics.IsPortableMetric("p_top"))
	assert.True(t, metrics.IsPortableMetric("margin"))
	assert.True(t, metrics.IsPortableMetric("norm_entropy"))
	assert.False(t, metrics.IsPortableMetric("typesafe.confidence"))
	assert.False(t, metrics.IsPortableMetric("confidence"))
	assert.False(t, metrics.IsPortableMetric(""))
}
