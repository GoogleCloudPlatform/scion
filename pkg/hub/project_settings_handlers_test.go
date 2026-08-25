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

//go:build !no_sqlite

package hub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectSettings_GetEmpty(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var settings hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&settings))
	assert.Empty(t, settings.DefaultTemplate)
	assert.Empty(t, settings.DefaultHarnessConfig)
	assert.Nil(t, settings.TelemetryEnabled)
}

func TestProjectSettings_PutAndGet(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	telemetry := true
	putBody := hubclient.ProjectSettings{
		DefaultTemplate:      "my-template",
		DefaultHarnessConfig: "claude-default",
		TelemetryEnabled:     &telemetry,
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "my-template", putResp.DefaultTemplate)
	assert.Equal(t, "claude-default", putResp.DefaultHarnessConfig)
	require.NotNil(t, putResp.TelemetryEnabled)
	assert.True(t, *putResp.TelemetryEnabled)

	// GET should return persisted values
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "my-template", getResp.DefaultTemplate)
	assert.Equal(t, "claude-default", getResp.DefaultHarnessConfig)
	require.NotNil(t, getResp.TelemetryEnabled)
	assert.True(t, *getResp.TelemetryEnabled)
}

func TestProjectSettings_ClearValues(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Set values first
	telemetry := true
	putBody := hubclient.ProjectSettings{
		DefaultTemplate:      "my-template",
		DefaultHarnessConfig: "claude-default",
		TelemetryEnabled:     &telemetry,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code)

	// Clear by sending empty values
	clearBody := hubclient.ProjectSettings{}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", clearBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.DefaultTemplate)
	assert.Empty(t, resp.DefaultHarnessConfig)
	assert.Nil(t, resp.TelemetryEnabled)
}

func TestProjectSettings_DefaultLimits(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		DefaultMaxTurns:      100,
		DefaultMaxModelCalls: 500,
		DefaultMaxDuration:   "2h",
		DefaultResources: &hubclient.ProjectResourceSpec{
			Requests: &hubclient.ProjectResourceList{CPU: "500m", Memory: "1Gi"},
			Limits:   &hubclient.ProjectResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "10Gi",
		},
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, 100, putResp.DefaultMaxTurns)
	assert.Equal(t, 500, putResp.DefaultMaxModelCalls)
	assert.Equal(t, "2h", putResp.DefaultMaxDuration)
	require.NotNil(t, putResp.DefaultResources)
	require.NotNil(t, putResp.DefaultResources.Requests)
	assert.Equal(t, "500m", putResp.DefaultResources.Requests.CPU)
	assert.Equal(t, "1Gi", putResp.DefaultResources.Requests.Memory)
	require.NotNil(t, putResp.DefaultResources.Limits)
	assert.Equal(t, "2", putResp.DefaultResources.Limits.CPU)
	assert.Equal(t, "4Gi", putResp.DefaultResources.Limits.Memory)
	assert.Equal(t, "10Gi", putResp.DefaultResources.Disk)

	// GET should return persisted values
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, 100, getResp.DefaultMaxTurns)
	assert.Equal(t, 500, getResp.DefaultMaxModelCalls)
	assert.Equal(t, "2h", getResp.DefaultMaxDuration)
	require.NotNil(t, getResp.DefaultResources)
	assert.Equal(t, "10Gi", getResp.DefaultResources.Disk)
}

func TestProjectSettings_ClearDefaultLimits(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Set values first
	putBody := hubclient.ProjectSettings{
		DefaultMaxTurns:      100,
		DefaultMaxModelCalls: 500,
		DefaultMaxDuration:   "2h",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code)

	// Clear by sending zero/empty values
	clearBody := hubclient.ProjectSettings{}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", clearBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.DefaultMaxTurns)
	assert.Equal(t, 0, resp.DefaultMaxModelCalls)
	assert.Empty(t, resp.DefaultMaxDuration)
	assert.Nil(t, resp.DefaultResources)
}

