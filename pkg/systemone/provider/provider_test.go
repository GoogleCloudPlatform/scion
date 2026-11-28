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

package provider_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailReasons_ClosedSetMatchesRuntimeDesign(t *testing.T) {
	all := provider.AllFailReasons()
	require.Len(t, all, 18, "runtime-design §6.3 defines exactly 18 closed Failed reasons")

	expectedRetryableLater := map[provider.FailReason]bool{
		provider.FailUnavailable: true,
		provider.FailRateLimited: true,
		provider.FailCapacity:    true,
		provider.FailCircuitOpen: true,
		provider.FailTimeout:     true,
	}

	expectedFallback := map[provider.FailReason]bool{
		provider.FailTimeout:              true,
		provider.FailCapacity:             true,
		provider.FailCircuitOpen:          true,
		provider.FailUnavailable:          true,
		provider.FailRateLimited:          true,
		provider.FailAuth:                 true,
		provider.FailEgressRefused:        true,
		provider.FailUnsupported:          true,
		provider.FailModelMismatch:        true,
		provider.FailThresholdUnvalidated: true,
	}

	for _, r := range all {
		assert.True(t, r.Valid(), "reason %q should be valid", r)
		assert.Equal(t, expectedRetryableLater[r], r.RetryableLater(), "RetryableLater mismatch for %q", r)
		assert.Equal(t, expectedFallback[r], r.FallbackEligible(), "FallbackEligible mismatch for %q", r)
	}

	assert.False(t, provider.FailReason("unknown_reason").Valid())
	assert.False(t, provider.FailReason("").Valid())
}

func TestNotes_ClosedSet(t *testing.T) {
	notes := provider.AllNotes()
	require.Len(t, notes, 6)
	assert.Contains(t, notes, provider.NoteUsageUnreported)
	for _, n := range notes {
		assert.True(t, provider.IsValidNote(n), "expected %q to be valid closed note", n)
	}
	assert.False(t, provider.IsValidNote(""))
	assert.False(t, provider.IsValidNote("arbitrary_provider_note"))
	assert.False(t, provider.IsValidNote("model_unreported\n"))
}

func TestProviderError_CapsMessageAndUnwraps(t *testing.T) {
	longUTF8 := strings.Repeat("é", 200) // 400 bytes in UTF-8
	pe := provider.NewProviderError(provider.FailRateLimited, true, 2*time.Second, 429, longUTF8)
	assert.LessOrEqual(t, len(pe.Message), provider.MaxProviderErrorMessageBytes)
	assert.True(t, utf8.ValidString(pe.Message), "truncated message must remain valid UTF-8")
	assert.Equal(t, "4xx", pe.StatusClass())
	assert.Contains(t, pe.Error(), string(provider.FailRateLimited))

	wrappedPtr := fmt.Errorf("invoke failed: %w", pe)
	gotPtr, ok := provider.AsProviderError(wrappedPtr)
	require.True(t, ok)
	assert.Equal(t, provider.FailRateLimited, gotPtr.Reason)
	assert.True(t, gotPtr.Retryable)
	assert.Equal(t, 2*time.Second, gotPtr.RetryAfter)

	wrappedVal := fmt.Errorf("invoke value: %w", *pe)
	gotVal, ok := provider.AsProviderError(wrappedVal)
	require.True(t, ok)
	assert.Equal(t, provider.FailRateLimited, gotVal.Reason)
}

type stubAdapter struct {
	name           string
	version        string
	sideEffectFree bool
	capsErr        error
	models         []string
}

