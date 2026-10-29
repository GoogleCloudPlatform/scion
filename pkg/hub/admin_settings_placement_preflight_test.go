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
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The server-config GET carries the placement pre-upgrade report of the
// stored runtimes and profiles: counts per category and the sections to
// repair, with no entry name or value. A Hub without a co-located legacy
// Runtime Broker reports its agents as not enumerated, never as clean.
func TestPlacementPreflightReportsCountsOnly(t *testing.T) {
	tempSettingsHome(t)
	srv, fakeStore, ops := newTestDBServer(t)
	fakeStore.seed("runtimes", json.RawMessage(`{
		"k8s": {"type": "kubernetes", "node_selector": {"scion.dev/pool": "agents"}},
		"desk": {"type": "docker", "runtime_class_name": "secret-class"},
		"camel": {"type": "kubernetes", "context": "c9", "nodeSelector": {"secret-key": "secret-value"}}
	}`))
	fakeStore.seed("profiles", json.RawMessage(`{
		"batch": {"runtime": "k8s", "node_selector": {"scion.dev/pool": "secret-pool"}},
		"dangling": {"runtime": "secret-runtime"}
	}`))
	_, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	resp := getServerConfigDB(t, srv, ops)
	p := resp.PlacementPreflight
	if p == nil {
		t.Fatal("no placement_preflight in the GET response")
	}
	want := map[string]int{
		string(config.SettingsMalformedPlacement):           1,
		string(config.SettingsInvalidPlacementField):        0,
		string(config.SettingsPlacementOnNonKubernetes):     1,
		string(config.SettingsProfileRuntimeMissing):        1,
		string(config.SettingsSiblingPlacementDisagreement): 0,
		string(config.SettingsPoolSelectorConflict):         1,
	}
	if !reflect.DeepEqual(p.SettingsCounts, want) {
		t.Fatalf("settings counts = %v, want %v", p.SettingsCounts, want)
	}
	if !reflect.DeepEqual(p.SettingsRepairSections, []string{"profiles", "runtimes"}) {
		t.Fatalf("repair sections = %v", p.SettingsRepairSections)
	}
	if p.AgentsScanned != 0 || len(p.AgentsNotEnumerated) != 1 || p.AgentsNotEnumerated[0] != placementPreflightNoColocated {
		t.Fatalf("agents = %+v", p)
	}
	if len(p.AgentCounts) != len(config.PlacementPreflightCategories) {
		t.Fatalf("agent counts = %v", p.AgentCounts)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"secret", "k8s", "batch", "desk", "camel", "dangling"} {
		if strings.Contains(string(body), v) {
			t.Fatalf("placement_preflight carries %q: %s", v, body)
		}
	}
}

// A PUT that sends the read-only report back (an echo of a GET body) has
// it dropped: the report is never a setting and never refuses the save.
func TestPlacementPreflightEchoIsDropped(t *testing.T) {
	tempSettingsHome(t)
	srv, _, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"default_model": "m1", "placement_preflight": {"settings_counts": {"settings_malformed_placement": 3}, "agents_scanned": 9}}`)
	if rr.Code != 200 {
		t.Fatalf("echo with the report: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "placement_preflight") {
		t.Fatalf("the report is reported as a saved or rejected key: %s", rr.Body.String())
	}
	if got := dropTopLevelMember([]byte(`{"Placement_Preflight": {}, "a": 1}`), placementPreflightKey); string(got) != `{"a":1}` {
		t.Fatalf("drop = %s", got)
	}
}