func TestApplyProjectDefaults_HarnessConfig(t *testing.T) {
	t.Run("applies default harness config when empty", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-harness-config": "claude-default",
			},
		}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "claude-default", ac.HarnessConfig)
	})

	t.Run("does not override explicit harness config", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-harness-config": "claude-default",
			},
		}
		ac := &store.AgentAppliedConfig{HarnessConfig: "custom-config"}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "custom-config", ac.HarnessConfig)
	})

	t.Run("nil project is safe", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, nil)
		assert.Empty(t, ac.HarnessConfig)
	})

	t.Run("nil annotations is safe", func(t *testing.T) {
		project := &store.Project{}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Empty(t, ac.HarnessConfig)
	})
}

func TestApplyProjectDefaults_HarnessAuth(t *testing.T) {
	t.Run("applies default harness auth when empty", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-harness-auth": "vertex-ai",
			},
		}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "vertex-ai", ac.HarnessAuth)
	})

	t.Run("does not override explicit harness auth", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-harness-auth": "vertex-ai",
			},
		}
		ac := &store.AgentAppliedConfig{HarnessAuth: "api-key"}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "api-key", ac.HarnessAuth)
	})

	t.Run("nil project is safe", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, nil)
		assert.Empty(t, ac.HarnessAuth)
	})

	t.Run("nil annotations is safe", func(t *testing.T) {
		project := &store.Project{}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Empty(t, ac.HarnessAuth)
	})

	t.Run("no default set leaves auth empty", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{},
		}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Empty(t, ac.HarnessAuth)
	})
}

// TestProjectSettings_DefaultHarnessAuth_RoundTrip verifies the annotation
// round-trip: PUT stores the value, GET returns it, empty PUT clears it.
func TestProjectSettings_DefaultHarnessAuth_RoundTrip(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		DefaultHarnessAuth: "vertex-ai",
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "vertex-ai", putResp.DefaultHarnessAuth)

	// GET should return persisted value
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "vertex-ai", getResp.DefaultHarnessAuth)

	// Clear by sending empty value
	clearBody := hubclient.ProjectSettings{}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", clearBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var clearResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clearResp))
	assert.Empty(t, clearResp.DefaultHarnessAuth)
}

// TestHarnessAuth_Precedence_ProjectBeatsHub verifies the full precedence chain:
// project default beats hub default when per-agent is empty.
func TestHarnessAuth_Precedence_ProjectBeatsHub(t *testing.T) {
	project := &store.Project{
		Annotations: map[string]string{
			"scion.io/default-harness-auth": "oauth-token",
		},
	}

	ac := &store.AgentAppliedConfig{}

	// Step 1: project defaults fill first
	applyProjectDefaults(ac, project)
	assert.Equal(t, "oauth-token", ac.HarnessAuth,
		"project default should fill empty HarnessAuth")

	// Step 2: hub defaults run after — should NOT override the project value
	applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
		DefaultHarnessAuth: "vertex-ai",
	})
	assert.Equal(t, "oauth-token", ac.HarnessAuth,
		"hub default must not override project default")
}

// TestHarnessAuth_Precedence_ExplicitBeatsAll verifies that an explicit per-agent
// harnessAuth beats both project and hub defaults.
func TestHarnessAuth_Precedence_ExplicitBeatsAll(t *testing.T) {
	project := &store.Project{
		Annotations: map[string]string{
			"scion.io/default-harness-auth": "oauth-token",
		},
	}

	ac := &store.AgentAppliedConfig{HarnessAuth: "api-key"}

	applyProjectDefaults(ac, project)
	assert.Equal(t, "api-key", ac.HarnessAuth,
		"explicit per-agent should beat project default")

	applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
		DefaultHarnessAuth: "vertex-ai",
	})
	assert.Equal(t, "api-key", ac.HarnessAuth,
		"explicit per-agent should beat hub default")
}

