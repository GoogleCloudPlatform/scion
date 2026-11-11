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
}

type resourceMetadataFields struct {
	Name        *string
	Slug        *string
	DisplayName *string
	Description *string
}

// resourceNameNotComponentMessage is the fixed refusal text for a template
// or harness-config name that is not exactly one path component. It never
// contains the name.
const resourceNameNotComponentMessage = "name must be a single path component"

// refuseNonComponentResourceName writes a validation error and returns true
// when name, a template or harness-config Name about to be stored, is not
// exactly one path component (api.IsSingleNameComponent): such a name is
// later joined into directory paths by the CLI and the brokers. The store
// refuses it too; this answers before any other work, with a fixed text.
func refuseNonComponentResourceName(w http.ResponseWriter, name string) bool {
	if api.IsSingleNameComponent(name) {
		return false
	}
	ValidationError(w, resourceNameNotComponentMessage, nil)
	return true
}

// applyResourceMetadataPatch applies a template or harness-config PATCH
// body to fields. A new name that is not one path component is refused
// (refuseNonComponentResourceName) and nothing is applied.
func applyResourceMetadataPatch(w http.ResponseWriter, r *http.Request, fields resourceMetadataFields) bool {
	var updates resourceMetadataPatch
	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}
	if updates.Name != "" && refuseNonComponentResourceName(w, updates.Name) {
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
	return true
}