func (a *stubAdapter) Name() string    { return a.name }
func (a *stubAdapter) Version() string { return a.version }
func (a *stubAdapter) Capabilities(_ provider.InstanceView) (provider.Capabilities, error) {
	if a.capsErr != nil {
		return provider.Capabilities{}, a.capsErr
	}
	return provider.Capabilities{
		Primitives: map[string]provider.PrimitiveLimits{
			spec.PrimitiveBinary: {
				Supported:                true,
				DistributionCompleteness: provider.CompletenessComplete,
			},
			spec.PrimitiveChoice: {
				Supported:                true,
				MinOptions:               2,
				MaxOptions:               64,
				DistributionCompleteness: provider.CompletenessComplete,
			},
		},
		MaxQuestionsPerRequest: 64,
		MaxInputTokens:         32768,
		Truncation:             provider.TruncationNever,
		ReturnsDistribution:    true,
		SideEffectFree:         a.sideEffectFree,
		AuthScheme:             provider.AuthBearer,
	}, nil
}

func (a *stubAdapter) Validate(_ provider.InstanceView, _ provider.RuleSpec) error {
	return nil
}

func (a *stubAdapter) Invoke(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
	return provider.ModelResult{}, nil
}

func (a *stubAdapter) ListModels(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
	return a.models, nil
}

type stubValidatingAdapter struct {
	stubAdapter
}

func (a *stubValidatingAdapter) ValidateInstance(inst provider.InstanceView) error {
	if strings.TrimSpace(inst.Model) == "" {
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model is required")
	}
	return nil
}

func TestRegistry_RegisterLookupAndSideEffectFreeEnforcement(t *testing.T) {
	reg := provider.NewRegistry()

	// Rejects nil, missing name/version, Capabilities(InstanceView{}) errors, and SideEffectFree == false (runtime-design §4.3).
	require.Error(t, reg.Register(nil))
	require.Error(t, reg.Register(&stubAdapter{name: "", version: "0.1.0", sideEffectFree: true}))
	require.Error(t, reg.Register(&stubAdapter{name: "bad", version: "", sideEffectFree: true}))
	require.Error(t, reg.Register(&stubAdapter{
		name:           "caps-err",
		version:        "0.1.0",
		sideEffectFree: true,
		capsErr:        provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "capabilities must not fail on zero InstanceView"),
	}))
	err := reg.Register(&stubAdapter{name: "mutating", version: "0.1.0", sideEffectFree: false})
	require.ErrorIs(t, err, provider.ErrNotSideEffectFree)

	// Registers valid adapter and rejects duplicates.
	good := &stubAdapter{name: "typesafe-test", version: "0.1.0", sideEffectFree: true, models: []string{"jev-1.14.0"}}
	require.NoError(t, reg.Register(good))
	require.ErrorIs(t, reg.Register(good), provider.ErrDuplicateAdapter)

	// Adapters with static Capabilities(InstanceView{}) and optional InstanceValidator
	// register cleanly without synthetic InstanceView fabrication.
	modelReq := &stubValidatingAdapter{stubAdapter: stubAdapter{name: "gemini-test", version: "0.1.0", sideEffectFree: true}}
	require.NoError(t, reg.Register(modelReq))
	validator, hasValidator := provider.Adapter(modelReq).(provider.InstanceValidator)
	require.True(t, hasValidator)
	require.Error(t, validator.ValidateInstance(provider.InstanceView{}))
	require.NoError(t, validator.ValidateInstance(provider.InstanceView{Model: "gemini-decision-1"}))

	got, ok := reg.Get("typesafe-test")
	require.True(t, ok)
	assert.Equal(t, "typesafe-test", got.Name())
	assert.Equal(t, []string{"gemini-test", "typesafe-test"}, reg.Names())

	// Optional ModelLister assertion works on registered adapter.
	lister, ok := got.(provider.ModelLister)
	require.True(t, ok)
	models, err := lister.ListModels(context.Background(), nil, provider.InstanceView{Name: "inst-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"jev-1.14.0"}, models)

	// Optional EndpointDefaulter and InstanceValidator are absent unless explicitly implemented.
	_, hasDefaulter := got.(provider.EndpointDefaulter)
	assert.False(t, hasDefaulter)
	_, gotValidator := got.(provider.InstanceValidator)
	assert.False(t, gotValidator)

	// Capabilities.SpecLimits bridges directly into spec.ValidatePortable.
	caps, err := got.Capabilities(provider.InstanceView{})
	require.NoError(t, err)
	s := &spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{ID: "q1", Type: spec.PrimitiveBinary, Instructions: "Check."},
		},
	}
	require.NoError(t, spec.ValidatePortable(s, caps.SpecLimits()))
}