// TestHarnessAuth_Precedence_HubFillsWhenBothEmpty verifies that the hub default
// fills when both per-agent and project defaults are empty.
func TestHarnessAuth_Precedence_HubFillsWhenBothEmpty(t *testing.T) {
	project := &store.Project{
		Annotations: map[string]string{},
	}

	ac := &store.AgentAppliedConfig{}

	applyProjectDefaults(ac, project)
	assert.Empty(t, ac.HarnessAuth,
		"no project default → HarnessAuth stays empty")

	applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
		DefaultHarnessAuth: "vertex-ai",
	})
	assert.Equal(t, "vertex-ai", ac.HarnessAuth,
		"hub default should fill when both per-agent and project are empty")
}

// TestHarnessAuth_Precedence_NoDefaultUnchanged verifies that when no default
// is set at any level, behavior is unchanged (harnessAuth stays empty).
func TestHarnessAuth_Precedence_NoDefaultUnchanged(t *testing.T) {
	project := &store.Project{
		Annotations: map[string]string{},
	}

	ac := &store.AgentAppliedConfig{}

	applyProjectDefaults(ac, project)
	applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{})

	assert.Empty(t, ac.HarnessAuth,
		"no defaults at any level → HarnessAuth stays empty")
}

func TestProjectSettings_DefaultModel(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		DefaultModel: "claude-sonnet-5",
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "claude-sonnet-5", putResp.DefaultModel)

	// GET should return persisted value
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "claude-sonnet-5", getResp.DefaultModel)

	// Clear by sending empty value
	clearBody := hubclient.ProjectSettings{}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", clearBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var clearResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clearResp))
	assert.Empty(t, clearResp.DefaultModel)
}

func TestApplyProjectDefaults_Model(t *testing.T) {
	t.Run("applies default model when empty", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-model": "claude-sonnet-5",
			},
		}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "claude-sonnet-5", ac.Model)
	})

	t.Run("does not override explicit model", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/default-model": "claude-sonnet-5",
			},
		}
		ac := &store.AgentAppliedConfig{Model: "claude-opus-4"}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "claude-opus-4", ac.Model)
	})
}

// TestApplyProjectDefaults_ActiveProfile pins the project tier of the profile
// precedence chain. scion.io/active-profile was parsed
// (projectSettingsFromAnnotations) and persisted
// (applyProjectSettingsToAnnotations) but never applied to any agent — the
// annotation had no read site outside its defining file, so setting it in the
// Project Settings UI did nothing.
//
// The request tier already worked and must keep working: the agent-create path
// stamps AppliedConfig.Profile from req.Profile in handlers_agent_create_helpers.go.
// Hence the only-if-unset guard, matching the Model and HarnessConfig siblings.
func TestApplyProjectDefaults_ActiveProfile(t *testing.T) {
	t.Run("applies active profile when empty", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/active-profile": "k8s-prod",
			},
		}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "k8s-prod", ac.Profile,
			"the project's active-profile annotation should reach AppliedConfig")
	})

	t.Run("does not override explicit profile", func(t *testing.T) {
		project := &store.Project{
			Annotations: map[string]string{
				"scion.io/active-profile": "k8s-prod",
			},
		}
		ac := &store.AgentAppliedConfig{Profile: "docker-local"}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "docker-local", ac.Profile,
			"an explicit request-level profile outranks the project annotation")
	})

	t.Run("no annotation leaves profile untouched", func(t *testing.T) {
		project := &store.Project{Annotations: map[string]string{}}
		ac := &store.AgentAppliedConfig{Profile: "docker-local"}
		applyProjectDefaults(ac, project)
		assert.Equal(t, "docker-local", ac.Profile)
	})

	t.Run("no annotation and no explicit value leaves profile empty", func(t *testing.T) {
		project := &store.Project{Annotations: map[string]string{}}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)
		assert.Empty(t, ac.Profile)
	})
}

