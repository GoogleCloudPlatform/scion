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

//go:build !no_sqlite

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Reported flat instance placement (node selector, tolerations, runtime
// class and the placementReported marker) is stored as the instance sent
// it: the Hub copies the marker verbatim and never sets it.

func placedPresentation(reported bool) *api.RuntimeTargetPresentation {
	p := dockerPresentation("placed")
	p.Target.NodeSelector = map[string]string{"scion.dev/pool": "agents", "disk": "ssd"}
	p.Target.Tolerations = []api.K8sToleration{{Key: "scion.dev/pool", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}}
	p.Target.RuntimeClassName = "gvisor"
	p.Target.PlacementReported = reported
	return p
}

// A presentation without the marker is stored without it, even when it
// carries placement fields; one with the marker keeps it. The stored
// placement equals what was sent.
func TestHubDoesNotSynthesizePlacementReported(t *testing.T) {
	f := newPresentationFixture(t, true)
	target := f.flat.RuntimeTarget.ID

	unmarked := placedPresentation(false)
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(unmarked, target, 100, 1, nil))
	row := f.row(t, f.flat.ID)
	require.NotNil(t, row.RuntimeTargetPresentation)
	assert.False(t, row.RuntimeTargetPresentation.Target.PlacementReported, "the Hub must not set the marker")
	assert.Equal(t, unmarked, row.RuntimeTargetPresentation)

	marked := placedPresentation(true)
	sendAsBroker(t, f.srv, f.flat.ID, presentationBeat(marked, target, 100, 2, nil))
	row = f.row(t, f.flat.ID)
	assert.True(t, row.RuntimeTargetPresentation.Target.PlacementReported)
	assert.Equal(t, marked, row.RuntimeTargetPresentation)
}

// The Hub's presentation hash covers the placement fields and the marker:
// equal presentations hash equally whatever the map insertion order, and
// changing the marker or a placement value changes the hash.
func TestPlacementReportedHashStable(t *testing.T) {
	a := placedPresentation(true)
	b := placedPresentation(true)
	b.Target.NodeSelector = map[string]string{"disk": "ssd"}
	b.Target.NodeSelector["scion.dev/pool"] = "agents"
	assert.Equal(t, mustHash(t, a), mustHash(t, b))

	unmarked := placedPresentation(false)
	assert.NotEqual(t, mustHash(t, a), mustHash(t, unmarked), "the marker is part of the hash")

	changed := placedPresentation(true)
	changed.Target.RuntimeClassName = "kata"
	assert.NotEqual(t, mustHash(t, a), mustHash(t, changed))

	reportedEmpty := dockerPresentation("placed")
	reportedEmpty.Target.PlacementReported = true
	assert.NotEqual(t, mustHash(t, dockerPresentation("placed")), mustHash(t, reportedEmpty),
		"reported empty placement and not reported hash differently")
}
