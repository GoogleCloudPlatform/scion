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

package cmd

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
)

// T12: CLI printMoveVerdict renders the "bundle" mode with size estimate and
// does not tag waived checks with (--force) or print the recreate loss warning.
func TestPrintMoveVerdict_BundleMode(t *testing.T) {
	var out bytes.Buffer
	printMoveVerdict(&out, &hubclient.MoveVerdict{
		Eligible:     true,
		SourceBroker: hubclient.MoveBrokerRef{ID: "b1", Name: "docker-a"},
		TargetBroker: hubclient.MoveBrokerRef{ID: "b2", Name: "docker-b"},
		Profile:      "default",
		RuntimeType:  "docker",
		TransferMode: "bundle",
		BundleEstimate: &hubclient.MoveBundleEstimate{
			WorkspaceBytes:   12 << 20,
			WorkspaceEntries: 300,
			HomeBytes:        4 << 20,
			HomeEntries:      50,
		},
		Checks: []hubclient.MoveCheck{
			{Name: "same_export", Result: "waived", Message: "not needed: workspace and home are copied through hub storage"},
			{Name: "workspace_bundleable", Result: "passed", Message: "clone-per-agent workspace and home can be bundled"},
			{Name: "bundle_source", Result: "passed", Message: "source runtime is durable (~16.0 MiB total)"},
		},
		Consequences: []hubclient.MoveConsequence{
			{Kind: "carried", Item: "workspace", Text: "copied through hub storage"},
			{Kind: "carried", Item: "home", Text: "copied through hub storage, except secrets"},
		},
	})

	got := out.String()
	assert.Contains(t, got, "bundle (workspace and home are copied through hub storage (~16.0 MiB))")
	assert.Contains(t, got, "same_export:")
	assert.NotContains(t, got, "waived (--force)")
	assert.NotContains(t, got, "will not survive this move")
	assert.NotContains(t, got, "bundle transfer unavailable:")
}

// T12: CLI printMoveVerdict prints "bundle transfer unavailable: <reason>" on
// a refused move, above the existing --force hint.
func TestPrintMoveVerdict_BundleUnavailableAboveForceHint(t *testing.T) {
	var out bytes.Buffer
	v := &hubclient.MoveVerdict{
		Eligible:          false,
		SourceBroker:      hubclient.MoveBrokerRef{ID: "b1", Name: "docker-a"},
		TargetBroker:      hubclient.MoveBrokerRef{ID: "b2", Name: "docker-b"},
		BundleUnavailable: "hub object storage is not configured",
		Checks: []hubclient.MoveCheck{
			{Name: "same_export", Result: "failed", Message: "brokers do not share an NFS export"},
		},
	}
	err := &apiclient.APIError{
		StatusCode: http.StatusConflict,
		Code:       "conflict",
		Message:    "brokers do not share an NFS export",
		Details:    map[string]any{"forceWouldApply": true},
	}

	printMoveVerdict(&out, v)
	printMoveForceHint(&out, err, false)

	got := out.String()
	unavailIdx := strings.Index(got, "bundle transfer unavailable: hub object storage is not configured")
	forceIdx := strings.Index(got, "A forced move does not preserve")
	assert.GreaterOrEqual(t, unavailIdx, 0, "expected bundle transfer unavailable line in %q", got)
	assert.Greater(t, forceIdx, unavailIdx, "bundle transfer unavailable must appear above --force hint")
}

// T12: checkHubSupportsForce accepts transferMode "bundle".
func TestCheckHubSupportsForce_BundleMode(t *testing.T) {
	resp := &hubclient.ReincarnateAgentResponse{
		SourceBrokerID: "b1",
		TargetBrokerID: "b2",
		MoveVerdict:    &hubclient.MoveVerdict{Eligible: true, TransferMode: "bundle"},
	}
	assert.NoError(t, checkHubSupportsForce(true, resp))
}

// T12 (O4): printMoveVerdict also prints "bundle transfer unavailable: <reason>"
// when --force falls through to an accepted recreate move.
func TestPrintMoveVerdict_ForcedRecreateShowsBundleUnavailable(t *testing.T) {
	var out bytes.Buffer
	printMoveVerdict(&out, &hubclient.MoveVerdict{
		Eligible:          true,
		SourceBroker:      hubclient.MoveBrokerRef{ID: "b1", Name: "docker-a"},
		TargetBroker:      hubclient.MoveBrokerRef{ID: "b2", Name: "docker-b"},
		TransferMode:      "recreate",
		BundleUnavailable: "hub object storage is not configured",
		Checks: []hubclient.MoveCheck{
			{Name: "same_export", Result: "waived"},
		},
	})
	got := out.String()
	assert.Contains(t, got, "Uncommitted and unpushed work in the agent's workspace will not survive this move.")
	assert.Contains(t, got, "bundle transfer unavailable: hub object storage is not configured")
}