// allResourceAnnotations is the full set of project resource defaults, used by
// the merge tests below so that each case can show exactly which fields
// survive.
func allResourceAnnotations() map[string]string {
	return map[string]string{
		"scion.io/default-resources-cpu-request":    "500m",
		"scion.io/default-resources-memory-request": "1Gi",
		"scion.io/default-resources-cpu-limit":      "2",
		"scion.io/default-resources-memory-limit":   "4Gi",
		"scion.io/default-resources-disk":           "10Gi",
	}
}

// TestApplyProjectDefaults_ResourcesMerge pins the per-field merge. The
// previous implementation replaced the whole ResourceSpec only when
// InlineConfig.Resources was nil, so a template that set any single field
// discarded every project default — including ones it said nothing about.
// MaxTurns/MaxModelCalls/MaxDuration ten lines above have always merged per
// field; resources now match.
func TestApplyProjectDefaults_ResourcesMerge(t *testing.T) {
	t.Run("applies all project resources when inline has none", func(t *testing.T) {
		project := &store.Project{Annotations: allResourceAnnotations()}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)

		require.NotNil(t, ac.InlineConfig)
		require.NotNil(t, ac.InlineConfig.Resources)
		assert.Equal(t, "500m", ac.InlineConfig.Resources.Requests.CPU)
		assert.Equal(t, "1Gi", ac.InlineConfig.Resources.Requests.Memory)
		assert.Equal(t, "2", ac.InlineConfig.Resources.Limits.CPU)
		assert.Equal(t, "4Gi", ac.InlineConfig.Resources.Limits.Memory)
		assert.Equal(t, "10Gi", ac.InlineConfig.Resources.Disk)
	})

	// The headline case: one inline field must not wipe out the other four.
	t.Run("partial inline resources fall through per field", func(t *testing.T) {
		project := &store.Project{Annotations: allResourceAnnotations()}
		ac := &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Resources: &api.ResourceSpec{
					Limits: api.ResourceList{Memory: "8Gi"},
				},
			},
		}
		applyProjectDefaults(ac, project)

		require.NotNil(t, ac.InlineConfig.Resources)
		assert.Equal(t, "8Gi", ac.InlineConfig.Resources.Limits.Memory,
			"the agent/template value must still win for the field it sets")
		assert.Equal(t, "500m", ac.InlineConfig.Resources.Requests.CPU,
			"unrelated project defaults must survive")
		assert.Equal(t, "1Gi", ac.InlineConfig.Resources.Requests.Memory)
		assert.Equal(t, "2", ac.InlineConfig.Resources.Limits.CPU)
		assert.Equal(t, "10Gi", ac.InlineConfig.Resources.Disk)
	})

	t.Run("fully specified inline resources are untouched", func(t *testing.T) {
		project := &store.Project{Annotations: allResourceAnnotations()}
		ac := &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Resources: &api.ResourceSpec{
					Requests: api.ResourceList{CPU: "100m", Memory: "256Mi"},
					Limits:   api.ResourceList{CPU: "1", Memory: "512Mi"},
					Disk:     "1Gi",
				},
			},
		}
		applyProjectDefaults(ac, project)

		assert.Equal(t, "100m", ac.InlineConfig.Resources.Requests.CPU)
		assert.Equal(t, "256Mi", ac.InlineConfig.Resources.Requests.Memory)
		assert.Equal(t, "1", ac.InlineConfig.Resources.Limits.CPU)
		assert.Equal(t, "512Mi", ac.InlineConfig.Resources.Limits.Memory)
		assert.Equal(t, "1Gi", ac.InlineConfig.Resources.Disk)
	})

	// A project that sets only some fields must not invent values for the rest.
	t.Run("partial project resources leave unset fields empty", func(t *testing.T) {
		project := &store.Project{Annotations: map[string]string{
			"scion.io/default-resources-disk": "10Gi",
		}}
		ac := &store.AgentAppliedConfig{}
		applyProjectDefaults(ac, project)

		require.NotNil(t, ac.InlineConfig)
		require.NotNil(t, ac.InlineConfig.Resources)
		assert.Equal(t, "10Gi", ac.InlineConfig.Resources.Disk)
		assert.Empty(t, ac.InlineConfig.Resources.Requests.CPU)
		assert.Empty(t, ac.InlineConfig.Resources.Requests.Memory)
		assert.Empty(t, ac.InlineConfig.Resources.Limits.CPU)
		assert.Empty(t, ac.InlineConfig.Resources.Limits.Memory)
	})

	t.Run("no project resource annotations leaves inline resources alone", func(t *testing.T) {
		project := &store.Project{Annotations: map[string]string{
			"scion.io/default-max-turns": "5",
		}}
		ac := &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{
				Resources: &api.ResourceSpec{Disk: "1Gi"},
			},
		}
		applyProjectDefaults(ac, project)

		require.NotNil(t, ac.InlineConfig.Resources)
		assert.Equal(t, "1Gi", ac.InlineConfig.Resources.Disk)
		assert.Empty(t, ac.InlineConfig.Resources.Requests.CPU)
		assert.Equal(t, 5, ac.InlineConfig.MaxTurns)
	})

	// The merge must not alias the project's spec into the agent's config:
	// projectResourceSpecToAPI allocates per call, but a future refactor that
	// returned a shared pointer would let one agent's mutation leak into the
	// next. Pinning it here makes that a test failure rather than a data race.
	t.Run("merged spec is not shared between agents", func(t *testing.T) {
		project := &store.Project{Annotations: allResourceAnnotations()}

		first := &store.AgentAppliedConfig{}
		applyProjectDefaults(first, project)
		second := &store.AgentAppliedConfig{}
		applyProjectDefaults(second, project)

		require.NotNil(t, first.InlineConfig.Resources)
		require.NotNil(t, second.InlineConfig.Resources)
		assert.NotSame(t, first.InlineConfig.Resources, second.InlineConfig.Resources)

		first.InlineConfig.Resources.Disk = "mutated"
		assert.Equal(t, "10Gi", second.InlineConfig.Resources.Disk,
			"mutating one agent's resources must not affect another's")
	})
}

