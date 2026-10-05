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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// errSpokeBus is an eventbus.EventBus whose Publish always returns err.
type errSpokeBus struct{ err error }

func (b errSpokeBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return b.err
}

func (errSpokeBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (errSpokeBus) Close() error { return nil }

var errPluginSpokeDown = errors.New("plugin spoke unavailable")

type outboundSpokeFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	user    *store.User
	agent   *store.Agent
	proxy   *MessageBrokerProxy
}

// newOutboundSpokeFixture wires the broker delivery path with the given
// inprocess spoke and a non-observer "web" plugin spoke that always fails.
func newOutboundSpokeFixture(t *testing.T, inproc eventbus.EventBus) *outboundSpokeFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "spoke-fail-project", Slug: "spoke-fail-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	user := &store.User{ID: api.NewUUID(), Email: "spoke-fail@example.com", DisplayName: "Spoke Fail"}
	require.NoError(t, s.CreateUser(ctx, user))
	agent := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "spoke-fail-agent",
		Slug:            "spoke-fail-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "test-broker",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: errSpokeBus{err: errPluginSpokeDown}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)

	return &outboundSpokeFixture{srv: srv, store: s, project: project, user: user, agent: agent, proxy: proxy}
}

func (f *outboundSpokeFixture) send(t *testing.T, msg string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{Recipient: "user:" + f.user.Email, Msg: msg})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.agent.ID},
		ProjectID: f.project.ID,
	}}))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.agent.ID)
	return rr
}

func (f *outboundSpokeFixture) storedRows(t *testing.T) int {
	t.Helper()
	res, err := f.store.ListMessages(context.Background(), store.MessageFilter{
		ProjectID:   f.project.ID,
		RecipientID: f.user.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	return len(res.Items)
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureIsDelivered covers
// ptone/scion#2757: when only a non-observer plugin spoke fails, the
// inprocess spoke has already queued the persisting deliverToUser, so the
// handler must report success (a retry would duplicate the stored row).
func TestHandleAgentOutboundMessage_PluginSpokeFailureIsDelivered(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)
	require.True(t, f.proxy.subscribeProjectUserMessages(f.project.ID))

	rr := f.send(t, "hello despite a failing plugin")
	require.Equal(t, http.StatusOK, rr.Code, "handler response: %s", rr.Body.String())

	deadline := time.Now().Add(3 * time.Second)
	for f.storedRows(t) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Give any duplicate write a chance to land before counting.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, f.storedRows(t), "expected exactly one stored row")
}

// TestHandleAgentOutboundMessage_InProcessFailureStillFails pins that a
// failure of the hub's own inprocess spoke is still reported as 502: the
// message was not stored, so the sender must see the failure.
func TestHandleAgentOutboundMessage_InProcessFailureStillFails(t *testing.T) {
	f := newOutboundSpokeFixture(t, errSpokeBus{err: eventbus.ErrEventBusClosed})

	rr := f.send(t, "hello into a closed bus")
	require.Equal(t, http.StatusBadGateway, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeDeliveryFailed, resp.Error.Code)
	require.Equal(t, 0, f.storedRows(t))
}

// TestHandleAgentOutboundMessage_InProcessBufferFullWithPluginFailure pins
// that a full inprocess subscriber buffer still maps to 503 when a plugin
// spoke fails in the same publish (ptone/scion#2311 behaviour unchanged).
func TestHandleAgentOutboundMessage_InProcessBufferFullWithPluginFailure(t *testing.T) {
	f := newOutboundSpokeFixture(t, alwaysDropUserBus{})

	rr := f.send(t, "hello into a full buffer")
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeUnavailable, resp.Error.Code)
}
