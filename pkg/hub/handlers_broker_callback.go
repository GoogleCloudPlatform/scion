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
)

// brokerCallbackRequest is the JSON body POSTed by broker plugins to deliver
// callback data (e.g. interactive card responses, action acknowledgements)
// back to the hub.
type brokerCallbackRequest struct {
	Data map[string]interface{} `json:"data"`
}

// handleBrokerCallback handles POST /api/v1/broker/callback.
// This is the endpoint that broker plugins use to deliver callback data
// (e.g. interactive card responses, button clicks, action acknowledgements)
// from external systems back to the hub.
//
// Authentication: Requires broker HMAC authentication (X-Scion-Broker-ID header
// validated by BrokerAuthMiddleware).
func (s *Server) handleBrokerCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Require broker HMAC authentication.
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed,
			"broker HMAC authentication required", nil)
		return
	}

	pluginName := r.Header.Get("X-Scion-Plugin-Name")
	log := s.messageLog.With(
		"broker_id", broker.ID(),
		"plugin_name", pluginName,
	)

	// Parse request body.
	var req brokerCallbackRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Data == nil {
		ValidationError(w, "data is required", map[string]interface{}{
			"field": "data",
		})
		return
	}

	log.Info("Broker callback received",
		"data_size", len(req.Data),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"received": true,
	})
}
