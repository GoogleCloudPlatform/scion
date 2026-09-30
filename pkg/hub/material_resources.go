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

// Package hub — typed resource construction for material permission
// decisions (ptone/scion#2129): the scope-to-resource-type mapping a
// material Decide or admission call needs. Candidate/grant construction
// itself is built on later slices; this file carries only the reviewed
// mapping and its consistency test.
package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scopeUnknownResourceType is scopeToResourceType's result for a scope
// string outside the four reviewed values. It is never "hub": an
// unrecognized scope must not be mistaken for the genuinely hub-scoped
// carve-out, and it is never a registered resource type, so it cannot
// resolve to a real parent by accident.
const scopeUnknownResourceType = "unknown"

// scopeToResourceType maps a material's stored scope string to the Resource
// ParentType a material Decide call uses. The registry's own resource type
// for a broker parent is "broker" (permissions/registry.go), not the
// "runtime_broker" scope string, so that one case is a rename rather than a
// pass-through. Project, hub and user map to themselves. Anything else maps
// to scopeUnknownResourceType, which denies rather than silently resolving
// to one of the four reviewed classes.
func scopeToResourceType(scope string) string {
	switch scope {
	case store.ScopeRuntimeBroker:
		return permissions.ResourceBroker
	case store.ScopeProject:
		return permissions.ResourceProject
	case store.ScopeHub:
		return permissions.ResourceHub
	case store.ScopeUser:
		return permissions.ResourceUser
	default:
		return scopeUnknownResourceType
	}
}
