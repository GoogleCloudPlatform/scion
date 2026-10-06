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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// projectSlugFormat describes the slug format a client-supplied project slug
// must already be in: the fixed points of api.Slugify.
var projectSlugFormat = fmt.Sprintf(
	"lowercase letters a-z, digits 0-9 and single hyphens, not starting or ending with a hyphen, at most %d characters",
	api.MaxSlugLength)

// projectSlugFormatMessage explains a refused client-supplied project slug.
// It names the field and the expected format, never the submitted value.
var projectSlugFormatMessage = "slug must be in slug format: " + projectSlugFormat

// isProjectSlugFormat reports whether a client-supplied project slug is
// non-empty and already equal to its api.Slugify form.
func isProjectSlugFormat(slug string) bool {
	return slug != "" && api.Slugify(slug) == slug
}

// requireProjectSlugFormat writes a 400 validation error and returns false
// when a client-supplied project slug is not in slug format. Slugs are
// accepted as submitted or refused; they are never rewritten.
func requireProjectSlugFormat(w http.ResponseWriter, slug string) bool {
	if isProjectSlugFormat(slug) {
		return true
	}
	ValidationError(w, projectSlugFormatMessage, map[string]interface{}{
		"field":  "slug",
		"format": projectSlugFormat,
	})
	return false
}