// newSettingsTestSA builds a project-scoped, verified SA belonging to project.
func newSettingsTestSA(t *testing.T, s store.Store, projectID, idName string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 tid(idName + t.Name()),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              idName + "@proj.iam.gserviceaccount.com",
		ProjectID:          "gcp-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), sa))
	return sa
}

// This test previously PUT "sa-123" — an ID that did not exist — and asserted
// 200, which pinned the unvalidated write as correct behaviour. It now uses a
// real verified SA; the rejection cases live in the tests below.
func TestProjectSettings_DefaultGCPIdentity(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-default")

	putBody := hubclient.ProjectSettings{
		DefaultGCPIdentityMode:             "assign",
		DefaultGCPIdentityServiceAccountID: sa.ID,
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "assign", putResp.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, putResp.DefaultGCPIdentityServiceAccountID)

	// GET should return persisted values
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "assign", getResp.DefaultGCPIdentityMode)
	assert.Equal(t, sa.ID, getResp.DefaultGCPIdentityServiceAccountID)
}

func TestProjectSettings_ClearDefaultGCPIdentity(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Set values first
	putBody := hubclient.ProjectSettings{
		DefaultGCPIdentityMode:             "passthrough",
		DefaultGCPIdentityServiceAccountID: "",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code)

	// Clear by sending empty values
	clearBody := hubclient.ProjectSettings{}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", clearBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.DefaultGCPIdentityMode)
	assert.Empty(t, resp.DefaultGCPIdentityServiceAccountID)
}

