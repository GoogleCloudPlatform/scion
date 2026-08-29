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

package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// =============================================================================
// Three-state classification lookup tests (#127, P3a)
// =============================================================================

// TestClassifyEnvKey_PresentInMap verifies state 1: key present in a non-nil
// map returns its classification.
func TestClassifyEnvKey_PresentInMap(t *testing.T) {
	cls := map[string]EnvKind{
		"SCION_MODEL":    EnvKindPlain,
		"GITHUB_TOKEN":   EnvKindSecretInjected,
		"GEMINI_API_KEY": EnvKindSecretFetchable,
	}

	kind, ok := ClassifyEnvKey(cls, "SCION_MODEL")
	assert.True(t, ok)
	assert.Equal(t, EnvKindPlain, kind)

	kind, ok = ClassifyEnvKey(cls, "GITHUB_TOKEN")
	assert.True(t, ok)
	assert.Equal(t, EnvKindSecretInjected, kind)

	kind, ok = ClassifyEnvKey(cls, "GEMINI_API_KEY")
	assert.True(t, ok)
	assert.Equal(t, EnvKindSecretFetchable, kind)
}

// TestClassifyEnvKey_AbsentFromPresentMap verifies state 2: key absent from
// a non-nil map returns ("", false). The caller must treat this as
// unclassified → default secret + loud error.
func TestClassifyEnvKey_AbsentFromPresentMap(t *testing.T) {
	cls := map[string]EnvKind{
		"SCION_MODEL": EnvKindPlain,
	}

	kind, ok := ClassifyEnvKey(cls, "UNKNOWN_KEY")
	assert.False(t, ok, "absent key must return ok=false")
	assert.Equal(t, EnvKind(""), kind, "absent key must return zero EnvKind")
}

// TestClassifyEnvKey_NilMap verifies state 3: nil map returns ("", false).
// The caller MUST distinguish this from state 2 via a separate nil check.
// A nil map means "classification unavailable" (e.g. old hub), NOT
// "all keys are unclassified → all secret".
//
// This test exists because collapsing state 3 into state 2 produces a
// total agent-start outage on version skew: every key classifies as secret,
// the loud path fires for every variable, and in P3b every value becomes
// unfetchable. The distinction is the firewall against that.
func TestClassifyEnvKey_NilMap(t *testing.T) {
	kind, ok := ClassifyEnvKey(nil, "SCION_MODEL")
	assert.False(t, ok, "nil map must return ok=false")
	assert.Equal(t, EnvKind(""), kind, "nil map must return zero EnvKind")
}

// TestClassifyEnvKey_NilVsEmpty_Distinguishable verifies that nil and empty
// maps are distinguishable by the caller. This is the three-state invariant:
// nil means "unavailable", empty means "all classified, none present".
// The caller distinguishes them with a nil check BEFORE calling ClassifyEnvKey.
func TestClassifyEnvKey_NilVsEmpty_Distinguishable(t *testing.T) {
	var nilMap map[string]EnvKind
	emptyMap := map[string]EnvKind{}

	// Both return (zero, false) for a missing key...
	kind1, ok1 := ClassifyEnvKey(nilMap, "KEY")
	kind2, ok2 := ClassifyEnvKey(emptyMap, "KEY")
	assert.Equal(t, ok1, ok2, "both return false for missing key")
	assert.Equal(t, kind1, kind2, "both return zero kind for missing key")

	// ...but the caller distinguishes them with a nil check:
	assert.True(t, nilMap == nil, "nil map is nil")
	assert.False(t, emptyMap == nil, "empty map is NOT nil")

	// This is the contract: check nil FIRST, then ClassifyEnvKey.
	// If the map is nil → state 3 (unavailable). If non-nil → state 1 or 2.
}

// TestClassifyEnvKey_EmptyMapKeyAbsent verifies that an empty (non-nil)
// map with a lookup for any key returns state 2 (unclassified), not state 3.
func TestClassifyEnvKey_EmptyMapKeyAbsent(t *testing.T) {
	cls := map[string]EnvKind{} // non-nil, empty

	kind, ok := ClassifyEnvKey(cls, "ANY_KEY")
	assert.False(t, ok, "empty map + absent key = unclassified (state 2)")
	assert.Equal(t, EnvKind(""), kind)

	// Crucially, cls is NOT nil — this is state 2, not state 3.
	assert.NotNil(t, cls)
}

// TestEnvKind_Constants verifies the string values of the four classification
// kinds. These are wire values (JSON) and must not change without a migration.
func TestEnvKind_Constants(t *testing.T) {
	assert.Equal(t, EnvKind("plain"), EnvKindPlain)
	assert.Equal(t, EnvKind("secret-fetchable"), EnvKindSecretFetchable)
	assert.Equal(t, EnvKind("secret-injected"), EnvKindSecretInjected)
	assert.Equal(t, EnvKind("secret-bootstrap"), EnvKindSecretBootstrap)
}

// TestEnvKindSecretBootstrap_DistinctFromInjected verifies that secret-bootstrap
// is a distinct kind from secret-injected. P3b must handle them differently:
// secret-injected values need an alternative delivery channel;
// secret-bootstrap values need no routing at all, because they bootstrap
// the channel. Whether either reaches argv is a separate, per-key fact.
func TestEnvKindSecretBootstrap_DistinctFromInjected(t *testing.T) {
	assert.NotEqual(t, EnvKindSecretBootstrap, EnvKindSecretInjected,
		"secret-bootstrap must be distinct from secret-injected — "+
			"they have different P3b delivery instructions")
	assert.NotEqual(t, EnvKindSecretBootstrap, EnvKindPlain,
		"secret-bootstrap is a credential, not plain")
	assert.NotEqual(t, EnvKindSecretBootstrap, EnvKindSecretFetchable,
		"secret-bootstrap is not in the secret store")
}
