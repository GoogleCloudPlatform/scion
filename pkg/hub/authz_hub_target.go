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

// Hub-level target adapters.
//
// A hub-level target resolves to the hub scope only through the adapters in
// this file, chosen by trusted server code for each operation. A request
// without this evidence resolves to an unknown target scope, and the bearer
// gate denies it. No call site falls back to treating a parentless resource
// as hub-level.

// hubScopedResource returns the canonical target for an instance-level hub
// resource: role definitions, role bindings, access constraints, quota and
// entitlement records, user records, groups without a project parent,
// hub-scoped GCP service accounts, skill registries, the hub-wide agent
// stop-all target and hub-scoped secrets. ResolveTargetScope classifies it
// as the hub scope through its explicit system parent.
//
// The authorization kernel treats a system parent the same as no parent
// (projectIDForResource and resourceProjectScope return no project for
// either), and isHubScopedResource accepts both shapes, so session
// decisions do not depend on which shape a caller builds.
func hubScopedResource(resourceType, id string) Resource {
	return Resource{Type: resourceType, ID: id, ParentType: "system"}
}

// isHubScopedResource reports whether a resource is hub-scoped by its own
// record: it has no project parent, either because it carries no parent or
// because it carries an explicit system parent with no parent ID. It is the
// one predicate for this shape; relationship rules that apply only to
// hub-scoped records use it so both shapes give the same decision.
func isHubScopedResource(r Resource) bool {
	if r.ParentID != "" {
		return false
	}
	return r.ParentType == "" || r.ParentType == "system"
}

// hubCollectionEvidence declares a collection-level request at the hub
// scope for permissionID, such as a hub-wide list or a hub-level create.
// It must be built by server code that knows which operation it runs,
// never from a request field. ResolveTargetScope accepts it only when the
// target names no instance and permissionID's collection classes
// (permissions.CollectionTargetClasses) include a hub-reachable class, and
// the bearer gate accepts it only for the exact permission evaluated.
func hubCollectionEvidence(permissionID string) TargetScopeEvidence {
	return TargetScopeEvidence{IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: permissionID}
}

// projectCollectionEvidence declares a collection-level request inside an
// existing project for permissionID, such as creating or listing resources
// in that project. The same rules as hubCollectionEvidence apply, with the
// project-scoped class required instead.
func projectCollectionEvidence(permissionID, projectID string) TargetScopeEvidence {
	return TargetScopeEvidence{IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: projectID, PermissionID: permissionID}
}

// Route bearer target values (RouteMetadata.BearerTarget).
const (
	// routeBearerTargetNone keeps the route guard target {Type, ID: "hub"}.
	routeBearerTargetNone = ""
	// routeBearerTargetHubInstance makes the route guard check the
	// instance-level hub target hubScopedResource(Resource, "hub").
	routeBearerTargetHubInstance = "hub_instance"
	// routeBearerTargetHubCollection makes the route guard check a
	// collection-level request at the hub scope, with
	// hubCollectionEvidence(Permission).
	routeBearerTargetHubCollection = "hub_collection"
)

// routeGuardTarget returns the target and evidence the hub-admin route
// guard checks for a route that declares a Permission. A route opts in to a
// hub-level target through BearerTarget; without it the guard checks
// {Type: Resource, ID: "hub"}, which resolves to the hub scope only for the
// "hub" resource type. An unknown BearerTarget value returns ok=false, and
// the guard refuses the request as misconfigured.
func routeGuardTarget(meta RouteMetadata) (Resource, TargetScopeEvidence, bool) {
	switch meta.BearerTarget {
	case routeBearerTargetNone:
		return Resource{Type: meta.Resource, ID: "hub"}, TargetScopeEvidence{}, true
	case routeBearerTargetHubInstance:
		return hubScopedResource(meta.Resource, "hub"), TargetScopeEvidence{}, true
	case routeBearerTargetHubCollection:
		return Resource{Type: meta.Resource}, hubCollectionEvidence(meta.Permission), true
	default:
		return Resource{}, TargetScopeEvidence{}, false
	}
}