// The tests below cover #22: the settings PUT used to store
// DefaultGCPIdentityServiceAccountID unvalidated and return 200, after which
// createAgentInProject silently fell back to metadataMode=block. Each case is a
// value that would have been accepted before and produced that silent failure.

func TestProjectSettings_DefaultGCPIdentity_RejectsUnknownSA(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountID: "no-such-sa"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a default pointing at a nonexistent SA must be refused at write time; got: %s", rec.Body.String())

	// The bad value must not have been persisted.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Empty(t, got.DefaultGCPIdentityServiceAccountID)
}

// Not-found and not-reachable must be indistinguishable, or the PUT becomes an
// existence oracle: a project owner could enumerate other projects' SA IDs by
// watching which ones fail differently.
func TestProjectSettings_DefaultGCPIdentity_OtherProjectSAIsNotAnOracle(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	other := &store.Project{
		ID:         tid("other-project-" + t.Name()),
		Name:       "Other Project",
		Slug:       "other-project",
		Visibility: "private",
	}
	require.NoError(t, s.CreateProject(t.Context(), other))
	otherSA := newSettingsTestSA(t, s, other.ID, "sa-elsewhere")

	recOther := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountID: otherSA.ID})
	require.Equal(t, http.StatusBadRequest, recOther.Code,
		"another project's SA must not be settable as this project's default; got: %s", recOther.Body.String())

	recUnknown := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountID: "no-such-sa"})
	require.Equal(t, http.StatusBadRequest, recUnknown.Code)

	assert.Equal(t, recUnknown.Body.String(), recOther.Body.String(),
		"an SA that exists elsewhere and one that does not exist must be indistinguishable to the caller")
}

func TestProjectSettings_DefaultGCPIdentity_RejectsUnverifiedSA(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-unverified-" + t.Name()),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "unverified@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  false,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), sa))

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountID: sa.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"consumption requires sa.Verified, so the write must too; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not verified",
		"this SA is already readable by the caller, so naming the reason discloses nothing")
}

// The crossing case: hub-scoped AND unverified. The scope gate passes here —
// a hub-scoped account is reachable from every project — so verification is
// the only thing left refusing it.
//
// Worth its own test rather than folding into _RejectsUnverifiedSA above,
// which uses a project-scoped account and would therefore still pass if the
// scope check alone rejected the write and verification were never consulted.
// Only this combination can tell those two apart.
//
// This case arrived from the other side. It was covered at the consumption
// site (TestAgentCreate_UnverifiedHubScopedDefault_FallsThroughToBlock) and
// not at the write site; that test used to install its default through this
// very route, so #22 turned it red and exposed the gap. Both ends now assert
// it independently.
func TestProjectSettings_DefaultGCPIdentity_RejectsUnverifiedHubScopedSA(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	sa := &store.GCPServiceAccount{
		ID:      tid("sa-hub-unverified-" + t.Name()),
		Scope:   store.ScopeHub,
		ScopeID: "some-hub-instance", // Provenance only; never compared.
		Email:   "hub-unverified@proj.iam.gserviceaccount.com",

		ProjectID: "gcp-proj",
		Verified:  false,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), sa))

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityServiceAccountID: sa.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"hub scope makes an account reachable, not usable; it must still be verified. got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not verified",
		"the refusal must name verification, not reachability: a hub-scoped account IS reachable here, "+
			"and reporting it as unavailable would send an operator looking for a scope problem that does not exist")
}

// mode=assign with no SA is the same defect in different clothes: the
// consumption path falls straight through to block.
func TestProjectSettings_DefaultGCPIdentity_RejectsAssignModeWithoutSA(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityMode: "assign"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"mode=assign with no service account saves a setting that does nothing; got: %s", rec.Body.String())
}

