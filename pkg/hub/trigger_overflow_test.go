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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type overflowFakeBrokerClient struct {
	RuntimeBrokerClient
	mu           sync.Mutex
	createdReqs  []*RemoteCreateAgentRequest
	sentMessages []*messages.StructuredMessage
}

func (b *overflowFakeBrokerClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	b.mu.Lock()
	b.createdReqs = append(b.createdReqs, req)
	b.mu.Unlock()
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:       req.ID,
			Slug:     req.Slug,
			Name:     req.Name,
			Phase:    string(state.PhaseRunning),
			Activity: string(state.ActivityIdle),
		},
	}, nil
}

func (b *overflowFakeBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	resp, err := b.CreateAgent(ctx, brokerID, endpoint, req)
	return resp, nil, err
}

func (b *overflowFakeBrokerClient) MessageAgent(_ context.Context, _, _, _, _, _ string, _ bool, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	if msg != nil {
		cp := *msg
		b.sentMessages = append(b.sentMessages, &cp)
	}
	b.mu.Unlock()
	return nil
}

func makeOverflowTestJSON(targetBytes int) []byte {
	const prefix = `{"marker":"s2-p5-acceptance","data":"`
	const suffix = `"}`
	pad := targetBytes - len(prefix) - len(suffix)
	if pad < 0 {
		pad = 0
	}
	var b bytes.Buffer
	b.Grow(targetBytes)
	b.WriteString(prefix)
	b.WriteString(strings.Repeat("x", pad))
	b.WriteString(suffix)
	return b.Bytes()
}

