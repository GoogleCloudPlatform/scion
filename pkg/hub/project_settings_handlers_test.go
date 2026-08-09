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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

func TestProjectSettings_DefaultGCPIdentity(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	putBody := hubclient.ProjectSettings{
		DefaultGCPIdentityMode:             "assign",
		DefaultGCPIdentityServiceAccountID: "sa-123",
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings", putBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var putResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&putResp))
	assert.Equal(t, "assign", putResp.DefaultGCPIdentityMode)
	assert.Equal(t, "sa-123", putResp.DefaultGCPIdentityServiceAccountID)

	// GET should return persisted values
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var getResp hubclient.ProjectSettings
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&getResp))
	assert.Equal(t, "assign", getResp.DefaultGCPIdentityMode)
	assert.Equal(t, "sa-123", getResp.DefaultGCPIdentityServiceAccountID)
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