// Clearing must always be permitted: it is the operator's only escape from a
// value that has since gone bad (SA deleted or un-verified after being set).
func TestProjectSettings_DefaultGCPIdentity_ClearingAlwaysAllowed(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)
	sa := newSettingsTestSA(t, s, project.ID, "sa-to-clear")

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityMode: "assign", DefaultGCPIdentityServiceAccountID: sa.ID})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{})
	require.Equal(t, http.StatusOK, rec.Code, "clearing must never be blocked; got: %s", rec.Body.String())

	var got hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Empty(t, got.DefaultGCPIdentityServiceAccountID)
}

// TestProjectSettings_HubScopedDefaultIsAcceptedAndConsumed pins that the write
// site and the consumption site AGREE about hub-scoped service accounts.
//
// It was originally written to pin their DISAGREEMENT. The #22 validator admits
// a hub-scoped SA — correct under Q5 option A, where such an account is
// legitimately pickable in any project — while handlers_agents_core.go:655 still
// used bare `sa.ScopeID == projectID` and silently fell through to block. Both
// halves were asserted so that converting :655 would turn this test RED and
// reach whoever did the conversion.
//
// It worked: step 4 of the Goal 2 landing sequence (a44b2950) landed and half B
// failed on exactly the assertion that named it. Half B now expects assign. Half
// A was never in question — the validator was always the correct side.
//
// Keep both halves asserted together. The value here is not either behaviour on
// its own; it is that a hub-scoped default which is ACCEPTED at write time is
// also HONOURED at creation time. If those two ever diverge again, in either
// direction, this is the test that says so.
func TestProjectSettings_HubScopedDefaultIsAcceptedAndConsumed(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	// The D4 mode coupling gate (sa_assign_gate.go) requires
	// gcpIamCheckMode=enforce for hub-scoped SA assignment. Wire it up
	// so that half B's agent creation passes the gate.
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget("hub-default@hub.iam.gserviceaccount.com"))

	hubSA := &store.GCPServiceAccount{
		ID:                 tid("sa-hub-default-" + t.Name()),
		Scope:              store.ScopeHub,
		ScopeID:            "hub-instance-1",
		Email:              "hub-default@hub.iam.gserviceaccount.com",
		ProjectID:          "hub-gcp-project",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), hubSA))

	// Half A — the validator admits it.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		hubclient.ProjectSettings{
			DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
			DefaultGCPIdentityServiceAccountID: hubSA.ID,
		})
	require.Equal(t, http.StatusOK, rec.Code,
		"a hub-scoped SA is pickable in any project under Q5 option A; got: %s", rec.Body.String())

	// Guard against a vacuous pass: half B would also see "block" if the setting
	// had simply not persisted, which would make this test green for the wrong
	// reason and useless as a tripwire.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var saved hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&saved))
	require.Equal(t, hubSA.ID, saved.DefaultGCPIdentityServiceAccountID,
		"the hub-scoped default must actually be stored for half B to mean anything")
	require.Equal(t, store.GCPMetadataModeAssign, saved.DefaultGCPIdentityMode)

	// Half B — consumption honours it.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-default-agent",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode,
		"a hub-scoped default accepted by the PUT must be honoured at agent creation; "+
			"if this is 'block' again, the write and consumption sites have diverged")
	assert.Equal(t, hubSA.ID, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, hubSA.Email, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountEmail)
}

