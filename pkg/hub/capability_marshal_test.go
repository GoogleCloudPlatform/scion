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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentWithCapabilities_MarshalJSON(t *testing.T) {
	agent := AgentWithCapabilities{
		Agent: store.Agent{
			ID:        "agent-1",
			ProjectID: "project-1",
			Name:      "my-agent",
		},
		Cap: &Capabilities{
			Actions: []string{"read"},
		},
		ResolvedHarness: "harness-1",
		CloudLogging:    true,
	}

	data, err := json.Marshal(agent)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Check embedded Agent fields
	assert.Equal(t, "agent-1", m["id"])
	assert.Equal(t, "project-1", m["projectId"])
	assert.Equal(t, "my-agent", m["name"])

	// Check capability fields
	assert.NotNil(t, m["_capabilities"])
	assert.Contains(t, m["_capabilities"].(map[string]interface{})["actions"], "read")
	assert.Equal(t, "harness-1", m["resolvedHarness"])
	assert.Equal(t, true, m["cloudLogging"])

	// Check legacy fields
	assert.Equal(t, "project-1", m["groveId"])
}

func TestProjectWithCapabilities_MarshalJSON(t *testing.T) {
	project := ProjectWithCapabilities{
		Project: store.Project{
			ID:   "p-1",
			Name: "Project 1",
			Slug: "project-1",
		},
		Cap: &Capabilities{
			Actions: []string{"write"},
		},
		CloudLogging: true,
	}

	data, err := json.Marshal(project)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Check embedded Project fields
	assert.Equal(t, "p-1", m["id"])
	assert.Equal(t, "Project 1", m["name"])
	assert.Equal(t, "project-1", m["slug"])

	// Check capability fields
	assert.NotNil(t, m["_capabilities"])
	assert.Contains(t, m["_capabilities"].(map[string]interface{})["actions"], "write")
	assert.Equal(t, true, m["cloudLogging"])

	// Check legacy fields
	assert.Equal(t, "p-1", m["groveId"])
	assert.Equal(t, "Project 1", m["groveName"])
	assert.Equal(t, "project-1", m["grove"])
}

func TestTemplateWithCapabilities_MarshalJSON(t *testing.T) {
	template := TemplateWithCapabilities{
		Template: store.Template{
			ID:        "t-1",
			ProjectID: "p-1",
			Slug:      "tmpl-1",
		},
		Cap: &Capabilities{
			Actions: []string{"create"},
		},
	}

	data, err := json.Marshal(template)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Check embedded Template fields
	assert.Equal(t, "t-1", m["id"])
	assert.Equal(t, "p-1", m["projectId"])
	assert.Equal(t, "tmpl-1", m["slug"])

	// Check capability fields
	assert.NotNil(t, m["_capabilities"])
	assert.Contains(t, m["_capabilities"].(map[string]interface{})["actions"], "create")

	// Check legacy fields
	assert.Equal(t, "p-1", m["groveId"])
}

func TestGroupWithCapabilities_MarshalJSON(t *testing.T) {
	group := GroupWithCapabilities{
		Group: store.Group{
			ID:        "g-1",
			ProjectID: "p-1",
			Name:      "Group 1",
		},
		Cap: &Capabilities{
			Actions: []string{"read"},
		},
	}

	data, err := json.Marshal(group)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "g-1", m["id"])
	assert.Equal(t, "p-1", m["projectId"])
	assert.Equal(t, "p-1", m["groveId"])
	assert.NotNil(t, m["_capabilities"])
}

func TestUserWithCapabilities_MarshalJSON(t *testing.T) {
	user := UserWithCapabilities{
		User: store.User{
			ID:    "u-1",
			Email: "user@example.com",
		},
		Cap: &Capabilities{
			Actions: []string{"read"},
		},
	}

	data, err := json.Marshal(user)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "u-1", m["id"])
	assert.Equal(t, "user@example.com", m["email"])
	assert.NotNil(t, m["_capabilities"])
}

func TestPolicyWithCapabilities_MarshalJSON(t *testing.T) {
	policy := PolicyWithCapabilities{
		Policy: store.Policy{
			ID:        "pol-1",
			ScopeType: store.PolicyScopeProject,
			ScopeID:   "p-1",
		},
		Cap: &Capabilities{
			Actions: []string{"read"},
		},
	}

	data, err := json.Marshal(policy)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "pol-1", m["id"])
	assert.Equal(t, "p-1", m["scopeId"])
	assert.Equal(t, "p-1", m["groveId"])
	assert.NotNil(t, m["_capabilities"])
}

func TestRuntimeBrokerWithCapabilities_MarshalJSON(t *testing.T) {
	broker := RuntimeBrokerWithCapabilities{
		RuntimeBroker: store.RuntimeBroker{
			ID:   "b-1",
			Name: "Broker 1",
		},
		Cap: &Capabilities{
			Actions: []string{"read"},
		},
	}

	data, err := json.Marshal(broker)
	require.NoError(t, err)

	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	assert.Equal(t, "b-1", m["id"])
	assert.Equal(t, "Broker 1", m["name"])
	assert.NotNil(t, m["_capabilities"])
}

func TestAgentWithCapabilities_UnmarshalJSON(t *testing.T) {
	data := `{"id":"a1","projectId":"p1","_capabilities":{"actions":["read"]}}`
	var agent AgentWithCapabilities
	err := json.Unmarshal([]byte(data), &agent)
	require.NoError(t, err)
	assert.Equal(t, "a1", agent.ID)
	assert.Equal(t, "p1", agent.ProjectID)
	assert.NotNil(t, agent.Cap)
	assert.Contains(t, agent.Cap.Actions, "read")
}

func TestProjectWithCapabilities_UnmarshalJSON(t *testing.T) {
	data := `{"id":"p1","name":"Project 1","slug":"p-1","_capabilities":{"actions":["write"]}}`
	var project ProjectWithCapabilities
	err := json.Unmarshal([]byte(data), &project)
	require.NoError(t, err)
	assert.Equal(t, "p1", project.ID)
	assert.NotNil(t, project.Cap)
	assert.Contains(t, project.Cap.Actions, "write")
}

func TestTemplateWithCapabilities_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectID", func(t *testing.T) {
		data := `{"id":"t1","projectId":"p1"}`
		var tmpl TemplateWithCapabilities
		err := json.Unmarshal([]byte(data), &tmpl)
		require.NoError(t, err)
		assert.Equal(t, "t1", tmpl.ID)
		assert.Equal(t, "p1", tmpl.ProjectID)
	})

	t.Run("HandleGroveID", func(t *testing.T) {
		data := `{"id":"t1","groveId":"p1"}`
		var tmpl TemplateWithCapabilities
		err := json.Unmarshal([]byte(data), &tmpl)
		require.NoError(t, err)
		assert.Equal(t, "t1", tmpl.ID)
		assert.Equal(t, "p1", tmpl.ProjectID)
	})
}