func TestDistributionAndTieHelpers(t *testing.T) {
	// Binary exact tie, near-0.5 tie boundary (uniform 1e-9 rule), and non-tie.
	distTie, valTie, tied, err := provider.BinaryDistribution(0.5)
	require.NoError(t, err)
	assert.True(t, tied)
	assert.True(t, provider.IsTie(distTie))
	assert.Nil(t, valTie)
	assert.Equal(t, map[string]float64{"true": 0.5, "false": 0.5}, distTie)

	// Inside 1e-9 margin: |2p - 1| <= 1e-9 is a tie (Bool == nil, tied == true),
	// including 0.5 ± 2e-10 and math.Nextafter(0.5+5e-10, 0) (the largest float64
	// above 0.5 whose binary64 margin p-(1-p) = 9.9999986e-10 <= 1e-9).
	for _, pNearTie := range []float64{
		0.5 + 2e-10,
		0.5 - 2e-10,
		0.5 + 4e-10,
		0.5 - 4e-10,
		math.Nextafter(0.5+5e-10, 0),
		math.Nextafter(0.5-5e-10, 1),
	} {
		distNear, valNear, nearTied, nearErr := provider.BinaryDistribution(pNearTie)
		require.NoError(t, nearErr, "p=%g", pNearTie)
		assert.True(t, nearTied, "expected tie for p=%.17g", pNearTie)
		assert.True(t, provider.IsTie(distNear), "expected IsTie=true for p=%.17g", pNearTie)
		assert.Nil(t, valNear, "expected nil Bool on tie for p=%.17g", pNearTie)
	}

	// Outside 1e-9 margin: 0.5 + 5e-10 rounds up in IEEE-754 binary64 by 0.37 ULP
	// so p-(1-p) = 1.000000082740371e-9 > 1e-9, and 0.5 ± 2e-9 has |2p-1| = 4e-9 > 1e-9.
	for _, tc := range []struct {
		p    float64
		want bool
	}{
		{p: 0.5 + 5e-10, want: true},
		{p: 0.5 - 5e-10, want: false},
		{p: 0.5 + 2e-9, want: true},
		{p: 0.5 - 2e-9, want: false},
	} {
		distClear, valClear, clearTied, clearErr := provider.BinaryDistribution(tc.p)
		require.NoError(t, clearErr, "p=%.17g", tc.p)
		assert.False(t, clearTied, "expected non-tie for p=%.17g", tc.p)
		assert.False(t, provider.IsTie(distClear), "expected IsTie=false for p=%.17g", tc.p)
		require.NotNil(t, valClear, "expected non-nil Bool for p=%.17g", tc.p)
		assert.Equal(t, tc.want, *valClear, "p=%.17g", tc.p)
	}

	distTrue, valTrue, tied, err := provider.BinaryDistribution(0.8)
	require.NoError(t, err)
	assert.False(t, tied)
	assert.False(t, provider.IsTie(distTrue))
	require.NotNil(t, valTrue)
	assert.True(t, *valTrue)
	assert.InDelta(t, 0.2, distTrue["false"], 1e-12)

	_, _, _, err = provider.BinaryDistribution(math.NaN())
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailMalformedResponse, pe.Reason)

	// Choice distribution validation & argmax/tie detection.
	declared := []string{"billing", "engineering", "cannot_tell"}
	validDist := map[string]float64{"billing": 0.15, "engineering": 0.80, "cannot_tell": 0.05}
	require.NoError(t, provider.ValidateDistribution(validDist, declared))
	choice, choiceTied := provider.ArgmaxChoice(validDist)
	assert.False(t, choiceTied)
	assert.False(t, provider.IsTie(validDist))
	assert.Equal(t, "engineering", choice)

	// Tie within 1e-9 at the top yields tied=true, whereas a tie for runner-up does not.
	tiedChoiceDist := map[string]float64{"billing": 0.5, "engineering": 0.5 - 5e-10, "cannot_tell": 5e-10}
	require.NoError(t, provider.ValidateDistribution(tiedChoiceDist, declared))
	choice, choiceTied = provider.ArgmaxChoice(tiedChoiceDist)
	assert.True(t, choiceTied)
	assert.True(t, provider.IsTie(tiedChoiceDist))
	assert.Empty(t, choice)

	runnerUpTieDist := map[string]float64{"billing": 0.30, "engineering": 0.40, "cannot_tell": 0.30}
	require.NoError(t, provider.ValidateDistribution(runnerUpTieDist, declared))
	choice, choiceTied = provider.ArgmaxChoice(runnerUpTieDist)
	assert.False(t, choiceTied)
	assert.False(t, provider.IsTie(runnerUpTieDist))
	assert.Equal(t, "engineering", choice)

	// Malformed distributions fail with FailMalformedResponse and never renormalize.
	malformed := []map[string]float64{
		{"billing": 0.5, "engineering": 0.5},                                 // missing key
		{"billing": 0.5, "engineering": 0.3, "cannot_tell": 0.2, "extra": 0}, // extra key
		{"billing": 0.5, "engineering": 0.3, "cannot_tell": 0.1},             // sum 0.9
		{"billing": math.NaN(), "engineering": 0.5, "cannot_tell": 0.5},      // NaN
		{"billing": -0.1, "engineering": 0.6, "cannot_tell": 0.5},            // negative
	}
	for _, bad := range malformed {
		err := provider.ValidateDistribution(bad, declared)
		require.Error(t, err)
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailMalformedResponse, pe.Reason)
	}
}

