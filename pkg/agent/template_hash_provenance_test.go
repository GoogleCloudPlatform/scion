// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const (
	hashTplImage  = "hash-template-image:1"
	localTplImage = "local-template-image:1"
)

// hydratedTemplateStartEnv is a project with a global "web-dev" template and
// a content-hash template cache dir, each bundling its own "claude-web"
// harness-config (told apart by user) and its own image. An agent is created
// from the hash dir with the "web-dev" slug.
type hydratedTemplateStartEnv struct {
	projectScionDir string
	agentName       string
	mgr             Manager
	captured        *runtime.RunConfig
}

func newHydratedTemplateStartEnv(t *testing.T) hydratedTemplateStartEnv {
	t.Helper()
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	writeHC := func(dir, user string) {
		_ = os.MkdirAll(filepath.Join(dir, "home"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "config.yaml"),
			[]byte("harness: claude\nuser: "+user+"\nimage: scion-claude:latest\n"), 0o644)
	}
	globalScionDir := filepath.Join(tmpDir, ".scion")
	writeHC(filepath.Join(globalScionDir, "harness-configs", "claude-web"), "globaluser")
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0o644)
	localTpl := filepath.Join(globalScionDir, "templates", "web-dev")
	writeHC(filepath.Join(localTpl, "harness-configs", "claude-web"), "localuser")
	_ = os.WriteFile(filepath.Join(localTpl, "scion-agent.json"),
		[]byte(`{"default_harness_config":"claude-web","image":"`+localTplImage+`"}`), 0o644)

	hashDir := filepath.Join(tmpDir, "template-cache", testContentHash)
	writeHC(filepath.Join(hashDir, "harness-configs", "claude-web"), "scion")
	_ = os.WriteFile(filepath.Join(hashDir, "scion-agent.json"),
		[]byte(`{"default_harness_config":"claude-web","image":"`+hashTplImage+`"}`), 0o644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0o755)

	captured := &runtime.RunConfig{}
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*captured = cfg
			return "mock-id", nil
		},
	})
	e := hydratedTemplateStartEnv{projectScionDir: projectScionDir, agentName: "hash-agent", mgr: mgr, captured: captured}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: e.agentName, Template: hashDir, TemplateName: "web-dev",
		ProjectPath: projectScionDir, NoAuth: true,
	}); err != nil {
		t.Fatalf("create Start: %v", err)
	}
	if captured.Image != hashTplImage {
		t.Fatalf("fixture: create image = %q, want %q", captured.Image, hashTplImage)
	}
	return e
}

func (e hydratedTemplateStartEnv) agentDir() string {
	return filepath.Join(e.projectScionDir, "agents", e.agentName)
}

// setInfoTemplateHash rewrites agent-info.json's templateHash, leaving the
// rest of the file as provisioning wrote it.
func (e hydratedTemplateStartEnv) setInfoTemplateHash(t *testing.T, hash string) {
	t.Helper()
	path := filepath.Join(config.GetAgentHomePath(e.projectScionDir, e.agentName), "agent-info.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		delete(m, "templateHash")
	} else {
		m["templateHash"] = hash
	}
	out, _ := json.Marshal(m)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e hydratedTemplateStartEnv) restart(t *testing.T, template string) runtime.RunConfig {
	t.Helper()
	*e.captured = runtime.RunConfig{}
	if _, err := e.mgr.Start(context.Background(), api.StartOptions{
		Name: e.agentName, Template: template, TemplateName: "web-dev",
		ProjectPath: e.projectScionDir, NoAuth: true,
	}); err != nil {
		t.Fatalf("restart Start (template %q): %v", template, err)
	}
	return *e.captured
}

// For an agent with image provenance, whether its template came from a
// content-addressed cache is read from the provenance record only: with
// agent-info.json's templateHash removed, a restart still treats the
// template as unresolvable — the same-named local template supplies
// neither the image nor the harness-config, and the recorded template
// image applies.
func TestStart_HydratedTemplateFromProvenanceIgnoresAgentInfoHash(t *testing.T) {
	e := newHydratedTemplateStartEnv(t)

	prov, err := readImageProvenance(e.agentDir())
	if err != nil || prov == nil {
		t.Fatalf("readImageProvenance: %v, %v", prov, err)
	}
	if prov.TemplateHash != testContentHash || prov.Template != "web-dev" {
		t.Fatalf("provenance template = %q, templateHash = %q; want %q, %q", prov.Template, prov.TemplateHash, "web-dev", testContentHash)
	}
	if prov.TemplateImage != hashTplImage {
		t.Fatalf("provenance templateImage = %q, want %q", prov.TemplateImage, hashTplImage)
	}

	e.setInfoTemplateHash(t, "")
	for _, template := range []string{"", "web-dev"} {
		got := e.restart(t, template)
		if got.Image != hashTplImage {
			t.Errorf("restart (template %q): image = %q, want the recorded template image %q", template, got.Image, hashTplImage)
		}
		if got.UnixUsername == "localuser" {
			t.Errorf("restart (template %q): used the local web-dev template's harness-config", template)
		}
	}
}

// A provenance record written before TemplateHash was recorded, whose
// Template is itself a content hash, is also treated as hydrated.
func TestTemplateHydrated(t *testing.T) {
	withInfoHash := &api.ScionConfig{Info: &api.AgentInfo{TemplateHash: testContentHash}}
	for _, tt := range []struct {
		name string
		prov *imageProvenance
		cfg  *api.ScionConfig
		want bool
	}{
		{name: "provenance hash", prov: &imageProvenance{Template: "web-dev", TemplateHash: testContentHash}, want: true},
		{name: "provenance content-hash template", prov: &imageProvenance{Template: testContentHash}, want: true},
		{name: "provenance named template ignores info hash", prov: &imageProvenance{Template: "web-dev"}, cfg: withInfoHash, want: false},
		{name: "legacy info hash", cfg: withInfoHash, want: true},
		{name: "legacy no hash", cfg: &api.ScionConfig{Info: &api.AgentInfo{Template: "web-dev"}}, want: false},
		{name: "legacy nil", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := templateHydrated(tt.prov, tt.cfg); got != tt.want {
				t.Errorf("templateHydrated = %v, want %v", got, tt.want)
			}
		})
	}
}

// An agent without image provenance (provisioned before it was recorded)
// keeps reading agent-info.json: its templateHash decides whether the
// template is looked up by name.
func TestStart_HydratedTemplateLegacyAgentUsesAgentInfo(t *testing.T) {
	e := newHydratedTemplateStartEnv(t)
	if err := os.Remove(filepath.Join(e.agentDir(), config.ImageProvenanceFileName)); err != nil {
		t.Fatal(err)
	}

	// templateHash set: the template is not looked up by name.
	got := e.restart(t, "")
	if got.UnixUsername == "localuser" || got.Image == localTplImage {
		t.Errorf("legacy agent with templateHash: used the local web-dev template (user %q, image %q)", got.UnixUsername, got.Image)
	}

	// templateHash removed: the recorded name is looked up, as before.
	e.setInfoTemplateHash(t, "")
	got = e.restart(t, "")
	if got.UnixUsername != "localuser" || got.Image != localTplImage {
		t.Errorf("legacy agent without templateHash: user %q, image %q; want the local web-dev template's %q, %q", got.UnixUsername, got.Image, "localuser", localTplImage)
	}
}
