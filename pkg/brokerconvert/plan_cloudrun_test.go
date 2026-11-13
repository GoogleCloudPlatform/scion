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
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestConvertPlan_CloudRunOperatorInputExits3(t *testing.T) {
	t.Run("cloudrun requires credentials_file operator input", func(t *testing.T) {
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
			Instance: "cloudrun-us-central1",
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
			"planted-proj-c91",
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
			"planted-proj-c91",
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
	})

	t.Run("cloudrun-instances requires credentials_file service_account and network operator input", func(t *testing.T) {
		const rawYAML = `schema_version: "1"
runtimes:
  cri:
    type: cloudrun-instances
    cloudrun_instances:
      project_id: planted-proj-c92
      region: us-central1
profiles:
  prod-inst:
    runtime: cri
`
		vs := &config.VersionedSettings{}
		require.NoError(t, yamlv3.Unmarshal([]byte(rawYAML), vs))

		p, err := BuildLocal(Input{Settings: vs, RawSettings: []byte(rawYAML)})
		require.NoError(t, err)

		require.Len(t, p.ProfileMap, 1)
		assert.Equal(t, ProfileMapping{
			Profile:  "prod-inst",
			Instance: "cloudrun-us-central1",
		}, p.ProfileMap[0])
		assert.Equal(t, 0, p.Summary.BlockingFindings)
		assert.Equal(t, 3, p.Summary.NoticeFindings)
		assert.Equal(t, 1, p.Summary.MappedProfiles)
		assert.True(t, p.Summary.DecisionRequired)
		assert.Equal(t, ResultDecisionsRequired, p.Summary.Result)
		assert.Equal(t, ExitBlocked, p.ExitCode())

		wantPaths := []string{
			"server.broker.instances[0].runtime_target.credentials_file",
			"server.broker.instances[0].runtime_target.network",
			"server.broker.instances[0].runtime_target.service_account",
		}
		require.Len(t, p.Findings, len(wantPaths))
		for i, wantPath := range wantPaths {
			assert.Equal(t, config.ConversionFlatTargetOperatorInputRequired, p.Findings[i].Code)
			assert.Equal(t, config.ConversionSeverityNotice, p.Findings[i].Severity)
			assert.Equal(t, wantPath, p.Findings[i].Path)
			assert.Equal(t, []string{"prod-inst"}, p.Findings[i].Profiles)
			assert.NotEqual(t, "see the conversion rules for this finding code", p.Findings[i].Text)
		}

		human, js := render(t, p)
		summaryIdx := strings.Index(string(human), "\nSummary\n")
		require.True(t, summaryIdx >= 0, "Summary section present in human output")
		summarySection := string(human)[summaryIdx:]
		for _, wantPath := range wantPaths {
			assert.Contains(t, summarySection, "operator input required: "+wantPath)
		}
		assert.NotContains(t, string(js), "planted-proj-c92")

		pres := Presentation(p, testComputedAt)
		assert.NotContains(t, mustJSON(t, pres), "planted-proj-c92")
		for _, pf := range pres.Findings {
			assert.Regexp(t, keyPath, pf.Path)
		}
	})
}

func TestConvertPlan_CloudRunFieldRequiredBlocking(t *testing.T) {
	const rawYAML = `schema_version: "1"
runtimes:
  cri:
    type: cloudrun-instances
    cloudrun_instances: {}
profiles:
  p:
    runtime: cri
`
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal([]byte(rawYAML), vs))

	p, err := BuildLocal(Input{Settings: vs, RawSettings: []byte(rawYAML)})
	require.NoError(t, err)

	assert.Equal(t, 2, p.Summary.BlockingFindings)
	assert.Equal(t, 3, p.Summary.NoticeFindings)
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

func TestProfileConversion_CloudRunKeyOmitsProjectID(t *testing.T) {
	const rawYAML = `schema_version: "1"
runtimes:
  cr-alpha:
    type: cloudrun
    cloudrun:
      project_id: proj-alpha
      location: us-central1
      service_account: agent-sa@proj-alpha.iam.gserviceaccount.com
      network: projects/proj-alpha/global/networks/agents-vpc
  cr-beta:
    type: cloudrun
    cloudrun:
      project_id: proj-beta
      location: us-central1
      service_account: agent-sa@proj-beta.iam.gserviceaccount.com
      network: projects/proj-beta/global/networks/agents-vpc
profiles:
  alpha:
    runtime: cr-alpha
  beta:
    runtime: cr-beta
`
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal([]byte(rawYAML), vs))

	p, err := BuildLocal(Input{Settings: vs, RawSettings: []byte(rawYAML)})
	require.NoError(t, err)
	require.Len(t, p.Instances, 2)

	keyA, keyB := p.Instances[0].Key, p.Instances[1].Key
	assert.NotEqual(t, keyA, keyB)
	suffixedRe := regexp.MustCompile(`^cloudrun-us-central1-[0-9a-f]{8}$`)
	assert.Regexp(t, suffixedRe, keyA)
	assert.Regexp(t, suffixedRe, keyB)
	for _, proj := range []string{"proj-alpha", "proj-beta"} {
		assert.NotContains(t, keyA, proj)
		assert.NotContains(t, keyB, proj)
	}

	presJSON := mustJSON(t, Presentation(p, testComputedAt))
	for _, proj := range []string{"proj-alpha", "proj-beta"} {
		assert.NotContains(t, presJSON, proj)
	}
}
