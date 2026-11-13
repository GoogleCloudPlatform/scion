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

package brokerconvert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestConvertPlan_CloudRunOperatorInputExits3(t *testing.T) {
	const rawYAML = `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: planted-proj-c91
      location: us-central1
      service_account: planted-sa@planted-proj-c91.iam.gserviceaccount.com
      network: projects/planted-proj-c91/global/networks/planted-vpc
      subnetwork: projects/planted-proj-c91/regions/us-central1/subnetworks/planted-subnet
profiles:
  prod:
    runtime: cr
`
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal([]byte(rawYAML), vs))

	p, err := BuildLocal(Input{Settings: vs, RawSettings: []byte(rawYAML)})
	require.NoError(t, err)

	// Only finding is flat_target_operator_input_required (notice).
	require.Len(t, p.Findings, 1)
	f := p.Findings[0]
	assert.Equal(t, config.ConversionFlatTargetOperatorInputRequired, f.Code)
	assert.Equal(t, config.ConversionSeverityNotice, f.Severity)
	assert.Equal(t, "server.broker.instances[0].runtime_target.credentials_file", f.Path)
	assert.Equal(t, []string{"prod"}, f.Profiles)
	assert.NotEqual(t, "see the conversion rules for this finding code", f.Text)

	// Profile keeps its ProfileMap entry, DecisionRequired triggers ExitBlocked (3).
	require.Len(t, p.ProfileMap, 1)
	assert.Equal(t, ProfileMapping{
		Profile:  "prod",
		Instance: "cloudrun-planted-proj-c91-us-central1",
	}, p.ProfileMap[0])
	assert.Equal(t, 0, p.Summary.BlockingFindings)
	assert.Equal(t, 1, p.Summary.NoticeFindings)
	assert.Equal(t, 1, p.Summary.MappedProfiles)
	assert.True(t, p.Summary.DecisionRequired)
	assert.Equal(t, ResultDecisionsRequired, p.Summary.Result)
	assert.Equal(t, ExitBlocked, p.ExitCode())

	// Human output names the required operator-input field in Summary.
	human, js := render(t, p)
	summaryIdx := strings.Index(string(human), "\nSummary\n")
	require.True(t, summaryIdx >= 0, "Summary section present in human output")
	summarySection := string(human)[summaryIdx:]
	assert.Contains(t, summarySection, "operator input required: server.broker.instances[0].runtime_target.credentials_file")

	// JSON output keeps schemaVersion 1 and frozen top-level keys, and omits CR8 never-published values.
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(js, &doc))
	assert.JSONEq(t, `1`, string(doc["schemaVersion"]))
	for _, secretOrLocal := range []string{
		"planted-sa@planted-proj-c91.iam.gserviceaccount.com",
		"planted-vpc",
		"planted-subnet",
	} {
		assert.NotContains(t, string(js), secretOrLocal)
	}

	// Presentation snapshot keeps keyPath invariant and omits CR8 never-published values.
	pres := Presentation(p, testComputedAt)
	presJSON := mustJSON(t, pres)
	for _, secretOrLocal := range []string{
		"planted-sa@planted-proj-c91.iam.gserviceaccount.com",
		"planted-vpc",
		"planted-subnet",
	} {
		assert.NotContains(t, presJSON, secretOrLocal)
	}
	for _, pf := range pres.Findings {
		assert.Regexp(t, keyPath, pf.Path)
	}

	// convert --apply continues refusing Cloud Run plans via RefusePlanNotClean,
	// both with Hub sections populated and in the local stopped phase.
	pWithHub := *p
	pWithHub.LegacyBrokerID = "broker-legacy-1"
	pWithHub.Capacity = Capacity{Status: CapacityPlanned, Outcome: "no_limit"}
	pWithHub.Agents = &AgentsSection{}
	pWithHub.Projects = &ProjectsSection{}
	pWithHub.summarize()
	assert.True(t, pWithHub.Summary.DecisionRequired)
	assert.Equal(t, ExitBlocked, pWithHub.ExitCode())

	env := &Env{GlobalDir: t.TempDir()}
	_, rep, applyErr := startApply(env, ApplyInput{Plan: &pWithHub})
	require.Error(t, applyErr)
	require.NotNil(t, rep)
	require.NotNil(t, rep.Refusal)
	assert.Equal(t, RefusePlanNotClean, rep.Refusal.Code)

	_, localRep, localErr := startLocalApply(env, ApplyInput{Plan: p, LocalPlan: true})
	require.Error(t, localErr)
	require.NotNil(t, localRep)
	require.NotNil(t, localRep.Refusal)
	assert.Equal(t, RefusePlanNotClean, localRep.Refusal.Code)
}

func TestConvertPlan_CloudRunFieldRequiredBlocking(t *testing.T) {
	const rawYAML = `schema_version: "1"
runtimes:
  cri:
    type: cloudrun-instances
    cloudrun_instances:
      project_id: my-project
      region: us-central1
profiles:
  p:
    runtime: cri
`
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal([]byte(rawYAML), vs))

	p, err := BuildLocal(Input{Settings: vs, RawSettings: []byte(rawYAML)})
	require.NoError(t, err)

	assert.Equal(t, 2, p.Summary.BlockingFindings)
	assert.Equal(t, 1, p.Summary.NoticeFindings)
	assert.Equal(t, 0, p.Summary.MappedProfiles)
	assert.True(t, p.Summary.DecisionRequired)
	assert.Equal(t, ResultBlockingFindings, p.Summary.Result)
	assert.Equal(t, ExitBlocked, p.ExitCode())

	require.Len(t, p.ProfileMap, 1)
	assert.Equal(t, ProfileMapping{
		Profile:   "p",
		BlockedBy: config.ConversionFlatTargetFieldRequired,
	}, p.ProfileMap[0])
	assert.NotEqual(t, "see the conversion rules for this finding code", FindingText(config.ConversionFlatTargetFieldRequired))
}
