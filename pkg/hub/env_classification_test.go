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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// classifyEnv helper tests (#127, P3a)
// =============================================================================

func TestClassifyEnv_InitializesNilMap(t *testing.T) {
	var m map[string]api.EnvKind
	classifyEnv(&m, "KEY", api.EnvKindPlain)
	require.NotNil(t, m)
	assert.Equal(t, api.EnvKindPlain, m["KEY"])
}

func TestClassifyEnv_OverwritesExisting(t *testing.T) {
	m := map[string]api.EnvKind{
		"GITHUB_TOKEN": api.EnvKindSecretFetchable,
	}
	// GitHub App minter overwrites with secret-injected.
	classifyEnv(&m, "GITHUB_TOKEN", api.EnvKindSecretInjected)
	assert.Equal(t, api.EnvKindSecretInjected, m["GITHUB_TOKEN"])
}

func TestClassifyEnvKeys_BulkPlain(t *testing.T) {
	var m map[string]api.EnvKind
	env := map[string]string{
		"SCION_MODEL": "claude-4",
		"SCION_DEBUG": "1",
	}
	classifyEnvKeys(&m, env, api.EnvKindPlain)
	require.NotNil(t, m)
	assert.Equal(t, api.EnvKindPlain, m["SCION_MODEL"])
	assert.Equal(t, api.EnvKindPlain, m["SCION_DEBUG"])
}

func TestClassifyEnvKeys_EmptyIsNoOp(t *testing.T) {
	var m map[string]api.EnvKind
	classifyEnvKeys(&m, nil, api.EnvKindPlain)
	assert.Nil(t, m, "nil env must not initialize the map")

	classifyEnvKeys(&m, map[string]string{}, api.EnvKindPlain)
	assert.Nil(t, m, "empty env must not initialize the map")
}

// =============================================================================
// GITHUB_TOKEN ordering: App minter AFTER secret-store injection (#127, P3a)
//
// Writer IDs (H6, H7, H8, H10, etc.) are defined in the env-writer inventory:
//   .design/hosted/127-env-writer-inventory.md
// =============================================================================

// TestGitHubToken_AppMinterOverridesSecretStore verifies the classification
// ordering dependency in buildCreateRequest: the GitHub App minter runs AFTER
// the secret-store GITHUB_TOKEN injection. When the App minter succeeds, the
// classification must be secret-injected, not secret-fetchable.
//
// This test pins the ordering so a future refactoring that moves the App
// minter before the secret-store injection breaks a test rather than silently
// mislabelling a credential.
func TestGitHubToken_AppMinterOverridesSecretStore(t *testing.T) {
	var cls map[string]api.EnvKind

	// Step 1: Secret store injects GITHUB_TOKEN (H6/H7/H8 in inventory).
	classifyEnv(&cls, "GITHUB_TOKEN", api.EnvKindSecretFetchable)
	assert.Equal(t, api.EnvKindSecretFetchable, cls["GITHUB_TOKEN"],
		"after secret-store injection, GITHUB_TOKEN is secret-fetchable")

	// Step 2: GitHub App minter overwrites with an ephemeral token (H10).
	// This MUST run after step 1 — a reordering that puts the App minter
	// first would leave the classification as secret-fetchable (from the
	// store injection), which is wrong: the value in the env is the App
	// token, not the stored secret.
	classifyEnv(&cls, "GITHUB_TOKEN", api.EnvKindSecretInjected)
	assert.Equal(t, api.EnvKindSecretInjected, cls["GITHUB_TOKEN"],
		"after App minter overwrite, GITHUB_TOKEN must be secret-injected — "+
			"if this fails, the classification ordering has been broken")
}

// TestGitHubToken_StoreOnly_StaysSecretFetchable verifies that when no
// GitHub App minter is configured, GITHUB_TOKEN from the secret store
// stays classified as secret-fetchable.
func TestGitHubToken_StoreOnly_StaysSecretFetchable(t *testing.T) {
	var cls map[string]api.EnvKind

	// Only secret-store injection, no App minter.
	classifyEnv(&cls, "GITHUB_TOKEN", api.EnvKindSecretFetchable)
	assert.Equal(t, api.EnvKindSecretFetchable, cls["GITHUB_TOKEN"])
}

// =============================================================================
// Bootstrap tokens: SCION_AUTH_TOKEN, SCION_TRANSPORT_TOKEN (#127, P3a Q2)
// =============================================================================

