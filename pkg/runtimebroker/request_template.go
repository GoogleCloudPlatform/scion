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

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// requestTemplateRefusal is the fixed error for a request template value that
// is not accepted. It never contains the value.
const requestTemplateRefusal = "template must be a template name or ID"

// requestTemplateAllowed reports whether a template value from a request
// (create's config.template, start's and restart's templateName) is accepted:
// empty, or a template slug or ID (config.IsTemplateSlug). A host directory or
// a remote source is never accepted from a request.
func requestTemplateAllowed(v string) bool {
	return v == "" || config.IsTemplateSlug(v)
}
