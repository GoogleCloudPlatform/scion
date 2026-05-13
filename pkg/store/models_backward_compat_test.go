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

package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTemplate_MarshalJSON_EmitsGroveID(t *testing.T) {
	tmpl := Template{
		ID:        "t-1",
		Name:      "test-template",
		ProjectID: "p-1",
		Scope:     "project",
		Status:    "active",
	}

	data, err := json.Marshal(tmpl)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "p-1", m["projectId"])
	assert.Equal(t, "p-1", m["groveId"])
}

func TestTemplate_MarshalJSON_EmptyProjectID(t *testing.T) {
	tmpl := Template{
		ID:     "t-1",
		Name:   "global-template",
		Scope:  "global",
		Status: "active",
	}

	data, err := json.Marshal(tmpl)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Both projectId and groveId should be omitted when empty
	_, hasProjectID := m["projectId"]
	_, hasGroveID := m["groveId"]
	assert.False(t, hasProjectID, "projectId should be omitted when empty")
	assert.False(t, hasGroveID, "groveId should be omitted when empty")
}

func TestTemplate_UnmarshalJSON_FromProjectID(t *testing.T) {
	data := `{"id":"t-1","name":"test","projectId":"p-1","scope":"project","status":"active"}`

	var tmpl Template
	err := json.Unmarshal([]byte(data), &tmpl)
	require.NoError(t, err)

	assert.Equal(t, "t-1", tmpl.ID)
	assert.Equal(t, "p-1", tmpl.ProjectID)
}

func TestTemplate_UnmarshalJSON_FromGroveID(t *testing.T) {
	data := `{"id":"t-1","name":"test","groveId":"p-1","scope":"project","status":"active"}`

	var tmpl Template
	err := json.Unmarshal([]byte(data), &tmpl)
	require.NoError(t, err)

	assert.Equal(t, "t-1", tmpl.ID)
	assert.Equal(t, "p-1", tmpl.ProjectID)
}

func TestTemplate_UnmarshalJSON_ProjectIDTakesPrecedence(t *testing.T) {
	data := `{"id":"t-1","name":"test","projectId":"p-1","groveId":"p-2","scope":"project","status":"active"}`

	var tmpl Template
	err := json.Unmarshal([]byte(data), &tmpl)
	require.NoError(t, err)

	assert.Equal(t, "p-1", tmpl.ProjectID, "projectId should take precedence over groveId")
}

func TestSubscriptionTemplate_MarshalJSON_EmitsGroveID(t *testing.T) {
	st := SubscriptionTemplate{
		ID:        "st-1",
		Name:      "test-sub-template",
		ProjectID: "p-1",
	}

	data, err := json.Marshal(st)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "p-1", m["projectId"])
	assert.Equal(t, "p-1", m["groveId"])
}

func TestSubscriptionTemplate_UnmarshalJSON_FromGroveID(t *testing.T) {
	data := `{"id":"st-1","name":"test-sub","groveId":"p-1"}`

	var st SubscriptionTemplate
	err := json.Unmarshal([]byte(data), &st)
	require.NoError(t, err)

	assert.Equal(t, "st-1", st.ID)
	assert.Equal(t, "p-1", st.ProjectID)
}

func TestSubscriptionTemplate_UnmarshalJSON_ProjectIDTakesPrecedence(t *testing.T) {
	data := `{"id":"st-1","name":"test-sub","projectId":"p-1","groveId":"p-2"}`

	var st SubscriptionTemplate
	err := json.Unmarshal([]byte(data), &st)
	require.NoError(t, err)

	assert.Equal(t, "p-1", st.ProjectID, "projectId should take precedence over groveId")
}
