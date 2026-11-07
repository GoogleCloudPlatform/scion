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

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// templateRefRefusal is the fixed error for a template value that is not a
// template name or ID. It never contains the value.
const templateRefRefusal = "template must be a template name or ID"

// projectDefaultTemplateRefusal is the fixed error when the template filled
// in from the project's default template setting is not a template name or
// ID. It names the setting so the operator can fix it, and never contains the
// value.
const projectDefaultTemplateRefusal = "the project's default template setting must be a template name or ID"

// templateRefAllowed reports whether a template value that reaches agent
// creation (an agent create request, a dispatch_agent event or schedule, or a
// default filled in for one) is accepted: empty, or a template slug or ID
// (config.IsTemplateSlug). An unmatched value can be forwarded to a Runtime
// Broker with local access, so a host directory or a remote source is never
// accepted.
func templateRefAllowed(v string) bool {
	return v == "" || config.IsTemplateSlug(v)
}