func TestValidateDistribution_SumToleranceFloatSlackBoundary(t *testing.T) {
	// Synthetic from docs (ai.google.dev/gemini-api/docs/decisions-api):
	// Gemini's documented 4-option "department" choice example has exact decimal
	// sum 0.999, whose IEEE-754 binary64 sum (0.002+0.003+0.004+0.99) has
	// |sum - 1| = 0.0010000000000000009 > float64(1e-3). The 1e-12 numerical
	// float slack at the 1e-3 boundary accepts this without renormalization.
	deptIDs := []string{"billing", "sales", "support", "other"}
	docExample := map[string]float64{
		"support": 0.99,
		"billing": 0.004,
		"sales":   0.003,
		"other":   0.002,
	}
	require.NoError(t, provider.ValidateDistribution(docExample, deptIDs))
	// Confirm ValidateDistribution never mutates or renormalizes the map.
	assert.Equal(t, 0.99, docExample["support"])
	assert.Equal(t, 0.004, docExample["billing"])
	assert.Equal(t, 0.003, docExample["sales"])
	assert.Equal(t, 0.002, docExample["other"])

	// Upper 1e-3 boundary with symmetric binary64 representation also passes.
	upperBoundary := map[string]float64{
		"support": 0.99,
		"billing": 0.005,
		"sales":   0.003,
		"other":   0.003, // decimal sum 1.001
	}
	require.NoError(t, provider.ValidateDistribution(upperBoundary, deptIDs))

	// Sums just outside 1 ± (1e-3 + 1e-12), materially outside 1 ± 1e-3,
	// out-of-bounds [0, 1] values, and non-finite values still fail as
	// malformed_response and are never renormalized.
	twoIDs := []string{"a", "b"}
	rejectCases := []struct {
		name string
		ids  []string
		dist map[string]float64
	}{
		{
			name: "just_below_lower_boundary_1_minus_1e3_minus_1e9",
			ids:  twoIDs,
			dist: map[string]float64{"a": 0.5, "b": 0.5 - (1e-3 + 1e-9)},
		},
		{
			name: "just_above_upper_boundary_1_plus_1e3_plus_1e9",
			ids:  twoIDs,
			dist: map[string]float64{"a": 0.5, "b": 0.5 + (1e-3 + 1e-9)},
		},
		{
			name: "materially_below_sum_0_98",
			ids:  deptIDs,
			dist: map[string]float64{"support": 0.90, "billing": 0.04, "sales": 0.02, "other": 0.02},
		},
		{
			name: "materially_above_sum_1_02",
			ids:  deptIDs,
			dist: map[string]float64{"support": 0.90, "billing": 0.06, "sales": 0.03, "other": 0.03},
		},
		{
			name: "negative_within_sum_band",
			ids:  twoIDs,
			dist: map[string]float64{"a": 1.0, "b": -1e-12},
		},
		{
			name: "above_one_within_sum_band",
			ids:  twoIDs,
			dist: map[string]float64{"a": 1.0 + 1e-12, "b": 0.0},
		},
		{
			name: "nan_value",
			ids:  twoIDs,
			dist: map[string]float64{"a": math.NaN(), "b": 1.0},
		},
		{
			name: "pos_inf_value",
			ids:  twoIDs,
			dist: map[string]float64{"a": math.Inf(1), "b": 0.0},
		},
		{
			name: "neg_inf_value",
			ids:  twoIDs,
			dist: map[string]float64{"a": math.Inf(-1), "b": 1.0},
		},
	}

	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.ValidateDistribution(tc.dist, tc.ids)
			require.Error(t, err)
			pe, ok := provider.AsProviderError(err)
			require.True(t, ok)
			assert.Equal(t, provider.FailMalformedResponse, pe.Reason)
		})
	}
}