// TestTriggerOverflow_AcceptanceItem10_EndToEnd verifies Stage 2 §8 / §16 item 10:
//   - 900 KiB body on a 1 MiB endpoint is accepted and stored out of line in pkg/storage;
//   - GET .../trigger-events/{id}?payload=1 streams the exact 900 KiB body under trigger.read authz;
//   - CEL has(event.payload) is false while event.attributes.payload_bytes/payload_ref match;
//   - message envelope AND dispatch_agent ~/.scion/trigger-event.json carry external_payload
//     with first 16 KiB in text, truncated: true, bytes: N;
//   - 64 KiB body behaviour is unchanged (stored inline);
//   - metrics report accepted_overflow and payloadBytes histogram;
//   - purge deletes the overflow object with its row;
//   - per-project byte budget overflow returns 429 "queue full".
func TestTriggerOverflow_AcceptanceItem10_EndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "s2-p5-overflow")
	enableTriggersExperimentForTest(t, f.srv, true)

	// Configure local storage on the hub server.
	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "hub-overflow-test",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	f.srv.SetStorage(localStor)

	// Install fake Runtime Broker supporting HomeFiles and capturing messages + created agents.
	fakeBroker := &overflowFakeBrokerClient{}
	f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, fakeBroker, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

	// Create a running target agent for the message trigger.
	targetAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "overflow-msg-target",
		Name:            "overflow-msg-target",
		ProjectID:       f.proj.ID,
		OwnerID:         f.creator.ID,
		RuntimeBrokerID: f.broker.ID,
		Phase:           string(state.PhaseRunning),
		Activity:        string(state.ActivityIdle),
	}
	require.NoError(t, f.store.CreateAgent(ctx, targetAg))

	ownerIdent := NewAuthenticatedUser(f.creator.ID, f.creator.Email, f.creator.DisplayName, f.creator.Role, "web")
	ownerCtx := contextWithCredentialContext(contextWithIdentity(ctx, ownerIdent), credentialContextForIdentity(ownerIdent))

	// Also create a user without trigger.read to verify ?payload=1 authz.
	viewer := &store.User{
		ID:          uuid.NewString(),
		Email:       "viewer-overflow@example.com",
		DisplayName: "Viewer",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, viewer))
	viewerIdent := NewAuthenticatedUser(viewer.ID, viewer.Email, viewer.DisplayName, viewer.Role, "web")
	viewerCtx := contextWithCredentialContext(contextWithIdentity(ctx, viewerIdent), credentialContextForIdentity(viewerIdent))

	doJSON := func(authCtx context.Context, method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(authCtx)
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	// 1. Create a 1 MiB webhook endpoint.
	epRec := doJSON(ownerCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", f.proj.ID),
		map[string]any{
			"name":         "large-payload-ep",
			"maxBodyBytes": 1024 * 1024,
		})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))
	assert.Equal(t, 1024*1024, ep.MaxBodyBytes)

	// 2. Create three triggers on this endpoint:
	//    (a) CEL `has(event.payload)` -> must NOT fire on overflow event.
	//    (b) message trigger with CEL `!has(event.payload) && event.attributes.payload_bytes == "921600"` -> MUST fire.
	//    (c) dispatch_agent trigger with CEL `!has(event.payload)` -> MUST fire and write truncated home file.
	createTrig := func(req CreateTriggerRequest) {
		r := doJSON(ownerCtx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/triggers", f.proj.ID), req)
		require.Equal(t, http.StatusCreated, r.Code, r.Body.String())
	}
	createTrig(CreateTriggerRequest{
		Name:   "inline-only-rule",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.big_build"},
			Rules: []TriggerMatchRuleSpec{{Dialect: store.TriggerDialectCEL, Expr: `has(event.payload)`}},
		},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "must-not-fire"},
	})
	createTrig(CreateTriggerRequest{
		Name:   "overflow-msg-rule",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.big_build"},
			Rules: []TriggerMatchRuleSpec{{Dialect: store.TriggerDialectCEL, Expr: `!has(event.payload) && event.attributes.payload_bytes == "921600" && event.attributes.payload_ref.startsWith("trigger-events/")`}},
		},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAg.ID, Message: "large build received"},
	})
	createTrig(CreateTriggerRequest{
		Name:   "overflow-dispatch-rule",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.big_build"},
			Rules: []TriggerMatchRuleSpec{{Dialect: store.TriggerDialectCEL, Expr: `!has(event.payload)`}},
		},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Inspect overflow event",
		},
	})

	// 3. Send a 900 KiB signed body to POST /api/v1/hooks/{id}.
	body900K := makeOverflowTestJSON(900 * 1024)
	postSigned := func(delivery, evName string, body []byte) *httptest.ResponseRecorder {
		now := time.Now().UTC()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderDelivery, delivery)
		req.Header.Set(trigger.HeaderEvent, evName)
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(ep.Secret, now.Unix(), delivery, evName, "", body))
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	rec900 := postSigned("deliv-900k", "big_build", body900K)
	require.Equal(t, http.StatusAccepted, rec900.Code, rec900.Body.String())
	var ingResp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec900.Body.Bytes(), &ingResp))
	evID := ingResp.EventID

	// Verify DB row and object storage.
	evRow, err := f.store.GetTriggerEvent(ctx, evID)
	require.NoError(t, err)
	assert.Empty(t, evRow.Payload)
	wantRef := "trigger-events/" + f.proj.ID + "/" + evID + ".json"
	assert.Equal(t, wantRef, evRow.PayloadRef)
	assert.Equal(t, len(body900K), evRow.PayloadBytes)

	// 4. Stream via GET .../trigger-events/{id}?payload=1 (owner allowed, viewer forbidden).
	streamRec := doJSON(ownerCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s?payload=1", f.proj.ID, evID), nil)
	require.Equal(t, http.StatusOK, streamRec.Code)
	assert.Equal(t, "application/json", streamRec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", streamRec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-store", streamRec.Header().Get("Cache-Control"))
	assert.Equal(t, body900K, streamRec.Body.Bytes(), "streamed ?payload=1 bytes must match exact 900 KiB body")

	viewerStreamRec := doJSON(viewerCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s?payload=1", f.proj.ID, evID), nil)
	assert.Equal(t, http.StatusForbidden, viewerStreamRec.Code)

	// 5. Drain the event through Dispatcher (wired with Storage: f.srv.GetStorage).
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.store,
		Core:          serverTriggerCore{srv: f.srv},
		Storage:       f.srv.GetStorage,
		SubjectClaims: trigger.NewSubjectClaims(f.store, f.store, nil),
		Rules:         f.srv.TriggerRules(),
	})
	require.NoError(t, err)
	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	now := time.Now().UTC()
	rows, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "overflow-test", now, now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, r := range rows {
		it := durablequeue.Item{Seq: r.Seq, Kind: r.Kind, DedupKey: r.DedupKey, Payload: r.Payload, Attempts: r.Attempts}
		res := disp.HandleItem(ctx, it, nil)
		require.True(t, res.Done)
		_, err := dqStore.CompleteDurableItem(ctx, r.Seq, "overflow-test")
		require.NoError(t, err)
	}

	// Verify message envelope: only overflow-msg-rule fired (inline-only-rule did NOT), and external_payload is truncated.
	fakeBroker.mu.Lock()
	require.Len(t, fakeBroker.sentMessages, 1, "only !has(event.payload) message trigger should fire")
	var msgEvBody messaging.EventBody
	require.NoError(t, json.Unmarshal(fakeBroker.sentMessages[0].Event, &msgEvBody))
	require.NotNil(t, msgEvBody.External)
	assert.True(t, msgEvBody.External.Truncated)
	assert.Equal(t, len(body900K), msgEvBody.External.Bytes)
	assert.Equal(t, string(body900K[:messaging.MaxExternalPayloadInlineBytes]), msgEvBody.External.Text)
	assert.Empty(t, msgEvBody.External.JSON)

	// Verify dispatch_agent home file (~/.scion/trigger-event.json) has truncated external_payload.
	require.Len(t, fakeBroker.createdReqs, 1)
	require.Len(t, fakeBroker.createdReqs[0].HomeFiles, 1)
	var homeEvBody messaging.EventBody
	require.NoError(t, json.Unmarshal(fakeBroker.createdReqs[0].HomeFiles[0].Content, &homeEvBody))
	fakeBroker.mu.Unlock()
	require.NotNil(t, homeEvBody.External)
	assert.True(t, homeEvBody.External.Truncated)
	assert.Equal(t, len(body900K), homeEvBody.External.Bytes)
	assert.Equal(t, string(body900K[:messaging.MaxExternalPayloadInlineBytes]), homeEvBody.External.Text)
	assert.Empty(t, homeEvBody.External.JSON)

	// 6. 64 KiB body on the same endpoint is stored inline unchanged.
	body64K := makeOverflowTestJSON(64 * 1024)
	rec64 := postSigned("deliv-64k", "big_build", body64K)
	require.Equal(t, http.StatusAccepted, rec64.Code, rec64.Body.String())
	var ing64 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec64.Body.Bytes(), &ing64))
	ev64, err := f.store.GetTriggerEvent(ctx, ing64.EventID)
	require.NoError(t, err)
	assert.Equal(t, string(body64K), ev64.Payload)
	assert.Empty(t, ev64.PayloadRef)

	// 7. Metrics snapshot records accepted_overflow, accepted, and payloadBytes histogram.
	snap := f.srv.TriggerSnapshotMetrics().GetSnapshot()
	assert.Equal(t, int64(1), snap.Ingress[store.TriggerSourceKindWebhook][trigger.IngressResultAcceptedOverflow])
	assert.Equal(t, int64(1), snap.Ingress[store.TriggerSourceKindWebhook][trigger.IngressResultAccepted])
	assert.Equal(t, int64(2), snap.PayloadBytes[store.TriggerSourceKindWebhook].Count)
	assert.Equal(t, int64(len(body900K)+len(body64K)), snap.PayloadBytes[store.TriggerSourceKindWebhook].Sum)
	assert.Equal(t, int64(1), snap.PayloadBytes[store.TriggerSourceKindWebhook].Buckets["le_65536"])
	assert.Equal(t, int64(2), snap.PayloadBytes[store.TriggerSourceKindWebhook].Buckets["le_1048576"])

	// 8. Purge deletes the overflow object with its row; subsequent ?payload=1 returns 404.
	futureNow := time.Now().UTC().Add(8 * 24 * time.Hour)
	maintRes, err := trigger.RunMaintenanceWithStorage(ctx, f.store, localStor, futureNow, trigger.MaintenanceOptions{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, maintRes.EventsPurged, 1)
	assert.Equal(t, 1, maintRes.OverflowObjectsPurged)

	exists, err := localStor.Exists(ctx, wantRef)
	require.NoError(t, err)
	assert.False(t, exists, "purged overflow object must be deleted from storage")

	afterPurgeRec := doJSON(ownerCtx, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s?payload=1", f.proj.ID, evID), nil)
	assert.Equal(t, http.StatusNotFound, afterPurgeRec.Code)

	// 9. Per-project byte budget (256 MiB): seed 256 MiB of live overflow rows -> next overflow POST returns 429.
	for i := 0; i < 256; i++ {
		seedID := uuid.NewString()
		require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:           seedID,
			ProjectID:    f.proj.ID,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     ep.ID,
			Type:         "webhook.big_build",
			Subject:      "webhook:" + ep.ID,
			DedupKey:     "budget-seed-" + strconv.Itoa(i),
			OccurredAt:   time.Now().UTC(),
			ReceivedAt:   time.Now().UTC(),
			PayloadRef:   "trigger-events/" + f.proj.ID + "/" + seedID + ".json",
			PayloadBytes: 1024 * 1024,
			Status:       store.TriggerEventStatusProcessed,
		}))
	}
	// Fresh ingress instance queries DB and enforces 256 MiB cap -> 429 queue full.
	f.srv.webhookIngress = newWebhookIngress(f.srv, nil)
	rec429 := postSigned("deliv-over-budget", "big_build", body900K)
	assert.Equal(t, http.StatusTooManyRequests, rec429.Code, rec429.Body.String())
	assert.Equal(t, "30", rec429.Header().Get("Retry-After"))
	assert.Contains(t, rec429.Body.String(), "queue full")
}

