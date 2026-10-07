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

// Host is the service's only view of the system it runs in. It answers
// three questions: who is calling, whether that caller may act in a scope,
// and whether the caller's credential allows an action at all.
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
	// caller yields false. Authorize true implies Permits true.
	Authorize(ctx context.Context, scopeRef, permission string) bool

	// Permits reports whether the caller's credential itself allows
	// permission on artifacts homed in scopeRef: the restrictions a token
	// carries (its scopes, ceiling and boundary), checked before ownership,
	// grants or role bindings. The service asks it first on every path, so
	// neither ownership nor a grant reaches past what the credential allows.
	// It fails closed like Authorize.
	Permits(ctx context.Context, scopeRef, permission string) bool
}

// ScopeExplainer is an optional extension of Host. When a credential
// lacks a scope that publishing needs, the service asks it which one, so
// the caller gets a 403 naming the scope instead of an answer that looks
// like an expired credential.
//
// The service consults it only on the publish path, which names no
// existing artifact. Reads keep their uniform 404 and never call it.
type ScopeExplainer interface {
	// MissingScope reports the name of the credential scope the caller of
	// ctx lacks for permission (one of the Permission constants), or ""
	// when the caller is unauthenticated, holds every scope the permission
	// needs, or is refused for another reason. It is a pure description:
	// a non-empty answer never grants anything.
	MissingScope(ctx context.Context, permission string) string
}
