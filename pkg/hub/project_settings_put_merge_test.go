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
	"maps"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeTestStoredSettings is a project with every annotation-backed setting
// populated, so that keeping and clearing are both observable for every
// field.
func mergeTestStoredSettings() map[string]string {
	thinking := 40
	yes := true
	activeProfile := "prof-a"
	project := &store.Project{}
	applyProjectSettingsToAnnotations(project, &hubclient.ProjectSettings{
		ActiveProfile:          &activeProfile,
		DefaultTemplate:        "tmpl-a",
		DefaultHarnessConfig:   "hc-a",
		DefaultHarnessAuth:     "auth-a",
		DefaultModel:           "model-a",
		DefaultThinkingLevel:   &thinking,
		TelemetryEnabled:       &yes,
		AutoExposePortsEnabled: &yes,
		DefaultMaxTurns:        10,
		DefaultMaxModelCalls:   20,
		DefaultMaxDuration:     "1h",
		DefaultResources: &hubclient.ProjectResourceSpec{
			Requests: &hubclient.ProjectResourceList{CPU: "500m", Memory: "1Gi"},
			Limits:   &hubclient.ProjectResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "10Gi",
		},
		DefaultGCPIdentityMode:                      store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID:          "sa-a",
		DefaultGCPIdentityServiceAccountIDByProfile: map[string]string{"p1": "sa-p1"},
		MaxAgentRole:     "full",
		DefaultAgentRole: "baseline",
	})
	return project.Annotations
}

// putMergeField describes how one field of hubclient.ProjectSettings
// behaves on PUT. In the want maps, an empty string means the annotation is
// deleted.
type putMergeField struct {
	// name is the JSON field name.
	name string
	// ignored marks a field the endpoint accepts but does not store.
	ignored bool
	// setJSON is a new, non-empty value and setWant the annotations it writes.
	setJSON string
	setWant map[string]string
	// clearJSON is the explicit value that clears the field.
	clearJSON string
	// keys are the annotations the field owns; clearing deletes them all.
	keys []string
	// nullClears marks the tri-state fields where null resets to inherit.
	nullClears bool
	// zeroJSON/zeroWant cover a tri-state field whose zero value is stored.
	zeroJSON string
	zeroWant map[string]string
}

var resourceKeys = []string{
	projectSettingDefaultResourcesCPUReq,
	projectSettingDefaultResourcesMemReq,
	projectSettingDefaultResourcesCPULim,
	projectSettingDefaultResourcesMemLim,
	projectSettingDefaultResourcesDisk,
}