// TestGCPServiceAccount_HubScopedCreateStillRejected is a DELIBERATE TRIPWIRE for
// the ordering of the Goal 2 landing sequence. It asserts that a feature does not
// exist yet, which is why it explains itself at length.
//
// The sequence is strict:
//
//	step 1 — P0.4 assign grant baseline, both arms
//	step 2 — convert the assignment authorization from ActionRead to ActionAssign
//	step 3 — relocate the reachability predicate to pkg/store        [LANDED]
//	step 4 — convert the three assign sites to it                    [LANDED]
//	step 5 — item A, POST scope=hub
//
// THE HOLD ON ITEM A IS A SECURITY HOLD, AND IT NOW HANGS ON STEP 2 — NOT ON
// STEPS 3 AND 4, WHICH HAVE LANDED. The distinction matters, because a comment
// naming only 3 and 4 would describe a hold that no longer exists for the reason
// it gives, which is worse than no comment.
//
// What changed: before step 4, an early item A was merely broken. The assign
// sites compared sa.ScopeID against the project ID, so a hub-scoped SA was
// refused with a fail-closed 400. Step 4 removed that 400 by design. What now
// stands between a hub-scoped SA and assignment is the ActionRead check alone.
//
// FOR A HUMAN CALLER that check passes for every hub member: the SA resource is
// parentless, so the project-owner bypass is skipped, and hub-member-read-all
// ("*", read+list) matches it because matchesResource has no arm for a "hub"
// ScopeType and falls through to true; every user is put in hub-members on
// login. FOR AN AGENT CALLER it DENIES — principals come from the agent's own
// groups, which never include hub-members, so the wildcard policy is never
// fetched, and the read baseline then requires a non-empty project ID that a
// parentless resource cannot supply.
//
// State the caller. Hub scope REMOVES confinement for humans and ADDS it for
// agents, so any unqualified sentence about "the gate" here is half wrong —
// including the one this paragraph replaced, which said "every hub member" flat
// (corrected by sa-arch at 9427fa19 after aid-em caught it).
//
// The hold does not weaken: the human path alone is the exposure. Landing item A
// before step 2 does not produce a feature that half-works. It produces
// hub-scoped credentials assignable by any HUMAN member of any project: the
// cross-project exposure of design 8.2, live. Step 2 is what closes it, because
// the ActionAssign arm is project-scoped and a project-scoped policy cannot match
// P9: TestGCPServiceAccount_HubScopedCreateStillRejected removed.
// The tripwire was a deliberate hold until step 2 (ActionAssign conversion)
// landed and hub-scoped BYO registration could safely open. P9 completes
// that: hub-scoped BYO registration is now live, guarded by hub membership at
// registration and by mode coupling + actAs at assignment. The prerequisite
// tests (PlainHubMemberAllowed, FormerHubMemberCreatorDenied) are green.

func TestApplyProjectDefaults_GCPIdentityNotApplied(t *testing.T) {
	// applyProjectDefaults does NOT apply GCP identity — that's handled
	// directly in createAgentInProject. This test verifies it doesn't interfere.
	project := &store.Project{
		Annotations: map[string]string{
			"scion.io/default-gcp-identity-mode":               "passthrough",
			"scion.io/default-gcp-identity-service-account-id": "sa-123",
		},
	}
	ac := &store.AgentAppliedConfig{}
	applyProjectDefaults(ac, project)
	// GCP identity should NOT be set by applyProjectDefaults
	assert.Nil(t, ac.GCPIdentity)
}

func TestProjectSettings_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/nonexistent/settings", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectSettings_MaxAgentRole_PutAndGet(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		MaxAgentRole: "readonly",
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "readonly", putResp.MaxAgentRole)

	// GET should return persisted value
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "readonly", getResp.MaxAgentRole)
}

func TestProjectSettings_MaxAgentRole_InvalidReturns400(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		MaxAgentRole: "superadmin",
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "maxAgentRole")
}

func TestProjectSettings_MaxAgentRole_ClearValue(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Set it first
	putBody := hubclient.ProjectSettings{MaxAgentRole: "readonly"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code)

	// Clear it
	putBody = hubclient.ProjectSettings{MaxAgentRole: ""}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Empty(t, getResp.MaxAgentRole)
}

func createTestProjectForSettings(t *testing.T, s store.Store) *store.Project {
	t.Helper()
	project := &store.Project{
		ID:         tid("test-project-settings-" + t.Name()),
		Name:       "Test Project",
		Slug:       "test-project-settings",
		Visibility: "private",
	}
	require.NoError(t, s.CreateProject(t.Context(), project))
	return project
}