type catalogAdapter struct {
	stubAdapter
	models []provider.ModelDecl
}

func (c *catalogAdapter) KnownModels() []provider.ModelDecl {
	return c.models
}

func TestKnownModelsFor_FilteringDeduplicationAndDefensiveCopy(t *testing.T) {
	assert.Nil(t, provider.KnownModelsFor(nil))
	assert.Nil(t, provider.KnownModelsFor(&stubAdapter{name: "no-catalog"}))
	assert.Nil(t, provider.KnownModelsFor(&catalogAdapter{stubAdapter: stubAdapter{name: "empty"}}))

	src := []provider.ModelDecl{
		{ID: "  model-a  ", DisplayName: " Model A ", Source: provider.ModelSourceProviderDoc},
		{ID: "", DisplayName: "Blank", Source: provider.ModelSourceProviderDoc},
		{ID: "   ", DisplayName: "Spaces", Source: provider.ModelSourceProviderDoc},
		{ID: "bad\nid", DisplayName: "Bad ID", Source: provider.ModelSourceProviderDoc},
		{ID: "bad-name", DisplayName: "Bad\x00Name", Source: provider.ModelSourceProviderDoc},
		{ID: "bad-src", DisplayName: "Bad Src", Source: "provider\rdoc"},
		{ID: "model-a", DisplayName: "Duplicate A", Source: provider.ModelSourceProviderDoc},
		{ID: "model-b", Source: provider.ModelSourceProviderDoc},
	}
	a := &catalogAdapter{stubAdapter: stubAdapter{name: "with-catalog"}, models: src}

	got := provider.KnownModelsFor(a)
	require.Equal(t, []provider.ModelDecl{
		{ID: "model-a", DisplayName: "Model A", Source: provider.ModelSourceProviderDoc},
		{ID: "model-b", DisplayName: "", Source: provider.ModelSourceProviderDoc},
	}, got)

	// Mutating the returned slice does not affect subsequent calls.
	got[0].ID = "mutated"
	gotAgain := provider.KnownModelsFor(a)
	require.Len(t, gotAgain, 2)
	assert.Equal(t, "model-a", gotAgain[0].ID)
}