// putMergeFields has one row per field of hubclient.ProjectSettings.
// TestMergeProjectSettingsPut_EveryField fails when a field has no row.
var putMergeFields = []putMergeField{
	{name: "activeProfile", setJSON: `"prof-b"`, setWant: map[string]string{projectSettingActiveProfile: "prof-b"}, clearJSON: `""`, keys: []string{projectSettingActiveProfile}},
	{name: "defaultTemplate", setJSON: `"tmpl-b"`, setWant: map[string]string{projectSettingDefaultTemplate: "tmpl-b"}, clearJSON: `""`, keys: []string{projectSettingDefaultTemplate}},
	{name: "defaultHarnessConfig", setJSON: `"hc-b"`, setWant: map[string]string{projectSettingDefaultHarnessConfig: "hc-b"}, clearJSON: `""`, keys: []string{projectSettingDefaultHarnessConfig}},
	{name: "defaultHarnessAuth", setJSON: `"auth-b"`, setWant: map[string]string{projectSettingDefaultHarnessAuth: "auth-b"}, clearJSON: `""`, keys: []string{projectSettingDefaultHarnessAuth}},
	{name: "defaultModel", setJSON: `"model-b"`, setWant: map[string]string{projectSettingDefaultModel: "model-b"}, clearJSON: `""`, keys: []string{projectSettingDefaultModel}},
	{
		name: projectSettingsFieldThinkingLevel, setJSON: `70`, setWant: map[string]string{projectSettingDefaultThinkingLevel: "70"},
		clearJSON: `null`, keys: []string{projectSettingDefaultThinkingLevel}, nullClears: true,
		zeroJSON: `0`, zeroWant: map[string]string{projectSettingDefaultThinkingLevel: "0"},
	},
	{
		name: projectSettingsFieldTelemetryEnabled, setJSON: `false`, setWant: map[string]string{projectSettingTelemetryEnabled: "false"},
		clearJSON: `null`, keys: []string{projectSettingTelemetryEnabled}, nullClears: true,
		zeroJSON: `false`, zeroWant: map[string]string{projectSettingTelemetryEnabled: "false"},
	},
	{
		name: projectSettingsFieldAutoExposePortsEnabled, setJSON: `false`, setWant: map[string]string{projectSettingAutoExposePortsEnabled: "false"},
		clearJSON: `null`, keys: []string{projectSettingAutoExposePortsEnabled}, nullClears: true,
		zeroJSON: `false`, zeroWant: map[string]string{projectSettingAutoExposePortsEnabled: "false"},
	},
	{name: "bucket", ignored: true, setJSON: `{"provider":"gcs","bucket":"b"}`, clearJSON: `{}`},
	{name: "runtimes", ignored: true, setJSON: `{"docker":{}}`, clearJSON: `{}`},
	{name: "harnesses", ignored: true, setJSON: `{"claude":{}}`, clearJSON: `{}`},
	{name: "profiles", ignored: true, setJSON: `{"p":{}}`, clearJSON: `{}`},
	{name: "defaultMaxTurns", setJSON: `11`, setWant: map[string]string{projectSettingDefaultMaxTurns: "11"}, clearJSON: `0`, keys: []string{projectSettingDefaultMaxTurns}},
	{name: "defaultMaxModelCalls", setJSON: `21`, setWant: map[string]string{projectSettingDefaultMaxModelCalls: "21"}, clearJSON: `0`, keys: []string{projectSettingDefaultMaxModelCalls}},
	{name: "defaultMaxDuration", setJSON: `"2h"`, setWant: map[string]string{projectSettingDefaultMaxDuration: "2h"}, clearJSON: `""`, keys: []string{projectSettingDefaultMaxDuration}},
	{
		// Object fields are replaced whole, not merged key by key.
		name: "defaultResources", setJSON: `{"disk":"5Gi"}`,
		setWant: map[string]string{
			projectSettingDefaultResourcesDisk:   "5Gi",
			projectSettingDefaultResourcesCPUReq: "",
			projectSettingDefaultResourcesMemReq: "",
			projectSettingDefaultResourcesCPULim: "",
			projectSettingDefaultResourcesMemLim: "",
		},
		clearJSON: `{}`, keys: resourceKeys,
	},
	{name: projectSettingsFieldGCPIdentityMode, setJSON: `"passthrough"`, setWant: map[string]string{projectSettingDefaultGCPIdentityMode: "passthrough"}, clearJSON: `""`, keys: []string{projectSettingDefaultGCPIdentityMode}},
	{name: projectSettingsFieldGCPIdentitySAID, setJSON: `"sa-b"`, setWant: map[string]string{projectSettingDefaultGCPIdentitySAID: "sa-b"}, clearJSON: `""`, keys: []string{projectSettingDefaultGCPIdentitySAID}},
	{name: projectSettingsFieldGCPIdentitySAIDByProfile, setJSON: `{"p2":"sa-p2"}`, setWant: map[string]string{projectSettingDefaultGCPIdentitySAIDByProfile: `{"p2":"sa-p2"}`}, clearJSON: `{}`, keys: []string{projectSettingDefaultGCPIdentitySAIDByProfile}},
	{name: projectSettingsFieldMaxAgentRole, setJSON: `"readonly"`, setWant: map[string]string{projectSettingMaxAgentRole: "readonly"}, clearJSON: `""`, keys: []string{projectSettingMaxAgentRole}},
	{name: projectSettingsFieldDefaultAgentRole, setJSON: `"readonly"`, setWant: map[string]string{projectSettingDefaultAgentRole: "readonly"}, clearJSON: `""`, keys: []string{projectSettingDefaultAgentRole}},
}

// runPutMerge applies body to a fully populated project the way the PUT
// handler does and returns the resulting annotations.
func runPutMerge(t *testing.T, body string) (map[string]string, map[string]bool) {
	t.Helper()
	project := &store.Project{Annotations: mergeTestStoredSettings()}
	merged, present, err := mergeProjectSettingsPut(projectSettingsFromAnnotations(project), []byte(body))
	require.NoError(t, err)
	applyProjectSettingsToAnnotations(project, merged)
	return project.Annotations, present
}