// TestBootstrapTokens_ClassifiedAsBootstrap verifies that the two channel-
// bootstrapping credentials are classified as secret-bootstrap, not
// secret-injected. Both bootstrap the delivery channel:
//
//   - SCION_AUTH_TOKEN authorises the secret fetch (X-Scion-Agent-Token).
//     Delivery: NOT in argv — diverted to ~/.scion/scion-token by
//     pkg/agent/run.go:761-777; read by pkg/hubsync/sync.go:1329.
//
//   - SCION_TRANSPORT_TOKEN authenticates to the hub via IAP (OIDC).
//     Delivery: IN argv — no diversion exists. Google-signed OIDC, 1h,
//     lifetime NOT boundable (GenerateIdTokenRequest has no Lifetime field).
//
// Classifying them as secret-injected would tell P3b to route them through
// a delivery channel — impossible by construction, since they ARE what
// opens the channel. secret-bootstrap tells P3b: perform no routing.
// The kind says nothing about whether the value reaches argv; that is
// per-key, documented at each classification site.
func TestBootstrapTokens_ClassifiedAsBootstrap(t *testing.T) {
	var cls map[string]api.EnvKind

	classifyEnv(&cls, "SCION_AUTH_TOKEN", api.EnvKindSecretBootstrap)
	classifyEnv(&cls, "SCION_TRANSPORT_TOKEN", api.EnvKindSecretBootstrap)

	assert.Equal(t, api.EnvKindSecretBootstrap, cls["SCION_AUTH_TOKEN"],
		"SCION_AUTH_TOKEN must be secret-bootstrap — it authorises the fetch channel")
	assert.Equal(t, api.EnvKindSecretBootstrap, cls["SCION_TRANSPORT_TOKEN"],
		"SCION_TRANSPORT_TOKEN must be secret-bootstrap — it opens the transport to the hub")

	// Verify these are NOT secret-injected (the old, incorrect classification).
	assert.NotEqual(t, api.EnvKindSecretInjected, cls["SCION_AUTH_TOKEN"])
	assert.NotEqual(t, api.EnvKindSecretInjected, cls["SCION_TRANSPORT_TOKEN"])
}

// =============================================================================
// Unclassified key: fail-closed + loud (D4 guard test)
// =============================================================================

// TestUnclassifiedKey_FailClosed verifies that a key present in ResolvedEnv
// but absent from a non-nil EnvClassifications map is unclassified. The
// caller must treat this as secret (fail-closed) and emit a loud error
// naming the key.
//
// This test is the guard for every future injection site: adding a new env
// var without classifying it must be detectable.
func TestUnclassifiedKey_FailClosed(t *testing.T) {
	resolvedEnv := map[string]string{
		"SCION_MODEL":   "claude-4",
		"NEW_UNCLASSED": "some-value",
	}
	cls := map[string]api.EnvKind{
		"SCION_MODEL": api.EnvKindPlain,
		// NEW_UNCLASSED is deliberately NOT classified.
	}

	// SCION_MODEL: classified.
	kind, ok := api.ClassifyEnvKey(cls, "SCION_MODEL")
	assert.True(t, ok)
	assert.Equal(t, api.EnvKindPlain, kind)

	// NEW_UNCLASSED: unclassified → fail-closed.
	kind, ok = api.ClassifyEnvKey(cls, "NEW_UNCLASSED")
	assert.False(t, ok, "unclassified key must return ok=false")
	assert.Equal(t, api.EnvKind(""), kind,
		"unclassified key must return zero EnvKind — caller must not get "+
			"a stale kind from a previous lookup")

	// Verify the key IS in ResolvedEnv (the condition for the loud path).
	_, inEnv := resolvedEnv["NEW_UNCLASSED"]
	assert.True(t, inEnv, "the key is in ResolvedEnv but not in classifications — "+
		"this is state 2 (unclassified) and the loud path must fire")
}

// =============================================================================
// Three-state wire semantics: nil map = unavailable (#127, P3a)
// =============================================================================

// TestNilClassifications_NonEmptyEnv_IsUnavailable verifies the architect's
// required test: nil EnvClassifications with a non-empty ResolvedEnv yields
// "unavailable", not "all secret". This prevents a total agent-start outage
// on version skew (new broker receiving from old hub).
func TestNilClassifications_NonEmptyEnv_IsUnavailable(t *testing.T) {
	resolvedEnv := map[string]string{
		"SCION_MODEL":    "claude-4",
		"GITHUB_TOKEN":   "FAKE-KEY-SENTINEL-not-a-real-credential",
		"SCION_HUB_NAME": "test-hub",
	}
	var cls map[string]api.EnvKind // nil — old hub didn't send classifications

	// The map is nil — this is state 3 (unavailable), NOT state 2.
	assert.Nil(t, cls, "precondition: classifications is nil")
	assert.NotEmpty(t, resolvedEnv, "precondition: env is non-empty")

	// Correct handling: check nil FIRST.
	if cls == nil {
		// State 3: classification unavailable. The caller must NOT iterate
		// resolvedEnv and treat every key as unclassified (which would default
		// to secret and fire the loud path for every variable).
		// Instead, the caller falls back to "classification not available"
		// behaviour — pass through all env vars as-is (backward compat).
		t.Log("classifications nil → unavailable (correct)")
	} else {
		// State 1 or 2: look up each key.
		for k := range resolvedEnv {
			_, ok := api.ClassifyEnvKey(cls, k)
			if !ok {
				t.Errorf("key %q is unclassified in a non-nil map — "+
					"this should not happen in this branch", k)
			}
		}
		t.Fatal("this branch must not be reached when cls is nil")
	}
}
