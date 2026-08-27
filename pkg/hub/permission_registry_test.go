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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestPermissionRegistryEntriesDeclareCurrentUse(t *testing.T) {
	ids := map[string]bool{}
	for _, permission := range permissions.Registry {
		if permission.ID == "" {
			t.Fatal("registry permission with empty ID")
		}
		if ids[permission.ID] {
			t.Fatalf("duplicate permission ID %q", permission.ID)
		}
		ids[permission.ID] = true
		if permission.Resource == "" {
			t.Fatalf("%s has empty resource", permission.ID)
		}
		if permission.Action == "" {
			t.Fatalf("%s has empty action", permission.ID)
		}
		if len(permission.Enforcement) == 0 && len(permission.NonRouteUse) == 0 {
			t.Fatalf("%s must declare route enforcement or explicit non-route use", permission.ID)
		}
		for _, enforcement := range permission.Enforcement {
			assertEnforcementReferenceExists(t, permission.ID, enforcement)
		}
	}
}

func TestCapabilityActionMapsAreRegistryDerived(t *testing.T) {
	assertActionMapEqual(t, "resource actions", ResourceActions, permissions.ResourceActions())
	assertActionMapEqual(t, "scope actions", ScopeActions, permissions.ScopeActions())

	if !slices.Contains(ResourceActions["project"], ActionUpdate) {
		t.Fatal("project:update must appear in resource capabilities")
	}
	if !slices.Contains(ResourceActions["agent"], ActionPortAccess) {
		t.Fatal("agent:port_access must appear in resource capabilities")
	}
	for _, stale := range []Action{ActionStart, ActionStop, ActionMessage} {
		if slices.Contains(ResourceActions["agent"], stale) {
			t.Fatalf("agent:%s is not independently enforced and must not appear in capabilities", stale)
		}
	}
}

func TestUATScopesAreRegistryDerived(t *testing.T) {
	wantValid := permissions.UATValidScopes()
	assertStringBoolMapEqual(t, "UATValidScopes", store.UATValidScopes, wantValid)

	wantManage := permissions.UATManageScopes()
	gotManage := append([]string(nil), store.UATManageScopes...)
	sort.Strings(gotManage)
	if strings.Join(gotManage, "\n") != strings.Join(wantManage, "\n") {
		t.Fatalf("UATManageScopes drifted from registry\ngot:  %v\nwant: %v", gotManage, wantManage)
	}

	for _, stale := range []string{
		store.UATScopeAgentStart,
		store.UATScopeAgentStop,
		store.UATScopeAgentMessage,
		store.UATScopeAgentDispatch,
	} {
		if store.UATValidScopes[stale] {
			t.Fatalf("stale UAT scope %q must not be valid for new tokens", stale)
		}
		if slices.Contains(store.UATManageScopes, stale) {
			t.Fatalf("stale UAT scope %q must not be expanded by agent:manage", stale)
		}
	}
	for _, required := range []string{store.UATScopeProjectUpdate, store.UATScopeAgentPortAccess} {
		if !store.UATValidScopes[required] {
			t.Fatalf("valid UAT scope %q missing from registry-derived validation", required)
		}
	}
}

func TestAgentTokenScopesMapToRegistry(t *testing.T) {
	want := map[AgentTokenScope][]string{
		ScopeAgentStatusUpdate: {"agent.status_update"},
		ScopeAgentLogAppend:    {"agent.log_append"},
		ScopeProjectSecretRead: {"project.secret_read"},
		ScopeAgentCreate:       {"agent.create"},
		ScopeAgentLifecycle:    {"agent.attach", "agent.delete"},
		ScopeAgentNotify:       {"agent.notify"},
		ScopeAgentTokenRefresh: {"agent.token_refresh"},
		ScopeAgentPortForward:  {"agent.port_forward"},
		ScopeIdentityToken:     {"agent.identity_token"},
		ScopeProjectRead:       {"project.read"},
	}
	for scope, wantIDs := range want {
		gotIDs := registryPermissionIDsForAgentScope(string(scope))
		if strings.Join(gotIDs, "\n") != strings.Join(wantIDs, "\n") {
			t.Fatalf("agent token scope %q maps to wrong registry permissions\ngot:  %v\nwant: %v", scope, gotIDs, wantIDs)
		}
	}
}

