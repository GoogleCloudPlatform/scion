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

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestHubAgentDefaults_ImageTag: the hub-resolved image tag counts as a
// default (a value with only the tag is not empty and rides the context),
// the accessor is nil-safe, and the JSON field is omitted when unset, so a
// dispatch without a tag is byte-identical (ptone/scion#6625).
func TestHubAgentDefaults_ImageTag(t *testing.T) {
	var nilDefaults *HubAgentDefaults
	if got := nilDefaults.ResolvedImageTag(); got != "" {
		t.Errorf("nil ResolvedImageTag = %q, want empty", got)
	}
	if got := HubAgentDefaultsFromContext(context.Background()).ResolvedImageTag(); got != "" {
		t.Errorf("no defaults on the context: ResolvedImageTag = %q, want empty", got)
	}

	d := &HubAgentDefaults{ImageTag: "v0.3.0"}
	if d.IsEmpty() {
		t.Error("a value with only ImageTag must not be empty")
	}
	if got := HubAgentDefaultsFromContext(ContextWithHubAgentDefaults(context.Background(), d)).ResolvedImageTag(); got != "v0.3.0" {
		t.Errorf("ResolvedImageTag from context = %q, want v0.3.0", got)
	}

	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != `{"imageTag":"v0.3.0"}` {
		t.Errorf("JSON = %s, want {\"imageTag\":\"v0.3.0\"}", blob)
	}
	blob, err = json.Marshal(&HubAgentDefaults{MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "imageTag") {
		t.Errorf("an unset tag must be omitted: %s", blob)
	}
	if !(&HubAgentDefaults{}).IsEmpty() {
		t.Error("the zero value must be empty")
	}
}
