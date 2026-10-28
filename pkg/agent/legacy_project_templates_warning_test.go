/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A start whose project path is a workspace root (the broker's path for a
// hub-managed project) warns when templates are left in the root's own
// templates/ directory, which is no longer a template source. A templates/
// directory without scion-agent configs does not warn.
func TestStartWarnsAboutTemplatesAtWorkspaceRoot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		legacyFile  string // under <root>/templates; "" for none
		wantWarning bool
	}{
		{"template at the old location", "old-tpl/scion-agent.yaml", true},
		{"unrelated templates dir", "page.html", false},
		{"no templates dir", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
			mustMkdirAll(t, hcDir)
			writeTestFile(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			mustMkdirAll(t, tplDir)
			writeTestFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)
			writeTestFile(t, filepath.Join(globalScionDir, "settings.yaml"), "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

			root := filepath.Join(tmpDir, "project")
			agentDir := filepath.Join(root, ".scion", "agents", "tpl-test")
			mustMkdirAll(t, filepath.Join(agentDir, "home"))
			writeTestFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness": "generic"}`)
			if tc.legacyFile != "" {
				p := filepath.Join(root, "templates", tc.legacyFile)
				mustMkdirAll(t, filepath.Dir(p))
				writeTestFile(t, p, "image: x\n")
			}

			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					return "mock-id", nil
				},
			}
			info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:        "tpl-test",
				ProjectPath: root,
				BrokerMode:  true,
				NoAuth:      true,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			var got string
			for _, w := range info.Warnings {
				if strings.Contains(w, "are no longer used; project templates are read from") {
					got = w
				}
			}
			if (got != "") != tc.wantWarning {
				t.Fatalf("legacy templates warning = %q, want present=%t (warnings: %v)", got, tc.wantWarning, info.Warnings)
			}
			if tc.wantWarning && (!strings.Contains(got, "old-tpl") || !strings.Contains(got, filepath.Join(".scion", "templates"))) {
				t.Errorf("warning %q should name the template and the new location", got)
			}
		})
	}
}
