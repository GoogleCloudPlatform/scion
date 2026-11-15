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
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// ephemeralCreateFixture is a project whose default (legacy) Runtime
// Broker advertises ephemeralPersistence when capable is set, with the
// ephemeral experiment set to gateOn.
func ephemeralCreateFixture(t *testing.T, capable, gateOn bool) *flatHubFixture {
	t.Helper()
	f := newFlatHubFixture(t, flatHubOpts{})
	if capable {
		f.legacy.Capabilities.EphemeralPersistence = true
		require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), f.legacy))
	}
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.CloudRunEphemeralAgents: gateOn})
	return f
}

func ephemeralCreateBody(name string, persistence string) map[string]interface{} {
	body := map[string]interface{}{"name": name, "task": "t"}
	if persistence != "" {
		body["config"] = map[string]interface{}{"persistence": persistence}
	}
	return body
}

// TestCreateAgent_Persistence covers the create-time resolution: refusals
// with the gate off or on a broker without the capability (no agent row,
// no dispatch), the stamp before the first dispatch, and an unchanged
// persistent create.
func TestCreateAgent_Persistence(t *testing.T) {
	t.Run("gate off refuses ephemeral", func(t *testing.T) {
		f := ephemeralCreateFixture(t, true, false)
		rec := f.create(t, ephemeralCreateBody("eph-off", "ephemeral"))
		d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		assert.Equal(t, experiments.CloudRunEphemeralAgents, d["experiment"])
		assert.Contains(t, decodeFlatAPIError(t, rec).Message, "(from the agent config)")
		assert.Nil(t, f.agentBySlug(t, "eph-off"))
		assert.Nil(t, f.client.lastCreateReq)
	})
	t.Run("broker without the capability refuses ephemeral", func(t *testing.T) {
		f := ephemeralCreateFixture(t, false, true)
		rec := f.create(t, ephemeralCreateBody("eph-docker", "ephemeral"))
		requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodePersistenceUnsupported)
		assert.Contains(t, decodeFlatAPIError(t, rec).Message, "(from the agent config) isn't available on Runtime Broker legacy-broker")
		assert.Nil(t, f.agentBySlug(t, "eph-docker"))
	})
	t.Run("gate on and capable: stamped and dispatched ephemeral", func(t *testing.T) {
		f := ephemeralCreateFixture(t, true, true)
		rec := f.create(t, ephemeralCreateBody("eph-on", "ephemeral"))
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		a := f.agentBySlug(t, "eph-on")
		require.NotNil(t, a)
		assert.Equal(t, "ephemeral", a.Annotations[annotationPersistence])
		assert.Equal(t, "the agent config", a.Annotations[annotationPersistenceSource])
		require.NotNil(t, f.client.lastCreateReq)
		assert.Equal(t, api.PersistenceEphemeral, f.client.lastCreateReq.Persistence)
	})
	t.Run("persistent create unchanged with the gate off", func(t *testing.T) {
		f := ephemeralCreateFixture(t, false, false)
		rec := f.create(t, ephemeralCreateBody("pers", ""))
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		a := f.agentBySlug(t, "pers")
		require.NotNil(t, a)
		_, stamped := a.Annotations[annotationPersistence]
		assert.False(t, stamped, "no stamp without a choice")
		require.NotNil(t, f.client.lastCreateReq)
		assert.Empty(t, f.client.lastCreateReq.Persistence)
	})
	t.Run("invalid value refused", func(t *testing.T) {
		f := ephemeralCreateFixture(t, true, true)
		rec := f.create(t, ephemeralCreateBody("eph-bad", "sometimes"))
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
}

// TestDispatch_PersistenceFromStamp: start and restart carry the stamp;
// an agent without one sends nothing.
func TestDispatch_PersistenceFromStamp(t *testing.T) {
	f := newEmptyPerAgentFixture(t, "persistence", true)
	f.agent.Annotations = map[string]string{annotationPersistence: "ephemeral"}
	require.NoError(t, f.dispatcher.DispatchAgentStart(context.Background(), f.agent, "", false))
	assert.Equal(t, api.PersistenceEphemeral, f.client.lastStartExtras.Persistence)
	require.NoError(t, f.dispatcher.DispatchAgentRestart(context.Background(), f.agent))
	assert.Equal(t, api.PersistenceEphemeral, f.client.lastRestartExtras.Persistence)
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{Persistence: api.PersistenceEphemeral})
	assert.Equal(t, api.PersistenceEphemeral, payload["persistence"], "wire key persistence")
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, sent := payload["persistence"]
	assert.False(t, sent, "a persistent agent sends no persistence key")

	g := newEmptyPerAgentFixture(t, "persistence-none", true)
	require.NoError(t, g.dispatcher.DispatchAgentStart(context.Background(), g.agent, "", false))
	assert.Empty(t, g.client.lastStartExtras.Persistence)
}
