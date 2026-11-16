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

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

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
		Store:      srv.store,
		QueueStore: dqs,
		Queue:      srv.durableQueue,
		LoadSecret: srv.loadWebhookEndpointSecret,
		ClientIP: func(r *http.Request) string {
			return geExchangeClientIP(r, trustedNets)
		},
		Enabled: func() bool {
			return srv.experimentEnabled(experiments.Triggers)
		},
		Audit: trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
			Logger: logging.Subsystem("hub.audit"),
		}),
		Logger: logging.Subsystem("hub.triggers"),
	})
}

// handleWebhookIngress handles unauthenticated POST /api/v1/hooks/{id}
// requests (§8.1). A Server built without New (or without a DurableQueueStore)
// answers 404.
func (s *Server) handleWebhookIngress(w http.ResponseWriter, r *http.Request) {
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
