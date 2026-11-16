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
      block_service_account: planted-block@planted-proj-c91.iam.gserviceaccount.com
      assignable_service_accounts:
        - planted-worker-1@planted-proj-c91.iam.gserviceaccount.com
        - planted-worker-2@planted-proj-c91.iam.gserviceaccount.com
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

		// Only finding is flat_target_operator_input_required (notice) for credentials_file.
		wantPaths := []string{
			"server.broker.instances[0].runtime_target.credentials_file",
		}
		require.Len(t, p.Findings, len(wantPaths))
		for i, wantPath := range wantPaths {
			assert.Equal(t, config.ConversionFlatTargetOperatorInputRequired, p.Findings[i].Code)
			assert.Equal(t, config.ConversionSeverityNotice, p.Findings[i].Severity)
			assert.Equal(t, wantPath, p.Findings[i].Path)
			assert.Equal(t, []string{"prod"}, p.Findings[i].Profiles)
			assert.NotEqual(t, "see the conversion rules for this finding code", p.Findings[i].Text)
		}

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

		// Human output keeps folded identity values in the paste-ready YAML block,
		// names credentials_file in Summary, and never leaks SA values in Findings.
		human, js := render(t, p)
		yamlSection := section(t, string(human), "1. Proposed instances", "2. Profile map")
		assert.Contains(t, yamlSection, "block_service_account: planted-block@planted-proj-c91.iam.gserviceaccount.com")
		assert.Contains(t, yamlSection, "planted-worker-1@planted-proj-c91.iam.gserviceaccount.com")
		assert.Contains(t, yamlSection, "planted-worker-2@planted-proj-c91.iam.gserviceaccount.com")
		findingsSection := section(t, string(human), "3. Findings", "4. Capacity")
		assert.NotContains(t, findingsSection, "planted-")
		summaryIdx := strings.Index(string(human), "\nSummary\n")
		require.True(t, summaryIdx >= 0, "Summary section present in human output")
		summarySection := string(human)[summaryIdx:]
		for _, wantPath := range wantPaths {
			assert.Contains(t, summarySection, "operator input required: "+wantPath)
		}
		assert.NotContains(t, summarySection, "block_service_account")

		// JSON output keeps schemaVersion 1 and frozen top-level keys, and omits CR8 never-published values.
		var doc map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(js, &doc))
		assert.JSONEq(t, `1`, string(doc["schemaVersion"]))
		neverPublished := []string{
			"planted-proj-c91",
			"planted-sa@planted-proj-c91.iam.gserviceaccount.com",
			"planted-block@planted-proj-c91.iam.gserviceaccount.com",
			"planted-worker-1@planted-proj-c91.iam.gserviceaccount.com",
			"planted-worker-2@planted-proj-c91.iam.gserviceaccount.com",
			"planted-vpc",
			"planted-subnet",
		}
		for _, secretOrLocal := range neverPublished {
			assert.NotContains(t, string(js), secretOrLocal)
		}

		// Presentation snapshot keeps keyPath invariant and omits CR8 never-published values.
		pres := Presentation(p, testComputedAt)
		presJSON := mustJSON(t, pres)
		for _, secretOrLocal := range neverPublished {
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
		assert.Equal(t, 4, p.Summary.NoticeFindings)
		assert.Equal(t, 1, p.Summary.MappedProfiles)
		assert.True(t, p.Summary.DecisionRequired)
		assert.Equal(t, ResultDecisionsRequired, p.Summary.Result)
		assert.Equal(t, ExitBlocked, p.ExitCode())

		wantPaths := []string{
			"server.broker.instances[0].runtime_target.block_service_account",
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
	assert.Equal(t, 4, p.Summary.NoticeFindings)
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

	t.Run("cloudrun missing block_service_account or invalid assignable_service_accounts blocks without leaking emails", func(t *testing.T) {
		const crInvalidYAML = `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: planted-proj-c93
      location: us-central1
      service_account: planted-sa@planted-proj-c93.iam.gserviceaccount.com
      assignable_service_accounts:
        - Planted-Upper@planted-proj-c93.iam.gserviceaccount.com
      network: projects/planted-proj-c93/global/networks/planted-vpc
profiles:
  p:
    runtime: cr
`
		vsInvalid := &config.VersionedSettings{}
		require.NoError(t, yamlv3.Unmarshal([]byte(crInvalidYAML), vsInvalid))
		pInvalid, err := BuildLocal(Input{Settings: vsInvalid, RawSettings: []byte(crInvalidYAML)})
		require.NoError(t, err)

		assert.Equal(t, 2, pInvalid.Summary.BlockingFindings)
		assert.Equal(t, 1, pInvalid.Summary.NoticeFindings)
		assert.Equal(t, 0, pInvalid.Summary.MappedProfiles)
		assert.Equal(t, ResultBlockingFindings, pInvalid.Summary.Result)
		assert.Equal(t, ExitBlocked, pInvalid.ExitCode())
		assert.Equal(t, ProfileMapping{Profile: "p", BlockedBy: config.ConversionFlatTargetFieldRequired}, pInvalid.ProfileMap[0])

		var blockingPaths []string
		for _, f := range pInvalid.Findings {
			assert.Regexp(t, keyPath, f.Path)
			if f.Severity == config.ConversionSeverityBlocking {
				assert.Equal(t, config.ConversionFlatTargetFieldRequired, f.Code)
				blockingPaths = append(blockingPaths, f.Path)
			}
		}
		assert.Equal(t, []string{
			"runtimes.cr.cloudrun.assignable_service_accounts[0]",
			"runtimes.cr.cloudrun.block_service_account",
		}, blockingPaths)

		_, js := render(t, pInvalid)
		presJSON := mustJSON(t, Presentation(pInvalid, testComputedAt))
		for _, secretOrLocal := range []string{
			"planted-proj-c93",
			"planted-sa@planted-proj-c93.iam.gserviceaccount.com",
			"Planted-Upper@planted-proj-c93.iam.gserviceaccount.com",
		} {
			assert.NotContains(t, string(js), secretOrLocal)
			assert.NotContains(t, presJSON, secretOrLocal)
		}
	})
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
      block_service_account: no-role-alpha@proj-alpha.iam.gserviceaccount.com
      assignable_service_accounts:
        - worker-alpha@proj-alpha.iam.gserviceaccount.com
      network: projects/proj-alpha/global/networks/agents-vpc
  cr-beta:
    type: cloudrun
    cloudrun:
      project_id: proj-beta
      location: us-central1
      service_account: agent-sa@proj-beta.iam.gserviceaccount.com
      block_service_account: no-role-beta@proj-beta.iam.gserviceaccount.com
      assignable_service_accounts:
        - worker-beta@proj-beta.iam.gserviceaccount.com
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
	for _, tok := range []string{"proj-alpha", "proj-beta", "agent-sa@", "no-role-", "worker-"} {
		assert.NotContains(t, keyA, tok)
		assert.NotContains(t, keyB, tok)
	}

	presJSON := mustJSON(t, Presentation(p, testComputedAt))
	for _, tok := range []string{"proj-alpha", "proj-beta", "agent-sa@", "no-role-", "worker-"} {
		assert.NotContains(t, presJSON, tok)
	}
}
