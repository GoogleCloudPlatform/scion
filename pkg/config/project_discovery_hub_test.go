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

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverProjects_HubLink checks the hub project ID and endpoint that
// DiscoverProjects reports for linked projects: the project's own
// hub.project_id with its resolved endpoint, nothing for a project that is
// explicitly unlinked or has no link, and never the global project's link
// inherited through the merged settings. The fields stay out of the JSON.
func TestDiscoverProjects_HubLink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID"} {
		t.Setenv(name, "")
	}
	global := filepath.Join(home, GlobalDir)
	configs := filepath.Join(global, ProjectConfigsDir)
	write := func(dir, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(global, "schema_version: \"1\"\nhub:\n  endpoint: http://127.0.0.1:8080/\n  project_id: global-hub-id\n")
	write(filepath.Join(configs, "linked__aaaa", DotScion),
		"schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "src", "linked")+"\nhub:\n  endpoint: HTTPS://Hub.Example.com/\n  project_id: linked-hub-id\n")
	write(filepath.Join(configs, "inherits__bbbb", DotScion),
		"schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "src", "inherits")+"\nhub:\n  project_id: inherits-hub-id\n")
	write(filepath.Join(configs, "unlinked__cccc", DotScion),
		"schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "src", "unlinked")+"\nhub:\n  project_id: old-hub-id\n  linked: false\n")
	write(filepath.Join(configs, "plain__dddd", DotScion),
		"schema_version: \"1\"\nworkspace_path: "+filepath.Join(home, "src", "plain")+"\n")

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatal(err)
	}
	type link struct{ id, endpoint string }
	got := map[string]link{}
	for _, p := range projects {
		got[p.Name] = link{p.HubProjectID, p.HubEndpoint}
	}
	want := map[string]link{
		"global":   {"global-hub-id", "http://127.0.0.1:8080"},
		"linked":   {"linked-hub-id", "https://hub.example.com"},
		"inherits": {"inherits-hub-id", "http://127.0.0.1:8080"},
		"unlinked": {},
		"plain":    {},
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s: hub link = %+v (found %v), want %+v", name, g, ok, w)
		}
	}

	data, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"HubProjectID", "HubEndpoint", "hub.example.com", "127.0.0.1"} {
		if strings.Contains(string(data), s) {
			t.Errorf("JSON contains %q: %s", s, data)
		}
	}
}
