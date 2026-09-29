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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// Permission resolution for requests that name a resource type and action but
// no explicit permission (ptone/scion#2119).
//
// resolveResourcePermission is table driven and fails closed: a pair maps to a
// permission ID only when exactly one registry entry has that (Resource,
// Action), or when the pair is listed in unregisteredResourcePermissions.
// Every other pair returns an error and Decide denies with reason
// "unresolvable permission". Callers whose pair is ambiguous (for example
// hub + read) must pass AuthzRequest.Permission.

// errUnresolvablePermission is returned for a (resource type, action) pair
// that does not name exactly one permission.
var errUnresolvablePermission = errors.New("unresolvable permission")

// unresolvablePermissionReason is the Decide deny reason for a request whose
// permission cannot be resolved.
const unresolvablePermissionReason = "unresolvable permission"

type resourceActionKey struct {
	ResourceType string
	Action       Action
}

// unregisteredResourcePermissions lists (resource type, action) pairs that
// production call sites evaluate without an explicit permission and that have
// no registry entry. Each maps to the permission ID role definitions and
// relationship rows use for it. Adding an entry requires naming its call
// sites; a newly registered permission should be used instead.
var unregisteredResourcePermissions = map[resourceActionKey]string{
	// handlers_messages.go (message list, message stream) and
	// handlers_logs.go (agent log read): full-visibility check.
	// TODO(ptone/scion#2120): drop this entry once agent.manage is registered.
	{ResourceType: "agent", Action: ActionManage}: "agent.manage",
	// handlers_env_secrets.go and handlers_runtime_brokers.go: runtime
	// broker scoped env/secret and broker record checks.
	{ResourceType: "runtime_broker", Action: ActionRead}:   "runtime_broker.read",
	{ResourceType: "runtime_broker", Action: ActionUpdate}: "runtime_broker.update",
}

type resourcePermissionEntry struct {
	id        string
	ambiguous []string
}

var resourcePermissionTable = buildResourcePermissionTable(permissions.Registry, unregisteredResourcePermissions)

func buildResourcePermissionTable(registry []permissions.Permission, unregistered map[resourceActionKey]string) map[resourceActionKey]resourcePermissionEntry {
	ids := map[resourceActionKey][]string{}
	for _, p := range registry {
		k := resourceActionKey{ResourceType: p.Resource, Action: Action(p.Action)}
		ids[k] = append(ids[k], p.ID)
	}
	table := make(map[resourceActionKey]resourcePermissionEntry, len(ids)+len(unregistered))
	for k, list := range ids {
		if len(list) == 1 {
			table[k] = resourcePermissionEntry{id: list[0]}
			continue
		}
		sorted := append([]string(nil), list...)
		sort.Strings(sorted)
		table[k] = resourcePermissionEntry{ambiguous: sorted}
	}
	for k, id := range unregistered {
		if _, exists := table[k]; exists {
			// A registered pair always takes the registry answer; the
			// consistency test rejects such an entry.
			continue
		}
		table[k] = resourcePermissionEntry{id: id}
	}
	return table
}

// resolveResourcePermission returns the permission ID for (resourceType,
// action), or an error wrapping errUnresolvablePermission when the pair is
// unknown or names more than one permission.
func resolveResourcePermission(resourceType string, action Action) (string, error) {
	entry, ok := resourcePermissionTable[resourceActionKey{ResourceType: resourceType, Action: action}]
	if !ok {
		return "", fmt.Errorf("%w: no permission registered for %s/%s", errUnresolvablePermission, resourceType, action)
	}
	if len(entry.ambiguous) > 0 {
		return "", fmt.Errorf("%w: %s/%s names %d permissions (%s); pass an explicit permission",
			errUnresolvablePermission, resourceType, action, len(entry.ambiguous), strings.Join(entry.ambiguous, ", "))
	}
	return entry.id, nil
}
