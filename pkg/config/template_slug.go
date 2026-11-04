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

package config

import "github.com/GoogleCloudPlatform/scion/pkg/api"

// IsTemplateSlug reports whether v is exactly one local path component that
// can name a template (or a harness-config) inside a template directory: a
// template slug, a directory name or a template ID
// (api.IsSingleNameComponent: not empty, valid UTF-8 without NUL, local, no
// "/" or "\", not "." or "..", no leading ":" and no "://").
//
// It is the form a template value from a request (agent create or start,
// scheduled dispatch) must have: such a value never names a host directory or
// a remote source.
func IsTemplateSlug(v string) bool {
	return api.IsSingleNameComponent(v)
}
