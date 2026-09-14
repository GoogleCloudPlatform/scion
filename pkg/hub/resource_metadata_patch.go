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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type resourceMetadataPatch struct {
	Name        string `json:"name,omitempty"`
	Slug        string `json:"slug,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

type resourceMetadataFields struct {
	Name        *string
	Slug        *string
	DisplayName *string
	Description *string
	Visibility  *string
}

func applyResourceMetadataPatch(w http.ResponseWriter, r *http.Request, fields resourceMetadataFields) bool {
	var updates resourceMetadataPatch
	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}

	if updates.Name != "" {
		*fields.Name = updates.Name
		if updates.Slug == "" {
			*fields.Slug = api.Slugify(updates.Name)
		}
	}
	if updates.Slug != "" {
		*fields.Slug = updates.Slug
	}
	if updates.DisplayName != "" {
		*fields.DisplayName = updates.DisplayName
	}
	if updates.Description != "" {
		*fields.Description = updates.Description
	}
	if updates.Visibility != "" {
		*fields.Visibility = updates.Visibility
	}
	return true
}
