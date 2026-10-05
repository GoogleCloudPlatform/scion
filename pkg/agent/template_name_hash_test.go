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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// testContentHash is a content hash in the form the template cache uses as a
// directory name.
var testContentHash = "sha256:" + strings.Repeat("ab", 32)

// hashTemplateEnv is a project with an existing agent and a global template
// named "web-dev" whose image identifies it when merged into the agent's
// config.
type hashTemplateEnv struct {
	scionDir  string
	agentName string
	agentHome string
}

const localWebDevImage = "local-web-dev:1"

func newHashTemplateEnv(t *testing.T) hashTemplateEnv {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	webDev := filepath.Join(globalScionDir, "templates", "web-dev")
	if err := os.MkdirAll(webDev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDev, "scion-agent.json"),
		[]byte(`{"image":"`+localWebDevImage+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	scionDir := filepath.Join(tmpDir, "project", ".scion")
	agentName := "hash-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"claude","harness_config":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return hashTemplateEnv{scionDir: scionDir, agentName: agentName, agentHome: agentHome}
}

func (e hashTemplateEnv) writeInfo(t *testing.T, info string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.agentHome, "agent-info.json"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGetAgent_HashTemplateInAgentInfo covers an existing agent whose
// agent-info.json records a template loaded from a content-addressed cache,
// with and without a template slug from the dispatch. The template is never
// looked up by the slug (the local "web-dev" template is not merged), and the
// returned info never names the template by its hash.
func TestGetAgent_HashTemplateInAgentInfo(t *testing.T) {
	tests := []struct {
		name         string
		info         string
		slug         string
		wantTemplate string
	}{
		{
			name:         "legacy hash name, no slug",
			info:         `{"name":"hash-agent","template":"` + testContentHash + `"}`,
			wantTemplate: "",
		},
		{
			name:         "legacy hash name, slug",
			info:         `{"name":"hash-agent","template":"` + testContentHash + `"}`,
			slug:         "web-dev",
			wantTemplate: "web-dev",
		},
		{
			name:         "slug with template hash, no slug",
			info:         `{"name":"hash-agent","template":"web-dev","templateHash":"` + testContentHash + `"}`,
			wantTemplate: "web-dev",
		},
		{
			name:         "empty name with template hash, slug",
			info:         `{"name":"hash-agent","template":"","templateHash":"` + testContentHash + `"}`,
			slug:         "web-dev",
			wantTemplate: "web-dev",
		},
		{
			name:         "hash slug is ignored",
			info:         `{"name":"hash-agent","template":"` + testContentHash + `"}`,
			slug:         testContentHash,
			wantTemplate: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newHashTemplateEnv(t)
			env.writeInfo(t, tt.info)
			ctx := context.Background()
			if tt.slug != "" {
				ctx = api.ContextWithTemplateName(ctx, tt.slug)
			}
			_, _, _, cfg, err := GetAgent(ctx, env.agentName, "", "", "", env.scionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if cfg.Image == localWebDevImage {
				t.Fatalf("GetAgent merged the local %q template by name; the loaded template must not change", "web-dev")
			}
			if cfg.Info == nil {
				t.Fatal("cfg.Info = nil, want agent-info.json contents")
			}
			if cfg.Info.Template != tt.wantTemplate {
				t.Errorf("Info.Template = %q, want %q", cfg.Info.Template, tt.wantTemplate)
			}
			if cfg.Info.TemplateHash != testContentHash {
				t.Errorf("Info.TemplateHash = %q, want %q", cfg.Info.TemplateHash, testContentHash)
			}
		})
	}
}

// TestGetAgent_AgentInfoWithoutTemplateHashUnchanged checks that an
// agent-info.json written before templateHash existed, naming a template that
// is available locally, still loads that template by name.
func TestGetAgent_AgentInfoWithoutTemplateHashUnchanged(t *testing.T) {
	for _, slug := range []string{"", "other-slug"} {
		t.Run("slug="+slug, func(t *testing.T) {
			env := newHashTemplateEnv(t)
			env.writeInfo(t, `{"name":"hash-agent","template":"web-dev"}`)
			ctx := context.Background()
			if slug != "" {
				ctx = api.ContextWithTemplateName(ctx, slug)
			}
			_, _, _, cfg, err := GetAgent(ctx, env.agentName, "", "", "", env.scionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if cfg.Image != localWebDevImage {
				t.Errorf("cfg.Image = %q, want %q from the local template", cfg.Image, localWebDevImage)
			}
			if cfg.Info == nil || cfg.Info.Template != "web-dev" || cfg.Info.TemplateHash != "" {
				t.Errorf("Info = %+v, want template web-dev and no template hash", cfg.Info)
			}
		})
	}
}

// TestProvisionAgent_HashTemplateDirRecordsSlug provisions from a template
// in a content-hash cache directory and checks that agent-info.json records
// the slug (or nothing) as the template name, and the hash separately.
func TestProvisionAgent_HashTemplateDirRecordsSlug(t *testing.T) {
	for _, tt := range []struct {
		name, slug, want string
	}{
		{name: "slug", slug: "web-dev", want: "web-dev"},
		{name: "no slug", slug: "", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			t.Cleanup(func() { _ = os.Chdir(oldWd) })
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0o755)
			seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

			cacheDir := filepath.Join(tmpDir, "cache", testContentHash)
			_ = os.MkdirAll(cacheDir, 0o755)
			_ = os.WriteFile(filepath.Join(cacheDir, "scion-agent.json"),
				[]byte(`{"default_harness_config":"claude"}`), 0o644)

			scionDir := filepath.Join(tmpDir, "project", ".scion")
			_ = os.MkdirAll(scionDir, 0o755)

			ctx := context.Background()
			if tt.slug != "" {
				ctx = api.ContextWithTemplateName(ctx, tt.slug)
			}
			agentHome, _, _, err := ProvisionAgent(ctx, "hydrated-agent", cacheDir, "", "", scionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("ProvisionAgent: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
			if err != nil {
				t.Fatal(err)
			}
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err != nil {
				t.Fatal(err)
			}
			if info.Template != tt.want {
				t.Errorf("agent-info.json template = %q, want %q", info.Template, tt.want)
			}
			if info.TemplateHash != testContentHash {
				t.Errorf("agent-info.json templateHash = %q, want %q", info.TemplateHash, testContentHash)
			}
		})
	}
}

func TestInfoTemplateFields(t *testing.T) {
	hashDir := filepath.Join("/cache", testContentHash)
	tests := []struct {
		name     string
		tplName  string
		chain    []*config.Template
		slug     string
		wantName string
		wantHash string
	}{
		{name: "local template", tplName: "web-dev", chain: []*config.Template{{Name: "web-dev", Path: "/t/web-dev"}}, wantName: "web-dev"},
		{name: "local template ignores slug", tplName: "web-dev", chain: []*config.Template{{Name: "web-dev", Path: "/t/web-dev"}}, slug: "other", wantName: "web-dev"},
		{name: "hash dir with slug", tplName: hashDir, chain: []*config.Template{{Name: "", Path: hashDir}}, slug: "web-dev", wantName: "web-dev", wantHash: testContentHash},
		{name: "hash dir without slug", tplName: hashDir, chain: []*config.Template{{Name: "", Path: hashDir}}, wantHash: testContentHash},
		{name: "hash-named chain entry", tplName: hashDir, chain: []*config.Template{{Name: testContentHash, Path: hashDir}}, slug: "web-dev", wantName: "web-dev", wantHash: testContentHash},
		{name: "hash dir, no chain", tplName: hashDir, slug: "web-dev", wantName: "web-dev", wantHash: testContentHash},
		{name: "hash slug never used", tplName: hashDir, chain: []*config.Template{{Path: hashDir}}, slug: testContentHash, wantHash: testContentHash},
		{name: "no chain, plain name", tplName: "web-dev", wantName: "web-dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.slug != "" {
				ctx = api.ContextWithTemplateName(ctx, tt.slug)
			}
			gotName, gotHash := infoTemplateFields(ctx, tt.tplName, tt.chain)
			if gotName != tt.wantName || gotHash != tt.wantHash {
				t.Errorf("infoTemplateFields() = (%q, %q), want (%q, %q)", gotName, gotHash, tt.wantName, tt.wantHash)
			}
		})
	}
}

// TestAgentTemplateDisplayName covers the value used for both the
// scion.template label and SCION_TEMPLATE_NAME: never a content hash.
func TestAgentTemplateDisplayName(t *testing.T) {
	info := func(tpl string) *api.ScionConfig { return &api.ScionConfig{Info: &api.AgentInfo{Template: tpl}} }
	tests := []struct {
		name string
		slug string
		cfg  *api.ScionConfig
		want string
	}{
		{name: "slug wins", slug: "web-dev", cfg: info("other"), want: "web-dev"},
		{name: "info name", cfg: info("web-dev"), want: "web-dev"},
		{name: "hash info, no slug", cfg: info(testContentHash), want: ""},
		{name: "hash info, slug", slug: "web-dev", cfg: info(testContentHash), want: "web-dev"},
		{name: "hash slug falls back to info", slug: testContentHash, cfg: info("web-dev"), want: "web-dev"},
		{name: "hash slug and hash info", slug: testContentHash, cfg: info(testContentHash), want: ""},
		{name: "nil config", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentTemplateDisplayName(tt.slug, tt.cfg); got != tt.want {
				t.Errorf("agentTemplateDisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStartHashTemplateLabelAndEnv starts an agent from a template in a
// content-hash cache directory and then starts it again without the template
// path, as a broker start or restart does. The scion.template label and
// SCION_TEMPLATE_NAME never carry the hash, and the second start does not
// pick up a same-named local template's bundled harness-config.
func TestStartHashTemplateLabelAndEnv(t *testing.T) {
	for _, tt := range []struct {
		name, slug, want string
	}{
		{name: "slug", slug: "web-dev", want: "web-dev"},
		{name: "no slug", slug: "", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			t.Cleanup(func() { _ = os.Chdir(oldWd) })
			t.Setenv("HOME", tmpDir)

			writeHC := func(dir, user string) {
				_ = os.MkdirAll(filepath.Join(dir, "home"), 0o755)
				extra := ""
				if user == "localuser" {
					// Identifies this harness-config when the harness is
					// resolved from it (NoAuth drop-to-shell message).
					extra = "no_auth:\n  behavior: drop-to-shell\n  message: from-local-template\n"
				}
				_ = os.WriteFile(filepath.Join(dir, "config.yaml"),
					[]byte("harness: claude\nuser: "+user+"\nimage: scion-claude:latest\n"+extra), 0o644)
			}
			globalScionDir := filepath.Join(tmpDir, ".scion")
			writeHC(filepath.Join(globalScionDir, "harness-configs", "claude-web"), "globaluser")
			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0o644)
			// A local template with the slug's name, bundling a different
			// harness-config of the same name.
			localTpl := filepath.Join(globalScionDir, "templates", "web-dev")
			writeHC(filepath.Join(localTpl, "harness-configs", "claude-web"), "localuser")
			_ = os.WriteFile(filepath.Join(localTpl, "scion-agent.json"), []byte(`{"default_harness_config":"claude-web"}`), 0o644)

			hashDir := filepath.Join(tmpDir, "template-cache", testContentHash)
			writeHC(filepath.Join(hashDir, "harness-configs", "claude-web"), "scion")
			_ = os.WriteFile(filepath.Join(hashDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude-web"}`), 0o644)

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			_ = os.MkdirAll(projectScionDir, 0o755)

			var captured runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					captured = config
					return "mock-id", nil
				},
			})
			check := func(step, wantUser string) {
				t.Helper()
				if got := captured.Labels["scion.template"]; got != tt.want {
					t.Errorf("%s: scion.template label = %q, want %q", step, got, tt.want)
				}
				wantEnv := tt.want
				if wantEnv == "" {
					wantEnv = "custom"
				}
				gotEnv := ""
				for _, e := range captured.Env {
					if v, ok := strings.CutPrefix(e, "SCION_TEMPLATE_NAME="); ok {
						gotEnv = v
					}
				}
				if gotEnv != wantEnv {
					t.Errorf("%s: SCION_TEMPLATE_NAME = %q, want %q", step, gotEnv, wantEnv)
				}
				if captured.UnixUsername != wantUser {
					t.Errorf("%s: UnixUsername = %q, want %q", step, captured.UnixUsername, wantUser)
				}
			}

			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "hash-agent", Template: hashDir, TemplateName: tt.slug,
				ProjectPath: projectScionDir, NoAuth: true,
			}); err != nil {
				t.Fatalf("create Start: %v", err)
			}
			check("create", "scion")
			data, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(projectScionDir, "hash-agent"), "agent-info.json"))
			if err != nil {
				t.Fatal(err)
			}
			var info api.AgentInfo
			if err := json.Unmarshal(data, &info); err != nil {
				t.Fatal(err)
			}
			if info.Template != tt.want || info.TemplateHash != testContentHash {
				t.Errorf("agent-info.json template = %q, templateHash = %q; want %q, %q", info.Template, info.TemplateHash, tt.want, testContentHash)
			}

			captured = runtime.RunConfig{}
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "hash-agent", TemplateName: tt.slug,
				ProjectPath: projectScionDir, NoAuth: true,
			}); err != nil {
				t.Fatalf("restart Start: %v", err)
			}
			check("restart", "globaluser")

			// A start that names the template by a non-absolute name (as a
			// local start of an existing agent can) does not resolve the
			// same-named local template for the harness-config either.
			captured = runtime.RunConfig{}
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "hash-agent", Template: "web-dev", TemplateName: tt.slug,
				ProjectPath: projectScionDir, NoAuth: true,
			}); err != nil {
				t.Fatalf("named-template Start: %v", err)
			}
			if captured.UnixUsername == "localuser" || captured.NoAuthMessage == "from-local-template" {
				t.Errorf("named-template start used the local %q template's harness-config (user %q, no-auth message %q)", "web-dev", captured.UnixUsername, captured.NoAuthMessage)
			}

			// A start of an agent with no agent directory, carrying only the
			// slug, does not load the same-named local template either.
			captured = runtime.RunConfig{}
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "fresh-agent", TemplateName: tt.slug, HarnessConfig: "claude-web",
				ProjectPath: projectScionDir, NoAuth: true,
			}); err != nil {
				t.Fatalf("fresh Start: %v", err)
			}
			if captured.UnixUsername != "globaluser" {
				t.Errorf("fresh start: UnixUsername = %q, want %q (the local %q template must not be loaded by slug)", captured.UnixUsername, "globaluser", "web-dev")
			}
		})
	}
}

