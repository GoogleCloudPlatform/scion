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
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The planted_split.yaml fixture has two Kubernetes profiles on one target
// that differ in shared_dir_size, a harness override image and a harness
// env value, with a planted context and kubeconfig path on the runtime.
// The first two are publishable (config.PresentInstance shows them in the split instances'
// presentations); the others never are.
var (
	plantedSplitUnpublished = []string{
		"PLANTED-CONTEXT-c55", "PLANTED-KUBECONFIG-c56",
		"PLANTED-ENV-TOKEN-A-c57", "PLANTED-ENV-TOKEN-B-c58",
	}
	plantedSplitPublished = []string{
		"731Gi", "739Gi",
		"planted-image-a-c53", "planted-image-b-c54",
	}
)

var testComputedAt = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func plantedSplitPresentation(t *testing.T) (*Plan, *api.ConversionPlanPresentation) {
	t.Helper()
	p, err := BuildLocal(loadInput(t, "planted_split.yaml"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(p.Instances), 2, "the profiles are split")
	return p, Presentation(p, testComputedAt)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// keyPath is the shape of a settings key path: dot-separated keys, with
// "*" for a profile set and [i] for a list index. No spaces, no slashes,
// no colons, so no image reference, size or file path fits.
var keyPath = regexp.MustCompile(`^([A-Za-z0-9_.*\-]+(\[[0-9]+\])?)*$`)

// TestPresentation_PlantedValues: a finding's path is a settings key path and
// never a value. No non-publishable planted value (context, kubeconfig, env
// value) appears anywhere in the snapshot; the publishable planted values
// (shared_dir_size, image) appear only inside instances[], where
// config.PresentInstance publishes them, never in findings or the profile map.
func TestPresentation_PlantedValues(t *testing.T) {
	p, pres := plantedSplitPresentation(t)
	all := mustJSON(t, pres)
	for _, v := range plantedSplitUnpublished {
		assert.NotContains(t, all, v, "non-publishable value in the snapshot")
	}
	assert.NotContains(t, all, "PLANTED-", "no planted value of any kind")

	findings := mustJSON(t, pres.Findings)
	profileMap := mustJSON(t, pres.ProfileMap)
	instances := mustJSON(t, pres.Instances)
	for _, v := range plantedSplitPublished {
		assert.NotContains(t, findings, v, "a publishable value in findings")
		assert.NotContains(t, profileMap, v, "a publishable value in the profile map")
		assert.Contains(t, instances, v, "config.PresentInstance publishes it in the split instance")
	}

	require.Len(t, pres.Findings, len(p.Findings))
	for i, f := range pres.Findings {
		assert.Equal(t, p.Findings[i].Path, f.Path, "the converter's key path, unchanged")
		assert.Regexp(t, keyPath, f.Path)
	}
}

// TestPresentation_InstancesByteEqualPresentInstance: every instance in the
// snapshot is exactly what a flat Runtime Broker would publish for the same
// instance, so the plan never publishes more.
func TestPresentation_InstancesByteEqualPresentInstance(t *testing.T) {
	p, pres := plantedSplitPresentation(t)
	require.Len(t, pres.Instances, len(p.settingsInstances))
	for i, inst := range p.settingsInstances {
		assert.Equal(t, mustJSON(t, config.PresentInstance(inst).API()), mustJSON(t, pres.Instances[i]))
	}
}

// TestPresentation_FindingsCarryNoMessage: a finding is code, severity,
// path and profiles; neither the converter message nor the fixed text.
func TestPresentation_FindingsCarryNoMessage(t *testing.T) {
	p, pres := plantedSplitPresentation(t)
	require.NotEmpty(t, pres.Findings)
	var doc struct {
		Findings []map[string]json.RawMessage `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(mustJSON(t, pres)), &doc))
	for _, f := range doc.Findings {
		keys := make([]string, 0, len(f))
		for k := range f {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"code", "severity", "path", "profiles"}, keys)
	}
	all := mustJSON(t, pres)
	for _, f := range p.Findings {
		assert.NotContains(t, all, f.Text, "the fixed text is the reader's, never sent")
	}
}

func TestPresentation_ProfileMapHoldsMappedProfilesOnly(t *testing.T) {
	p, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	pres := Presentation(p, testComputedAt)
	for _, m := range p.ProfileMap {
		got, ok := pres.ProfileMap[m.Profile]
		if m.Instance == "" {
			assert.False(t, ok, "%s is blocked and has no entry", m.Profile)
			continue
		}
		assert.Equal(t, m.Instance, got)
	}
	assert.Equal(t, api.ConversionPlanPresentationSchemaVersion, pres.SchemaVersion)
	assert.Equal(t, testComputedAt, pres.ComputedAt)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(mustJSON(t, pres))))
	assert.False(t, hasTruncated(pres), "an in-cap plan is not truncated")
}

func TestPresentation_EmptyPlanIsWellFormed(t *testing.T) {
	pres := Presentation(&Plan{}, testComputedAt)
	assert.Equal(t, `{"schemaVersion":1,"instances":[],"profileMap":{},"findings":[],"computedAt":"2026-11-03T12:00:00Z"}`, mustJSON(t, pres))
}

func hasTruncated(pres *api.ConversionPlanPresentation) bool {
	for _, f := range pres.Findings {
		if f.Code == api.ConversionPlanTruncatedCode {
			return true
		}
	}
	return false
}

func truncatedIsBlocking(t *testing.T, pres *api.ConversionPlanPresentation) {
	t.Helper()
	n := 0
	for _, f := range pres.Findings {
		if f.Code == api.ConversionPlanTruncatedCode {
			n++
			assert.Equal(t, config.ConversionSeverityBlocking, f.Severity)
			assert.Empty(t, f.Path)
			assert.Empty(t, f.Profiles)
		}
	}
	assert.Equal(t, 1, n, "exactly one plan_truncated finding")
}

func syntheticInstance(i int, pad string) api.RuntimeTargetPresentation {
	return api.RuntimeTargetPresentation{
		SchemaVersion: api.RuntimeTargetPresentationSchemaVersion,
		Key:           fmt.Sprintf("inst-%03d", i),
		Name:          fmt.Sprintf("inst-%03d", i),
		Target:        &api.PresentedTarget{Type: "kubernetes", DisplayName: pad},
	}
}

func TestPresentation_CountCapsTrimAndMarkBlocking(t *testing.T) {
	p := &Plan{}
	for i := 0; i < api.ConversionPlanMaxInstances+6; i++ {
		p.Instances = append(p.Instances, syntheticInstance(i, ""))
	}
	for i := 0; i < api.ConversionPlanMaxProfileMap+40; i++ {
		p.ProfileMap = append(p.ProfileMap, ProfileMapping{Profile: fmt.Sprintf("p%03d", i), Instance: "inst-000"})
	}
	// One blocking finding first, then more notices than the cap allows.
	p.Findings = append(p.Findings, Finding{Code: "unsupported_flat_target_type", Severity: config.ConversionSeverityBlocking, Path: "profiles.x.runtime"})
	for i := 0; i < api.ConversionPlanMaxFindings+20; i++ {
		p.Findings = append(p.Findings, Finding{Code: "legacy_field_no_effect", Severity: config.ConversionSeverityNotice, Path: fmt.Sprintf("profiles.p%03d.env", i)})
	}
	pres := Presentation(p, testComputedAt)
	assert.Len(t, pres.Instances, api.ConversionPlanMaxInstances)
	assert.LessOrEqual(t, len(pres.ProfileMap), api.ConversionPlanMaxProfileMap)
	assert.LessOrEqual(t, len(pres.Findings), api.ConversionPlanMaxFindings)
	assert.Equal(t, "unsupported_flat_target_type", pres.Findings[0].Code, "notices are dropped before the blocking finding")
	truncatedIsBlocking(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(mustJSON(t, pres))))
}

// TestPresentation_OverByteCapKeepsBlockingOrMarksIt: an over-cap plan that
// holds a blocking finding keeps it or carries a blocking plan_truncated.
// Notices go before blocking findings, and findings before instances.
func TestPresentation_OverByteCapKeepsBlockingOrMarksIt(t *testing.T) {
	pad := strings.Repeat("x", 240)
	p := &Plan{}
	for i := 0; i < api.ConversionPlanMaxInstances; i++ {
		p.Instances = append(p.Instances, syntheticInstance(i, pad))
	}
	p.Findings = append(p.Findings, Finding{Code: "flat_target_scope_violation", Severity: config.ConversionSeverityBlocking, Path: "runtimes.cluster", Profiles: []string{"a"}})
	for i := 0; i < 300; i++ {
		p.Findings = append(p.Findings, Finding{Code: "legacy_field_no_effect", Severity: config.ConversionSeverityNotice, Path: "profiles.p." + pad[:200], Profiles: []string{fmt.Sprintf("p%03d", i)}})
	}
	pres := Presentation(p, testComputedAt)
	enc := mustJSON(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(enc)))
	assert.LessOrEqual(t, len(enc), api.ConversionPlanMaxBytes)
	truncatedIsBlocking(t, pres)
	assert.Equal(t, "flat_target_scope_violation", pres.Findings[0].Code, "the blocking finding outlives the notices")
	assert.Less(t, len(pres.Findings), len(p.Findings)+1, "notices were dropped")
	assert.Len(t, pres.Instances, api.ConversionPlanMaxInstances, "findings are dropped before instances")

	// Instances alone over the byte cap: every finding goes first, then
	// instances; the plan still carries the blocking plan_truncated.
	big := &Plan{Findings: p.Findings[:2]}
	for i := 0; i < api.ConversionPlanMaxInstances; i++ {
		inst := syntheticInstance(i, pad)
		inst.Target.Namespace, inst.Target.PriorityClassName, inst.Target.SharedDirStorageClass = pad, pad, pad
		big.Instances = append(big.Instances, inst)
	}
	pres = Presentation(big, testComputedAt)
	enc = mustJSON(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(enc)))
	assert.Less(t, len(pres.Instances), api.ConversionPlanMaxInstances, "instances were dropped")
	for _, f := range pres.Findings {
		assert.Equal(t, config.ConversionSeverityBlocking, f.Severity, "notices and findings went before instances")
	}
	truncatedIsBlocking(t, pres)
}

func TestPresentation_OverLongStringDropsWholeElement(t *testing.T) {
	long := strings.Repeat("y", api.ConversionPlanMaxStringBytes+1)
	p := &Plan{
		Instances: []api.RuntimeTargetPresentation{syntheticInstance(0, ""), syntheticInstance(1, long)},
		ProfileMap: []ProfileMapping{
			{Profile: "a", Instance: "inst-000"},
			{Profile: "b", Instance: "inst-001"},
			{Profile: long, Instance: "inst-000"},
		},
		Findings: []Finding{
			{Code: "profile_conflict", Severity: config.ConversionSeverityNotice, Path: "profiles.*"},
			{Code: "unknown_settings_key", Severity: config.ConversionSeverityBlocking, Path: "profiles.a." + long},
		},
	}
	pres := Presentation(p, testComputedAt)
	enc := mustJSON(t, pres)
	assert.NotContains(t, enc, long[:api.ConversionPlanMaxStringBytes], "strings are never cut, their elements are dropped")
	require.Len(t, pres.Instances, 1)
	assert.Equal(t, "inst-000", pres.Instances[0].Key)
	assert.Equal(t, map[string]string{"a": "inst-000"}, pres.ProfileMap, "the dropped instance's mappings go with it")
	require.Len(t, pres.Findings, 2)
	assert.Equal(t, "profile_conflict", pres.Findings[0].Code)
	truncatedIsBlocking(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(enc)))
}

func planWithNotices(n int) *Plan {
	p := &Plan{}
	for i := 0; i < n; i++ {
		p.Findings = append(p.Findings, Finding{Code: "legacy_field_no_effect", Severity: config.ConversionSeverityNotice, Path: fmt.Sprintf("profiles.p%03d.env", i)})
	}
	return p
}

// TestPresentation_FindingsCapBoundary: findings are trimmed only above the
// cap. 511 and 512 findings are sent whole with no plan_truncated; 513 are
// trimmed to 511 plus the blocking marker (512 in all).
func TestPresentation_FindingsCapBoundary(t *testing.T) {
	for _, n := range []int{api.ConversionPlanMaxFindings - 1, api.ConversionPlanMaxFindings} {
		pres := Presentation(planWithNotices(n), testComputedAt)
		assert.Len(t, pres.Findings, n, "%d findings fit the cap", n)
		assert.False(t, hasTruncated(pres), "%d findings: nothing was truncated", n)
		require.NoError(t, api.CheckConversionPlanCaps([]byte(mustJSON(t, pres))))
	}
	pres := Presentation(planWithNotices(api.ConversionPlanMaxFindings+1), testComputedAt)
	assert.Len(t, pres.Findings, api.ConversionPlanMaxFindings)
	truncatedIsBlocking(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(mustJSON(t, pres))))
}

// TestPresentation_TruncatedAtFindingsCapMakesRoomForMarker: a plan trimmed
// for another reason (an over-long instance string) that holds exactly the
// cap of findings still fits the cap once plan_truncated is added.
func TestPresentation_TruncatedAtFindingsCapMakesRoomForMarker(t *testing.T) {
	p := planWithNotices(api.ConversionPlanMaxFindings)
	p.Instances = []api.RuntimeTargetPresentation{syntheticInstance(0, strings.Repeat("z", api.ConversionPlanMaxStringBytes+1))}
	pres := Presentation(p, testComputedAt)
	assert.Empty(t, pres.Instances, "the instance with the over-long string is dropped")
	assert.Len(t, pres.Findings, api.ConversionPlanMaxFindings)
	truncatedIsBlocking(t, pres)
	require.NoError(t, api.CheckConversionPlanCaps([]byte(mustJSON(t, pres))))
}
