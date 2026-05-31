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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

type systemIdentityRequest struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type systemIdentityResponse struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

func (s *Server) handleSystemIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w)
		return
	}

	if err := assertLoopback(r); err != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
		return
	}

	var req systemIdentityRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.DisplayName != "" {
		if err := config.UpdateSetting("", "server.auth.display_name", req.DisplayName, true); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save display name", nil)
			return
		}
	}

	if req.Email != "" {
		if err := config.UpdateSetting("", "server.auth.email", req.Email, true); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save email", nil)
			return
		}
	}

	writeJSON(w, http.StatusOK, systemIdentityResponse(req))
}