func TestTokenScopeSurfacesDoNotExposeStaleUATScopes(t *testing.T) {
	cliHelp := permissions.UATScopeHelp()
	for _, stale := range []string{"agent:start", "agent:stop", "agent:message", "agent:dispatch"} {
		if strings.Contains(cliHelp, stale) {
			t.Fatalf("generated CLI help still exposes stale UAT scope %q", stale)
		}
	}
	for _, required := range []string{"project:update", "agent:port_access"} {
		if !strings.Contains(cliHelp, required) {
			t.Fatalf("generated CLI help does not expose valid UAT scope %q", required)
		}
	}

	for _, path := range []string{"../../web/src/components/shared/token-list.ts"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := extractWebTokenScopes(t, path, string(content))
		want := registryUATScopes(true)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s AVAILABLE_SCOPES drifted from registry\ngot:  %v\nwant: %v", path, got, want)
		}
	}
}

func assertActionMapEqual(t *testing.T, name string, got map[string][]Action, want map[string][]string) {
	t.Helper()
	gotString := map[string][]string{}
	for resource, actions := range got {
		for _, action := range actions {
			gotString[resource] = append(gotString[resource], string(action))
		}
	}
	for resource := range gotString {
		sort.Strings(gotString[resource])
	}
	for resource := range want {
		sort.Strings(want[resource])
	}
	if !stringSliceMapEqual(gotString, want) {
		t.Fatalf("%s drifted from registry\ngot:  %v\nwant: %v", name, gotString, want)
	}
}

func assertStringBoolMapEqual(t *testing.T, name string, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length drifted from registry\ngot:  %v\nwant: %v", name, got, want)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s[%q] = %v, want %v", name, key, got[key], wantValue)
		}
	}
}

func stringSliceMapEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, aValues := range a {
		bValues, ok := b[key]
		if !ok || strings.Join(aValues, "\n") != strings.Join(bValues, "\n") {
			return false
		}
	}
	return true
}

func registryPermissionIDsForAgentScope(scope string) []string {
	var ids []string
	for _, permission := range permissions.Registry {
		if slices.Contains(permission.AgentScopes, scope) {
			ids = append(ids, permission.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func assertEnforcementReferenceExists(t *testing.T, permissionID, enforcement string) {
	t.Helper()

	fileRef, symbolRef, _ := strings.Cut(enforcement, ":")
	if fileRef == "" {
		t.Fatalf("%s has empty enforcement file reference %q", permissionID, enforcement)
	}
	path := filepath.Clean(filepath.Join("..", "..", fileRef))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s enforcement reference %q points to missing file: %v", permissionID, enforcement, err)
	}
	if symbolRef != "" && !strings.Contains(string(content), symbolRef) {
		t.Fatalf("%s enforcement reference %q points to missing symbol %q", permissionID, enforcement, symbolRef)
	}
}

func extractWebTokenScopes(t *testing.T, path, content string) []string {
	t.Helper()

	start := strings.Index(content, "const AVAILABLE_SCOPES = [")
	if start < 0 {
		t.Fatalf("%s missing AVAILABLE_SCOPES declaration", path)
	}
	end := strings.Index(content[start:], "] as const;")
	if end < 0 {
		t.Fatalf("%s missing AVAILABLE_SCOPES terminator", path)
	}
	block := content[start : start+end]

	matches := regexp.MustCompile(`value:\s*'([^']+)'`).FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		t.Fatalf("%s AVAILABLE_SCOPES has no scope values", path)
	}
	scopes := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		scope := match[1]
		if seen[scope] {
			t.Fatalf("%s AVAILABLE_SCOPES contains duplicate scope %q", path, scope)
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

func registryUATScopes(includeAliases bool) []string {
	options := permissions.UATScopeOptions(includeAliases)
	scopes := make([]string, 0, len(options))
	for _, option := range options {
		scopes = append(scopes, option.UATScope)
	}
	sort.Strings(scopes)
	return scopes
}