// TestProvisionAndReprovision_HashTemplateRecordsSlug checks that Provision
// (provision-only create) and Reprovision record the dispatch's template
// slug, not an empty name, for a template in a content-hash cache directory.
func TestProvisionAndReprovision_HashTemplateRecordsSlug(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	hashDir := filepath.Join(t.TempDir(), testContentHash)
	_ = os.MkdirAll(hashDir, 0o755)
	_ = os.WriteFile(filepath.Join(hashDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0o644)

	const agentName = "hash-provision-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	readInfo := func() api.AgentInfo {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(scionDir, agentName), "agent-info.json"))
		if err != nil {
			t.Fatal(err)
		}
		var info api.AgentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatal(err)
		}
		return info
	}

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Provision(context.Background(), api.StartOptions{
		Name: agentName, Template: hashDir, TemplateName: "web-dev",
		ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if info := readInfo(); info.Template != "web-dev" || info.TemplateHash != testContentHash {
		t.Errorf("after Provision: template = %q, templateHash = %q; want %q, %q", info.Template, info.TemplateHash, "web-dev", testContentHash)
	}

	// Reprovision requires a real clone in the workspace.
	_ = os.MkdirAll(filepath.Join(scionDir, "agents", agentName, "workspace", ".git"), 0o755)
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: hashDir, TemplateName: "web-dev-v2",
		ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	if info := readInfo(); info.Template != "web-dev-v2" || info.TemplateHash != testContentHash {
		t.Errorf("after Reprovision: template = %q, templateHash = %q; want %q, %q", info.Template, info.TemplateHash, "web-dev-v2", testContentHash)
	}
}

func TestTemplateRef(t *testing.T) {
	if got := templateRef(&config.Template{Name: "web-dev", Path: "/t/web-dev"}); got != "web-dev" {
		t.Errorf("templateRef(named) = %q, want web-dev", got)
	}
	hashDir := filepath.Join("/cache", testContentHash)
	if got := templateRef(&config.Template{Path: hashDir}); got != hashDir {
		t.Errorf("templateRef(unnamed) = %q, want %q", got, hashDir)
	}
}