// TestTriggerOverflow_TemplatingOptionalAndRequired_FireAndRenderParity verifies O2:
// for overflow events, ${event.payload.x?} renders empty and required ${event.payload.x}
// refuses with "payload stored out of line", identically at fire time and in POST /render.
func TestTriggerOverflow_TemplatingOptionalAndRequired_FireAndRenderParity(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "s2-p5-o2")
	enableTriggersExperimentForTest(t, f.srv, true)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "hub-o2-test",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	f.srv.SetStorage(localStor)

	fakeBroker := &overflowFakeBrokerClient{}
	f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, fakeBroker, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

	ownerIdent := NewAuthenticatedUser(f.creator.ID, f.creator.Email, f.creator.DisplayName, f.creator.Role, "web")
	ownerCtx := contextWithCredentialContext(contextWithIdentity(ctx, ownerIdent), credentialContextForIdentity(ownerIdent))

	doJSON := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(ownerCtx)
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	epRec := doJSON(http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", f.proj.ID),
		map[string]any{"name": "o2-ep", "maxBodyBytes": 1024 * 1024})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Trigger A: optional ${event.payload.repo?} -> must succeed with empty expansion.
	recOpt := doJSON(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/triggers", f.proj.ID), CreateTriggerRequest{
		Name:   "o2-optional-payload",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.o2"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionDispatchAgent,
			Template:      "default",
			Task:          "Repo=[${event.payload.repo?}] Event=${event.id}",
			UntrustedVars: []string{"event.payload.repo"},
		},
	})
	require.Equal(t, http.StatusCreated, recOpt.Code, recOpt.Body.String())
	var trOpt TriggerResponse
	require.NoError(t, json.Unmarshal(recOpt.Body.Bytes(), &trOpt))

	// Trigger B: required ${event.payload.repo} -> must refuse with "payload stored out of line".
	recReq := doJSON(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/triggers", f.proj.ID), CreateTriggerRequest{
		Name:   "o2-required-payload",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.o2"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionDispatchAgent,
			Template:      "default",
			Task:          "Repo=${event.payload.repo}",
			UntrustedVars: []string{"event.payload.repo"},
		},
	})
	require.Equal(t, http.StatusCreated, recReq.Code, recReq.Body.String())
	var trReq TriggerResponse
	require.NoError(t, json.Unmarshal(recReq.Body.Bytes(), &trReq))

	// Send a 900 KiB overflow event.
	body900K := makeOverflowTestJSON(900 * 1024)
	now := time.Now().UTC()
	reqHook := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(body900K))
	reqHook.Header.Set("Content-Type", "application/json")
	reqHook.Header.Set(trigger.HeaderDelivery, "o2-deliv-1")
	reqHook.Header.Set(trigger.HeaderEvent, "o2")
	reqHook.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(ep.Secret, now.Unix(), "o2-deliv-1", "o2", "", body900K))
	hookRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(hookRec, reqHook)
	require.Equal(t, http.StatusAccepted, hookRec.Code, hookRec.Body.String())
	var ingResp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(hookRec.Body.Bytes(), &ingResp))
	evID := ingResp.EventID

	// 1. Render preview parity:
	//    - Optional trigger renders cleanly with "Repo=[] Event=<evID>".
	rOpt := doJSON(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/triggers/%s/render?eventId=%s", f.proj.ID, trOpt.ID, evID), nil)
	require.Equal(t, http.StatusOK, rOpt.Code, rOpt.Body.String())
	var renderOpt TriggerRenderResponse
	require.NoError(t, json.Unmarshal(rOpt.Body.Bytes(), &renderOpt))
	assert.Empty(t, renderOpt.Errors)
	require.NotNil(t, renderOpt.Slots.Task)
	assert.Contains(t, renderOpt.Slots.Task.Rendered, "Repo=[] Event="+evID)

	//    - Required trigger reports "payload stored out of line".
	rReq := doJSON(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/triggers/%s/render?eventId=%s", f.proj.ID, trReq.ID, evID), nil)
	require.Equal(t, http.StatusOK, rReq.Code, rReq.Body.String())
	var renderReq TriggerRenderResponse
	require.NoError(t, json.Unmarshal(rReq.Body.Bytes(), &renderReq))
	require.NotEmpty(t, renderReq.Errors)
	assert.Contains(t, renderReq.Errors[0].Message, "payload stored out of line")

	// 2. Drain / Fire parity:
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         f.store,
		Core:          serverTriggerCore{srv: f.srv},
		Storage:       f.srv.GetStorage,
		SubjectClaims: trigger.NewSubjectClaims(f.store, f.store, nil),
		Rules:         f.srv.TriggerRules(),
	})
	require.NoError(t, err)
	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	claimNow := time.Now().UTC().Add(time.Second)
	rows, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "o2-test", claimNow, claimNow.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	it := durablequeue.Item{Seq: rows[0].Seq, Kind: rows[0].Kind, DedupKey: rows[0].DedupKey, Payload: rows[0].Payload, Attempts: rows[0].Attempts}
	res := disp.HandleItem(ctx, it, nil)
	require.True(t, res.Done)

	// Optional trigger fired and expanded ${event.payload.repo?} to empty.
	optFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.proj.ID, TriggerID: trOpt.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, optFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, optFires.Items[0].Status)
	fakeBroker.mu.Lock()
	require.Len(t, fakeBroker.createdReqs, 1)
	assert.Contains(t, fakeBroker.createdReqs[0].Config.Task, "Repo=[] Event="+evID)
	fakeBroker.mu.Unlock()

	// Required trigger was refused with "payload stored out of line".
	reqFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.proj.ID, TriggerID: trReq.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, reqFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusRefused, reqFires.Items[0].Status)
	assert.Contains(t, reqFires.Items[0].Error, "payload stored out of line")
}

