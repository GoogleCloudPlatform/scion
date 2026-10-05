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

package artifacts

import "context"

// Principal kinds returned by Host.Principal.
const (
	PrincipalKindUser  = "user"
	PrincipalKindAgent = "agent"
)

// Scope kinds. In v1 every scope is a scion project, and its ref is the
// project's stable id.
const (
	ScopeKindProject = "project"
)

// Permission IDs passed to Host.Authorize. They match the hub permission
// registry rows of the same name.
const (
	PermissionRead   = "artifact.read"
	PermissionCreate = "artifact.create"
	PermissionUpdate = "artifact.update"
	PermissionDelete = "artifact.delete"
	PermissionManage = "artifact.manage"
)

// Host is the service's only view of the system it runs in. It answers two
// questions: who is calling, and whether that caller may act in a scope.
//
// Every input and output is a string or a bool, so the interface can later be
// served over a wire protocol by a standalone deployment. Do not add hub
// types, identity objects or callbacks to it.
type Host interface {
	// Principal identifies the caller of the request whose context is ctx.
	// kind is one of the PrincipalKind constants. ref is the principal's
	// stable id (a user id or an agent id), never a display name, email or
	// slug, because it is stored as the artifact owner. homeScope is the
	// scope ref an artifact the caller publishes is homed in by default, or
	// "" when the caller has none (a user). ok is false when the request is
	// unauthenticated or the caller is of a kind the service does not serve.
	Principal(ctx context.Context) (kind, ref, homeScope string, ok bool)

	// Authorize reports whether the caller may exercise permission (one of
	// the Permission constants) on artifacts in the scope scopeRef. It fails
	// closed: an unknown permission, an empty scope or an unauthenticated
	// caller yields false.
	Authorize(ctx context.Context, scopeRef, permission string) bool
}
