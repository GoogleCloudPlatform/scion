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

package runtimebroker

import (
	"fmt"
	"net/http"
)

// ImageStatusResponse is the response for GET /api/v1/images/status.
type ImageStatusResponse struct {
	LocalShort *ImageEntityState `json:"local_short,omitempty"`
	LocalLong  *ImageEntityState `json:"local_long,omitempty"`
}

// ImageEntityState represents the local state of a single image reference.
type ImageEntityState struct {
	Exists bool   `json:"exists"`
	Hash   string `json:"hash,omitempty"`
}

// ImagePullRequest is the request body for POST /api/v1/images/pull.
type ImagePullRequest struct {
	Image string `json:"image"`
}

// ImageDeleteRequest is the request body for DELETE /api/v1/images/local.
type ImageDeleteRequest struct {
	Image string `json:"image"`
}

func (s *Server) handleImageStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	query := r.URL.Query()
	shortImage := query.Get("short")
	longImage := query.Get("long")

	if shortImage == "" && longImage == "" {
		BadRequest(w, "at least one of 'short' or 'long' query parameters is required")
		return
	}

	resp := ImageStatusResponse{}

	if shortImage != "" {
		state := &ImageEntityState{}
		exists, err := s.runtime.ImageExists(ctx, shortImage)
		if err != nil {
			RuntimeError(w, fmt.Sprintf("failed to check image %q: %v", shortImage, err))
			return
		}
		state.Exists = exists
		if exists {
			if hash, err := s.runtime.ImageID(ctx, shortImage); err == nil {
				state.Hash = hash
			}
		}
		resp.LocalShort = state
	}

	if longImage != "" {
		state := &ImageEntityState{}
		exists, err := s.runtime.ImageExists(ctx, longImage)
		if err != nil {
			RuntimeError(w, fmt.Sprintf("failed to check image %q: %v", longImage, err))
			return
		}
		state.Exists = exists
		if exists {
			if hash, err := s.runtime.ImageID(ctx, longImage); err == nil {
				state.Hash = hash
			}
		}
		resp.LocalLong = state
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	var req ImagePullRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if req.Image == "" {
		ValidationError(w, "image is required", nil)
		return
	}

	ctx := r.Context()
	if err := s.runtime.PullImage(ctx, req.Image); err != nil {
		RuntimeError(w, fmt.Sprintf("failed to pull image %q: %v", req.Image, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "pulled", "image": req.Image})
}

func (s *Server) handleImageDeleteLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w)
		return
	}

	var req ImageDeleteRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if req.Image == "" {
		ValidationError(w, "image is required", nil)
		return
	}

	ctx := r.Context()

	exists, err := s.runtime.ImageExists(ctx, req.Image)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("failed to check image %q: %v", req.Image, err))
		return
	}
	if !exists {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_found", "image": req.Image})
		return
	}

	if err := s.runtime.RemoveImage(ctx, req.Image); err != nil {
		RuntimeError(w, fmt.Sprintf("failed to remove image %q: %v", req.Image, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "image": req.Image})
}
