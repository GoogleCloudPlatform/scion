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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const webhookEndpointChangedSubject = "trigger.webhook_endpoint.changed"

type webhookEndpointChangedEvent struct {
	EndpointID string `json:"endpointId"`
}

// invalidateWebhookEndpointCache evicts endpointID from this replica's ingress
// cache immediately and publishes a content-free invalidation event via s.events
// so peer replicas (via PostgresEventPublisher LISTEN/NOTIFY) evict their caches
// without waiting for EndpointCacheTTL (Stage 2 P4a O4).
func (s *Server) invalidateWebhookEndpointCache(endpointID string) {
	if s == nil || endpointID == "" {
		return
	}
	if s.webhookIngress != nil {
		s.webhookIngress.InvalidateEndpoint(endpointID)
	}
	if s.events != nil {
		s.events.PublishRaw(webhookEndpointChangedSubject, webhookEndpointChangedEvent{
			EndpointID: endpointID,
		})
	}
}

// startWebhookEndpointInvalidationSubscriber listens for cross-replica webhook
// endpoint config changes on s.events and evicts the local ingress cache.
// Exits cleanly when ctx is cancelled.
func (s *Server) startWebhookEndpointInvalidationSubscriber(ctx context.Context) {
	if s == nil || s.webhookIngress == nil || s.events == nil {
		return
	}
	ch, unsub := s.events.Subscribe(webhookEndpointChangedSubject)
	if ch == nil {
		unsub()
		return
	}
	go func() {
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				var payload webhookEndpointChangedEvent
				if err := json.Unmarshal(ev.Data, &payload); err == nil && payload.EndpointID != "" {
					s.webhookIngress.InvalidateEndpoint(payload.EndpointID)
				}
			}
		}
	}()
}

// newWebhookIngress constructs the POST /api/v1/hooks/{id} handler over srv's
// store, durable queue, trusted-proxy configuration, and secret cipher (§8.1).
// Returns nil when srv's store does not implement store.DurableQueueStore.
func newWebhookIngress(srv *Server, trustedProxies []string) *trigger.Ingress {
	if srv == nil || srv.store == nil || srv.durableQueue == nil {
		return nil
	}
	dqs, ok := srv.store.(store.DurableQueueStore)
	if !ok {
		return nil
	}
	trustedNets := parseTrustedProxies(trustedProxies)
	return trigger.NewIngress(trigger.IngressConfig{
		Store:                 srv.store,
		QueueStore:            dqs,
		Queue:                 srv.durableQueue,
		Storage:               srv.GetStorage,
		LoadSecret:            srv.loadWebhookEndpointSecret,
		VerifyGitHubSignature: githubapp.VerifyWebhookSignature,
		AuthorizeHubToken:     srv.authorizeWebhookIngressHubToken,
		ClientIP: func(r *http.Request) string {
			return trustedClientIP(r, trustedNets)
		},
		Enabled: func() bool {
			return srv.experimentEnabled(experiments.Triggers)
		},
		Audit: trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
			Logger: logging.Subsystem("hub.audit"),
		}),
		Metrics: serverTriggerMetrics{srv: srv},
		Logger:  logging.Subsystem("hub.triggers"),
	})
}

// authorizeWebhookIngressHubToken authorizes a hub_token webhook delivery on
// POST /api/v1/hooks/{id} (catalog operation webhookendpoint.send, §7.1).
func (s *Server) authorizeWebhookIngressHubToken(r *http.Request, ep *store.WebhookEndpoint) trigger.HubTokenAuthResult {
	if s == nil || r == nil || ep == nil {
		return trigger.HubTokenAuthResult{}
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return trigger.HubTokenAuthResult{Authenticated: false}
	}
	d := s.webhookEndpointAccessDecision(ctx, ep.ProjectID, ActionSend, ep)
	if !d.allowed {
		logAuthzDenial(r, identity, webhookEndpointResource(ep.ProjectID), ActionSend, d.reason)
		return trigger.HubTokenAuthResult{Authenticated: true, Allowed: false}
	}
	return trigger.HubTokenAuthResult{
		Authenticated: true,
		Allowed:       true,
		Sender:        principalKindForIdentity(identity) + ":" + identity.ID(),
	}
}

// handleWebhookIngress handles unauthenticated POST /api/v1/hooks/{id}
// requests (§8.1). Gated per request on experiments.Triggers (404 while off)
// rather than with requireExperiment at route registration so servers built
// over a synthetic test registry still boot. A Server built without New (or
// without a DurableQueueStore) also answers 404.
func (s *Server) handleWebhookIngress(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.Triggers) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	if s.webhookIngress == nil {
		http.NotFound(w, r)
		return
	}
	s.webhookIngress.ServeHTTP(w, r)
}