// TestTriggerOverflow_StreamPayload_RejectsCrossProjectRef pins O5:
// even if a trigger_events row in projA carries a tampered payload_ref pointing
// to an object belonging to another project, GET ?payload=1 must fail closed.
func TestTriggerOverflow_StreamPayload_RejectsCrossProjectRef(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "s2-p5-o5")
	enableTriggersExperimentForTest(t, f.srv, true)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "hub-o5-test",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	f.srv.SetStorage(localStor)

	otherProjectID := uuid.NewString()
	evID := uuid.NewString()
	crossRef, err := trigger.OverflowPayloadObjectPath(otherProjectID, evID)
	require.NoError(t, err)
	_, err = localStor.Upload(ctx, crossRef, strings.NewReader(`{"secret":"other-project"}`), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)

	// Plant a row in f.proj.ID with PayloadRef pointing to otherProjectID's object.
	require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:           evID,
		ProjectID:    f.proj.ID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     uuid.NewString(),
		Type:         "webhook.received",
		Subject:      "webhook:test",
		DedupKey:     "o5-cross-ref",
		OccurredAt:   time.Now().UTC(),
		ReceivedAt:   time.Now().UTC(),
		PayloadRef:   crossRef,
		PayloadBytes: 26,
		Status:       store.TriggerEventStatusProcessed,
	}))

	ownerIdent := NewAuthenticatedUser(f.creator.ID, f.creator.Email, f.creator.DisplayName, f.creator.Role, "web")
	ownerCtx := contextWithCredentialContext(contextWithIdentity(ctx, ownerIdent), credentialContextForIdentity(ownerIdent))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/trigger-events/%s?payload=1", f.proj.ID, evID), nil)
	req = req.WithContext(ownerCtx)
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "other-project", "cross-project object bytes must never be streamed")
}

