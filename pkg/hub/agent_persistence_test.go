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
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestResolveAgentPersistence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         persistenceInput
		want       api.Persistence
		wantSource string
		wantStatus int
		wantMsg    string
	}{
		{name: "none", in: persistenceInput{}, want: ""},
		{name: "agent persistent, gate off", in: persistenceInput{Request: "persistent"}, want: "persistent", wantSource: "the agent config"},
		{name: "agent ephemeral", in: persistenceInput{Request: "ephemeral", BrokerSupports: true, GateOn: true}, want: "ephemeral", wantSource: "the agent config"},
		{name: "template ephemeral", in: persistenceInput{Template: "ephemeral", TemplateName: "ci-runner", BrokerSupports: true, GateOn: true}, want: "ephemeral", wantSource: "template ci-runner"},
		{name: "agent beats template", in: persistenceInput{Request: "persistent", Template: "ephemeral", TemplateName: "ci-runner"}, want: "persistent", wantSource: "the agent config"},
		{name: "gate off", in: persistenceInput{Template: "ephemeral", TemplateName: "ci-runner", BrokerSupports: true},
			wantStatus: http.StatusPreconditionFailed, wantMsg: "Ephemeral persistence (from template ci-runner) is not enabled on this Hub (experiment hub.cloudrun_ephemeral_agents is off)"},
		{name: "broker lacks capability", in: persistenceInput{Template: "ephemeral", TemplateName: "ci-runner", BrokerName: "docker-local", GateOn: true},
			wantStatus: http.StatusUnprocessableEntity, wantMsg: "Ephemeral persistence (from template ci-runner) isn't available on Runtime Broker docker-local; it is supported on Cloud Run brokers only"},
		{name: "invalid", in: persistenceInput{Request: "maybe"}, wantStatus: http.StatusBadRequest, wantMsg: `invalid persistence "maybe": must be persistent or ephemeral (from the agent config)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, source, refusal := resolveAgentPersistence(tc.in)
			if tc.wantStatus != 0 {
				if assert.NotNil(t, refusal) {
					assert.Equal(t, tc.wantStatus, refusal.Status)
					assert.Equal(t, tc.wantMsg, refusal.Message)
				}
				return
			}
			assert.Nil(t, refusal)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

func TestPersistenceStamp(t *testing.T) {
	a := &store.Agent{}
	stampAgentPersistence(a, "", "")
	assert.Nil(t, a.Annotations, "persistent by default stamps nothing")
	assert.Equal(t, api.Persistence(""), dispatchPersistence(a))

	stampAgentPersistence(a, api.PersistencePersistent, "the agent config")
	assert.Equal(t, api.Persistence(""), dispatchPersistence(a), "a persistent stamp dispatches nothing")

	stampAgentPersistence(a, api.PersistenceEphemeral, "template ci-runner")
	assert.Equal(t, api.PersistenceEphemeral, dispatchPersistence(a))
	assert.Equal(t, "template ci-runner", a.Annotations[annotationPersistenceSource])

	// An annotations update can neither change, clear nor add the stamp.
	next := keepPersistenceStamp(a.Annotations, map[string]string{"x": "y", annotationPersistence: "persistent"})
	assert.Equal(t, map[string]string{"x": "y", annotationPersistence: "ephemeral", annotationPersistenceSource: "template ci-runner"}, next)
	next = keepPersistenceStamp(nil, map[string]string{annotationPersistence: "ephemeral", annotationPersistenceSource: "x"})
	assert.Empty(t, next)
}

func TestEphemeralPersistence_CapabilityAndDispatchGate(t *testing.T) {
	assert.True(t, capabilitiesFromStrings([]string{"ephemeralPersistence"}).EphemeralPersistence)
	assert.False(t, capabilitiesFromStrings([]string{"emptyPerAgentWorkspace"}).EphemeralPersistence)
	assert.True(t, brokerSupportsEphemeral(&store.RuntimeBroker{Capabilities: &store.BrokerCapabilities{EphemeralPersistence: true}}))
	assert.False(t, brokerSupportsEphemeral(nil))
	assert.True(t, slices.Contains(dispatchExperimentNames, experiments.CloudRunEphemeralAgents), "the broker gate needs the experiment forwarded")
}