// withChanges returns base with changes applied; an empty value deletes.
func withChanges(base, changes map[string]string) map[string]string {
	out := maps.Clone(base)
	for k, v := range changes {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// TestMergeProjectSettingsPut_EveryField checks the PUT rule for every field
// of the request type: absent keeps, null keeps (null resets the tri-state
// fields), the empty value clears, and a value replaces. Only the field
// under test may change.
func TestMergeProjectSettingsPut_EveryField(t *testing.T) {
	stored := mergeTestStoredSettings()
	require.Len(t, stored, len(projectSettingKeys),
		"the stored fixture must populate every project setting")

	// Guard: one row per struct field, so a new field cannot skip this test.
	typ := reflect.TypeOf(hubclient.ProjectSettings{})
	var structFields []string
	for i := 0; i < typ.NumField(); i++ {
		if name := jsonFieldName(typ.Field(i)); name != "" {
			structFields = append(structFields, name)
		}
	}
	var rowFields []string
	for _, f := range putMergeFields {
		rowFields = append(rowFields, f.name)
	}
	require.Equal(t, structFields, rowFields,
		"putMergeFields must have exactly one row per hubclient.ProjectSettings field, "+
			"in declaration order; add a row for the new field")

	t.Run("empty body keeps everything", func(t *testing.T) {
		got, present := runPutMerge(t, `{}`)
		assert.Equal(t, stored, got)
		assert.Empty(t, present)
	})

	for _, f := range putMergeFields {
		t.Run(f.name, func(t *testing.T) {
			clearWant := map[string]string{}
			for _, k := range f.keys {
				clearWant[k] = ""
			}

			t.Run("absent keeps", func(t *testing.T) {
				got, present := runPutMerge(t, `{"unrelatedUnknownKey":"x"}`)
				assert.Equal(t, stored, got)
				assert.False(t, present[f.name])
			})

			t.Run("null", func(t *testing.T) {
				got, present := runPutMerge(t, `{"`+f.name+`":null}`)
				if f.nullClears {
					assert.Equal(t, withChanges(stored, clearWant), got, "null resets a tri-state field")
					assert.True(t, present[f.name])
				} else {
					assert.Equal(t, stored, got, "null keeps the stored value")
					assert.False(t, present[f.name])
				}
			})

			t.Run("explicit empty value clears", func(t *testing.T) {
				got, _ := runPutMerge(t, `{"`+f.name+`":`+f.clearJSON+`}`)
				assert.Equal(t, withChanges(stored, clearWant), got)
			})

			t.Run("value replaces", func(t *testing.T) {
				got, present := runPutMerge(t, `{"`+f.name+`":`+f.setJSON+`}`)
				assert.Equal(t, withChanges(stored, f.setWant), got)
				assert.True(t, present[f.name])
			})

			if f.zeroJSON != "" {
				t.Run("zero value is stored", func(t *testing.T) {
					got, _ := runPutMerge(t, `{"`+f.name+`":`+f.zeroJSON+`}`)
					assert.Equal(t, withChanges(stored, f.zeroWant), got)
				})
			}
		})
	}
}

// TestMergeProjectSettingsPut_NullClearsFieldsHaveRows ties the null-resets
// exception list to the table, so the two cannot drift apart.
func TestMergeProjectSettingsPut_NullClearsFieldsHaveRows(t *testing.T) {
	fromRows := map[string]bool{}
	for _, f := range putMergeFields {
		if f.nullClears {
			fromRows[f.name] = true
		}
	}
	assert.Equal(t, projectSettingsNullClearsFields, fromRows)
}

// Field names match case-insensitively, as encoding/json does, so a body
// the decoder accepts is also seen as present by the merge.
func TestMergeProjectSettingsPut_CaseInsensitiveNames(t *testing.T) {
	got, present := runPutMerge(t, `{"DefaultTemplate":"tmpl-b","DEFAULTGCPIDENTITYMODE":""}`)
	assert.Equal(t, "tmpl-b", got[projectSettingDefaultTemplate])
	assert.NotContains(t, got, projectSettingDefaultGCPIdentityMode)
	assert.True(t, present["defaultTemplate"])
	assert.True(t, present[projectSettingsFieldGCPIdentityMode])
}

func TestMergeProjectSettingsPut_RejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{`[]`, `"x"`, `{"defaultMaxTurns":"ten"}`, `{`} {
		_, _, err := mergeProjectSettingsPut(&hubclient.ProjectSettings{}, []byte(body))
		assert.Error(t, err, "body %s", body)
	}
}