// TestTriggerOverflow_GitHubComment_OutOfLineParity verifies O-D:
// github.comment on an overflow event expands optional ${event.payload.x?} to
// empty and refuses required ${event.payload.x} with FireStatusRefused and
// "payload stored out of line", matching dispatch_agent and POST /render.
func TestTriggerOverflow_GitHubComment_OutOfLineParity(t *testing.T) {
	f, gh, _, _, instID := setupGitHubCommentFixture(t)
	ctx := context.Background()

	// Trigger 1: required ${event.payload.repo} -> refused with "payload stored out of line".
	reqRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-comment-o2-required",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionGitHubComment,
			Body:          "Repo=${event.payload.repo}",
			Target:        "event",
			UntrustedVars: []string{"event.payload.repo"},
		},
	})
	require.Equal(t, http.StatusCreated, reqRec.Code, reqRec.Body.String())
	var trReqResp TriggerResponse
	require.NoError(t, json.Unmarshal(reqRec.Body.Bytes(), &trReqResp))
	trReqRow, err := f.store.GetTrigger(ctx, trReqResp.ID)
	require.NoError(t, err)

	// Trigger 2: optional ${event.payload.repo?} -> fires with empty expansion.
	optRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "gh-comment-o2-optional",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
		Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
		Action: TriggerActionSpec{
			Type:          store.TriggerActionGitHubComment,
			Body:          "Repo=[${event.payload.repo?}]",
			Target:        "event",
			UntrustedVars: []string{"event.payload.repo"},
		},
	})
	require.Equal(t, http.StatusCreated, optRec.Code, optRec.Body.String())
	var trOptResp TriggerResponse
	require.NoError(t, json.Unmarshal(optRec.Body.Bytes(), &trOptResp))
	trOptRow, err := f.store.GetTrigger(ctx, trOptResp.ID)
	require.NoError(t, err)

	evID := uuid.NewString()
	mkOverflowReq := func(trRow *store.Trigger, fireID string) hubfire.FireRequest {
		return hubfire.FireRequest{
			ProjectID: f.projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    trRow.InitiatorAttribution,
				Ceiling:        trRow.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   trRow.ID,
				FireID:         fireID,
			},
			ActionType: trRow.ActionType,
			Payload:    trRow.ActionPayload,
			Event: &hubfire.FireEvent{
				ID:           evID,
				ProjectID:    f.projectID,
				SourceKind:   store.TriggerSourceKindGitHub,
				SourceID:     strconv.FormatInt(instID, 10),
				Type:         "github.issue_comment.created",
				Subject:      "acme/widgets#42",
				OccurredAt:   time.Now().UTC(),
				ReceivedAt:   time.Now().UTC().Add(-time.Second),
				Payload:      nil,
				PayloadRef:   "trigger-events/" + f.projectID + "/" + evID + ".json",
				PayloadBytes: 900 * 1024,
				Attributes: map[string]string{
					"repo":       "acme/widgets",
					"repo_owner": "acme",
					"repo_name":  "widgets",
					"number":     "42",
					"sender":     "alice",
				},
				Trigger: &hubfire.FireTriggerRef{ID: trRow.ID, Name: trRow.Name},
			},
			Executor: hubfire.ExecutorContext{
				Kind: trigger.ExecutorKindTriggerDrainer,
				ID:   trigger.ExecutorIDPrefixTriggerEvent + evID,
			},
			FireAt:   time.Now().UTC(),
			Attempts: 1,
		}
	}

	outReq := f.srv.fireAction(ctx, mkOverflowReq(trReqRow, "fire-gh-o2-req"))
	assert.Equal(t, hubfire.FireStatusRefused, outReq.Status)
	assert.Equal(t, "payload stored out of line", outReq.Error)
	assert.Empty(t, gh.snapshotComments(), "refused github.comment must not post a comment")

	outOpt := f.srv.fireAction(ctx, mkOverflowReq(trOptRow, "fire-gh-o2-opt"))
	require.Equal(t, hubfire.FireStatusFired, outOpt.Status, "err=%s", outOpt.Error)
	posted := gh.snapshotComments()
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0].Body, "Repo=[]")
}
