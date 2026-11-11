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
	"testing"

	"github.com/stretchr/testify/assert"
)

func fullBrokerAgentConfig() BrokerAgentConfig {
	return BrokerAgentConfig{
		Template:            "tmpl",
		HarnessConfig:       "claude",
		HarnessAuth:         "api-key",
		HarnessConfigSource: "template",
		Image:               "img:resolved",
		Profile:             "prof",
		Runtime:             "kubernetes",
	}
}

func TestApplyBrokerAgentConfig_WritesEveryResolvedValue(t *testing.T) {
	a := &Agent{AppliedConfig: &AgentAppliedConfig{HarnessConfig: "pre"}}
	ApplyBrokerAgentConfig(a, fullBrokerAgentConfig())
	assert.Equal(t, "tmpl", a.Template)
	assert.Equal(t, "kubernetes", a.Runtime)
	assert.Equal(t, "claude", a.AppliedConfig.HarnessConfig)
	assert.Equal(t, "api-key", a.AppliedConfig.HarnessAuth)
	assert.Equal(t, "template", a.AppliedConfig.HarnessConfigSource)
	assert.Equal(t, "img:resolved", a.AppliedConfig.Image)
	assert.Equal(t, "prof", a.AppliedConfig.Profile)
}

func TestApplyBrokerAgentConfig_EmptyValuesKeepRecorded(t *testing.T) {
	recorded := AgentAppliedConfig{
		HarnessConfig: "h", HarnessAuth: "a", HarnessConfigSource: "s", Image: "i", Profile: "p",
	}
	cfg := recorded
	a := &Agent{Template: "t", Runtime: "docker", AppliedConfig: &cfg}
	ApplyBrokerAgentConfig(a, BrokerAgentConfig{})
	assert.Equal(t, "t", a.Template)
	assert.Equal(t, "docker", a.Runtime)
	assert.Equal(t, recorded, *a.AppliedConfig)
}

func TestApplyBrokerAgentConfig_NilAppliedConfigSkipsAppliedFields(t *testing.T) {
	a := &Agent{}
	ApplyBrokerAgentConfig(a, fullBrokerAgentConfig())
	assert.Nil(t, a.AppliedConfig, "no AppliedConfig is created")
	assert.Equal(t, "tmpl", a.Template)
	assert.Equal(t, "kubernetes", a.Runtime)
}

func TestApplyBrokerAgentConfig_PinnedAgentKeepsProfile(t *testing.T) {
	a := &Agent{PinnedRuntimeTargetID: "target-1", AppliedConfig: &AgentAppliedConfig{Profile: "pinned"}}
	ApplyBrokerAgentConfig(a, fullBrokerAgentConfig())
	assert.Equal(t, "pinned", a.AppliedConfig.Profile)
	assert.Equal(t, "img:resolved", a.AppliedConfig.Image, "the other values are still written")
}
